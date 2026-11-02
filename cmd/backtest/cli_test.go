package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/backtest"
	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/engine"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// mainArgsEnv carries a child process's command line (JSON) for
// TestMainProcessHelper; its presence is what makes the helper run main.
const mainArgsEnv = "GFB_BACKTEST_MAIN_ARGS"

// TestMainProcessHelper is not a test: it is main, run inside a child copy
// of the test binary by runMain. main calls os.Exit, which would end the
// test process itself, so every main path is exercised in a child.
func TestMainProcessHelper(t *testing.T) {
	raw := os.Getenv(mainArgsEnv)
	if raw == "" {
		t.Skip("helper for runMain; runs only in a child process")
	}

	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		t.Fatal(err)
	}

	os.Args = append([]string{"backtest"}, args...)
	flag.CommandLine = flag.NewFlagSet("backtest", flag.ExitOnError)

	main()
	os.Exit(0)
}

// runMain runs main with args in a child process and returns its stdout,
// stderr and exit code.
func runMain(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()

	raw, _ := json.Marshal(args)

	cmd := exec.Command(os.Args[0], "-test.run=^TestMainProcessHelper$") //nolint:gosec // the test binary itself
	cmd.Env = append(os.Environ(), mainArgsEnv+"="+string(raw))
	cmd.Dir = t.TempDir() // a run that forgot -config must not create one in the repo

	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut

	err := cmd.Run()

	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("run child: %v", err)
	}

	return out.String(), errOut.String(), code
}

// captureOut runs fn with os.Stdout redirected and returns what it wrote:
// the report printers write straight to stdout, as a terminal tool should.
func captureOut(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	orig := os.Stdout
	os.Stdout = w

	done := make(chan string)

	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	fn()

	os.Stdout = orig
	_ = w.Close()

	return <-done
}

// TestMainRunsAGeneratedBacktest guards the command end to end: a
// -generate run prints the report (or -json the full result), -save writes
// candles that a later -csv run reads back, a missing config falls back to
// defaults rather than failing, and every bad input exits non-zero with the
// reason on stderr — exit 2 for usage, 1 for a failed load — instead of a
// misleading "no trades" report.
func TestMainRunsAGeneratedBacktest(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "missing.yaml")
	csv := filepath.Join(dir, "bars.csv")

	out, errOut, code := runMain(t, "-config", cfg, "-generate", "aaa, bbb", "-bars", "300",
		"-save", csv, "-montecarlo", "50")
	if code != 0 {
		t.Fatalf("generate run exited %d: %s", code, errOut)
	}

	for _, want := range []string{"BACKTEST REPORT", "Symbols", "Bars             : 300", "Final equity"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}

	if !strings.Contains(errOut, "saved 2 symbols / 600 bars") {
		t.Errorf("-save not confirmed on stderr: %s", errOut)
	}

	out, errOut, code = runMain(t, "-config", cfg, "-csv", csv, "-json")
	if code != 0 {
		t.Fatalf("csv -json run exited %d: %s", code, errOut)
	}

	var res struct {
		Bars         int     `json:"bars"`
		StartingCash float64 `json:"starting_cash"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Bars != 300 || res.StartingCash != 100000 {
		t.Errorf("-json output = %+v (%v): %.200s", res, err, out)
	}

	for _, tc := range []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"-config", cfg}, 2, "provide -csv"},
		{[]string{"-config", cfg, "-generate", "AAA", "-cash", "0"}, 2, "-cash must be > 0"},
		{[]string{"-config", cfg, "-generate", "AAA", "-bars", "0"}, 2, "-bars must be > 0"},
		{[]string{"-config", cfg, "-csv", filepath.Join(dir, "nope.csv")}, 1, "load csv"},
		{[]string{"-config", cfg, "-generate", " , "}, 1, "no candle data"},
		{[]string{"-config", cfg, "-generate", "AAA", "-save", filepath.Join(dir, "no", "such", "x.csv")}, 1, "save csv"},
		{[]string{"-config", cfg, "-generate", "AAA", "-db", filepath.Join(dir, "missing.db")}, 1, "read -db"},
	} {
		_, errOut, code := runMain(t, tc.args...)
		if code != tc.code || !strings.Contains(errOut, tc.msg) {
			t.Errorf("%v: exit %d, stderr %q; want exit %d mentioning %q", tc.args, code, errOut, tc.code, tc.msg)
		}
	}
}

// TestParseFlagsReadsTheCommandLine guards the flag table: every option
// reaches its field, and the defaults are the documented ones (100000
// cash, 500 bars, a 0.30 ruin threshold, seed 1).
func TestParseFlagsReadsTheCommandLine(t *testing.T) {
	origArgs, origFlags := os.Args, flag.CommandLine
	t.Cleanup(func() { os.Args, flag.CommandLine = origArgs, origFlags })

	flag.CommandLine = flag.NewFlagSet("backtest", flag.ContinueOnError)
	os.Args = []string{"backtest"}

	o := parseFlags()
	if *o.cash != 100000 || *o.bars != 500 || *o.ruin != 0.30 || *o.seed != 1 || *o.cfgPath != "data/config.yaml" {
		t.Errorf("defaults: cash %v bars %v ruin %v seed %v config %q",
			*o.cash, *o.bars, *o.ruin, *o.seed, *o.cfgPath)
	}

	flag.CommandLine = flag.NewFlagSet("backtest", flag.ContinueOnError)
	os.Args = []string{
		"backtest", "-config", "c.yaml", "-csv", "d.csv", "-generate", "X", "-bars", "7", "-cash", "5",
		"-order-size", "3", "-save", "s.csv", "-montecarlo", "9", "-ruin", "0.5", "-seed", "42",
		"-json", "-db", "bot.db",
	}

	o = parseFlags()
	if *o.cfgPath != "c.yaml" || *o.csvPath != "d.csv" || *o.gen != "X" || *o.bars != 7 || *o.cash != 5 ||
		*o.orderSize != 3 || *o.save != "s.csv" || *o.montecarlo != 9 || *o.ruin != 0.5 || *o.seed != 42 ||
		!*o.jsonOut || *o.dbPath != "bot.db" {
		t.Errorf("parsed options do not match the command line: %+v", o)
	}
}

// TestSymbolStrategiesBuildsEveryOverride guards per-symbol parity with
// live: each strategy_overrides symbol must get its OWN strategy in the
// backtest, or the backtest measures the shared strategy on symbols the
// live bot trades differently. No overrides means nil, not an empty map.
func TestSymbolStrategiesBuildsEveryOverride(t *testing.T) {
	if symbolStrategies(config.Config{}) != nil {
		t.Error("no overrides did not give nil")
	}

	cfg := config.Config{SymbolStrategy: map[string]config.StrategyConfig{
		"AAA": {Detectors: []string{"rsi"}, Combine: "unanimous"},
		"BBB": {Detectors: []string{"macd"}},
	}}

	got := symbolStrategies(cfg)
	if len(got) != 2 {
		t.Fatalf("symbolStrategies = %v", got)
	}

	if name := got["AAA"].Name(); !strings.Contains(name, "rsi") || !strings.Contains(name, "unanimous") {
		t.Errorf("AAA strategy = %s, want its own rsi/unanimous set", name)
	}

	if !strings.Contains(got["BBB"].Name(), "macd") {
		t.Errorf("BBB strategy = %s", got["BBB"].Name())
	}
}

// TestSplitSymbols guards the -generate list: trimmed, upper-cased, blanks
// dropped — " aapl,,msft " is two symbols, not four.
func TestSplitSymbols(t *testing.T) {
	t.Parallel()

	if got := strings.Join(splitSymbols(" aapl,,msft , "), "|"); got != "AAPL|MSFT" {
		t.Errorf("splitSymbols = %q", got)
	}

	if splitSymbols(" , ") != nil {
		t.Error("an all-blank list produced symbols")
	}
}

// TestReportFormatters guards the report's small formatters: annualised
// suppresses a figure on a short span (a two-day CAGR of 1e17% is worse
// than nothing), short renders durations compactly, bar scales to the
// largest magnitude with a minimum of one mark for a non-zero value, and
// abs is the plain absolute value.
func TestReportFormatters(t *testing.T) {
	t.Parallel()

	if got := strings.TrimSpace(annualised(1e17, true, "%")); got != "n/a" {
		t.Errorf("short span = %q", got)
	}

	if got := strings.TrimSpace(annualised(12.345, false, "%")); got != "12.35%" {
		t.Errorf("annualised = %q", got)
	}

	for d, want := range map[time.Duration]string{
		0: "—", 1500 * time.Millisecond: "2s", 90 * time.Minute: "1.5h",
		45 * time.Minute: "45m", 72 * time.Hour: "3.0d",
	} {
		if got := short(d); got != want {
			t.Errorf("short(%v) = %q, want %q", d, got, want)
		}
	}

	if bar(5, 0) != "" || bar(10, 10) != strings.Repeat("+", 24) || bar(-5, 10) != strings.Repeat("-", 12) ||
		bar(0.001, 10) != "+" || bar(0, 10) != "" {
		t.Error("bar scaling")
	}

	if abs(-2) != 2 || abs(3) != 3 {
		t.Error("abs")
	}
}

// TestPrintReportShowsEveryBreakdown guards the human report: headline
// metrics, the exit, symbol, month and holding tables, the Monte Carlo
// block and the last trades must all print; the symbol table is capped at
// maxRows; a losers-held-longer pattern is called out; and a short span
// suppresses the annualised figures rather than printing nonsense.
func TestPrintReportShowsEveryBreakdown(t *testing.T) {
	var symbols []backtest.SymbolResult
	for i := range maxRows + 3 {
		symbols = append(symbols, backtest.SymbolResult{Symbol: "S" + string(rune('A'+i)), Trades: 1})
	}

	r := backtest.Result{
		Bars: 300, StartingCash: 1000, FinalEquity: 1100, TotalReturnPct: 10, SpanDays: 5,
		NumTrades: 2, Wins: 1, Losses: 1, Misaligned: "AAA vs BBB",
		RoundTrips: []backtest.RoundTrip{{Symbol: "SA"}},
		ByExit:     []backtest.ExitResult{{Kind: "stop-loss", Trades: 1, AvgHeld: 2 * time.Hour}},
		BySymbol:   symbols,
		ByMonth:    []backtest.PeriodReturn{{Month: "2026-09", ReturnPct: 4}, {Month: "2026-10", ReturnPct: -2}},
		Hold: backtest.HoldStats{Trades: 2, Min: time.Hour, Max: 5 * time.Hour,
			AvgWinHeld: time.Hour, AvgLossHeld: 5 * time.Hour},
		MonteCarlo: &backtest.MonteCarlo{Runs: 100, Trades: 2, ReturnP5: -3, ReturnP50: 4, RuinPct: 30},
	}

	at := time.Date(2026, 10, 1, 14, 30, 0, 0, time.UTC)
	for range 12 {
		r.Trades = append(r.Trades, engineTrade(at))
	}

	out := captureOut(t, func() {
		printReport(r, map[string][]market.Candle{"AAA": nil})
	})

	for _, want := range []string{
		"BACKTEST REPORT", "WARNING          : symbols' bars do not line up in time (AAA vs BBB)",
		"annualised figures suppressed", "P&L by exit rule", "stop-loss", "P&L by symbol",
		"... and 3 more symbols", "monthly returns", "2026-10", "holding periods",
		"losers are held far longer", "Monte Carlo (100 resamplings of 2 round trips)",
		"The 5th percentile is a loss", "Last trades:", "2026-10-01 14:30",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q", want)
		}
	}

	if n := strings.Count(out, "2026-10-01 14:30"); n != 10 {
		t.Errorf("printed %d trades, want the last 10", n)
	}

	// A run with no round trips prints no breakdowns, and one with no trades
	// stops before "Last trades".
	quiet := captureOut(t, func() {
		printReport(backtest.Result{SpanDays: 400}, map[string][]market.Candle{"AAA": nil})
	})

	if strings.Contains(quiet, "P&L by exit rule") || strings.Contains(quiet, "Last trades") ||
		strings.Contains(quiet, "suppressed") {
		t.Errorf("an empty run printed tables or suppressed a long span:\n%s", quiet)
	}

	// The tables also refuse to print from empty input on their own.
	empty := captureOut(t, func() {
		printExitTable(nil)
		printSymbolTable(nil)
		printMonthTable([]backtest.PeriodReturn{{Month: "2026-10"}})
		printHoldStats(backtest.HoldStats{})
	})
	if empty != "" {
		t.Errorf("empty tables printed %q", empty)
	}
}

// engineTrade is one filled buy at at, for the "Last trades" listing.
func engineTrade(at time.Time) engine.TradeRecord {
	return engine.TradeRecord{Time: at, Side: broker.Buy, Symbol: "AAA", Qty: 1, Price: 10, Reason: "test"}
}
