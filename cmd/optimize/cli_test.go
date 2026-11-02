package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/backtest"
)

// mainArgsEnv carries a child process's command line (JSON) for
// TestMainProcessHelper; its presence is what makes the helper run main.
const mainArgsEnv = "GFB_OPTIMIZE_MAIN_ARGS"

// TestMainProcessHelper is not a test: it is main, run inside a child copy
// of the test binary by runMain, because main calls os.Exit.
func TestMainProcessHelper(t *testing.T) {
	raw := os.Getenv(mainArgsEnv)
	if raw == "" {
		t.Skip("helper for runMain; runs only in a child process")
	}

	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		t.Fatal(err)
	}

	os.Args = append([]string{"optimize"}, args...)
	flag.CommandLine = flag.NewFlagSet("optimize", flag.ExitOnError)

	main()
	os.Exit(0)
}

// runMain runs main with args in a child process, in dir, and returns its
// stdout, stderr and exit code.
func runMain(t *testing.T, dir string, args ...string) (stdout, stderr string, code int) {
	t.Helper()

	raw, _ := json.Marshal(args)

	cmd := exec.Command(os.Args[0], "-test.run=^TestMainProcessHelper$") //nolint:gosec // the test binary itself
	cmd.Env = append(os.Environ(), mainArgsEnv+"="+string(raw))
	cmd.Dir = dir

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

// TestMainRunsEveryMode guards the command end to end in each of its modes:
// a grid search and a random search print the leaderboard, refinement and
// paste-ready block and write the checkpoint; -json emits the same
// searchOutput the checkpoint holds; -walkforward and -sensitivity print
// their own reports; an unknown -rank warns that blend is used instead
// rather than silently substituting it. Every run gets its own -checkpoint
// in a temp dir — the default writes optimize_results.json into the
// working directory.
func TestMainRunsEveryMode(t *testing.T) {
	if testing.Short() {
		t.Skip("runs real searches")
	}

	dir := t.TempDir()
	cfg := filepath.Join(dir, "missing.yaml")
	ckpt := filepath.Join(dir, "ckpt.json")
	// 120 bars is enough for every mode to rank something, and keeps the grid
	// and walk-forward runs cheap under -race (300 bars took ~50s there).
	data := []string{"-config", cfg, "-generate", "AAA", "-bars", "120", "-min-trades", "0"}

	with := func(extra ...string) []string { return append(append([]string{}, data...), extra...) }

	out, errOut, code := runMain(t, dir, with("-refine=false", "-checkpoint", ckpt, "-top", "3")...)
	if code != 0 {
		t.Fatalf("grid search exited %d: %s", code, errOut)
	}

	for _, want := range []string{"Grid search:", "Ranked", "Top 3:", "REFINEMENT", "BEST CONFIG"} {
		if !strings.Contains(out, want) {
			t.Errorf("grid output lacks %q", want)
		}
	}

	var saved searchOutput

	raw, err := os.ReadFile(ckpt)
	if err != nil || json.Unmarshal(raw, &saved) != nil || len(saved.Trials) == 0 {
		t.Errorf("checkpoint not written: %v (%d bytes)", err, len(raw))
	}

	out, errOut, code = runMain(t, dir, with("-random", "4", "-rank", "nonsense", "-checkpoint", ckpt)...)
	if code != 0 || !strings.Contains(out, "Random search: 4 samples") {
		t.Errorf("random search exited %d:\n%s\n%s", code, out, errOut)
	}

	if !strings.Contains(errOut, `unknown -rank "nonsense"`) {
		t.Errorf("an unknown -rank was not warned about: %s", errOut)
	}

	out, _, code = runMain(t, dir, with("-random", "4", "-generations", "2", "-json", "-checkpoint", ckpt)...)

	jsonStart := strings.Index(out, "{")
	if code != 0 || jsonStart < 0 || json.Unmarshal([]byte(out[jsonStart:]), &saved) != nil || len(saved.Warnings) == 0 {
		t.Errorf("-json output is not a searchOutput (exit %d): %.300s", code, out)
	}

	out, errOut, code = runMain(t, dir, with("-walkforward", "2")...)
	if code != 0 || !strings.Contains(out, "Walk-forward analysis: 2 folds") {
		t.Errorf("walk-forward exited %d:\n%.400s\n%s", code, out, errOut)
	}

	out, errOut, code = runMain(t, dir, "-config", cfg, "-generate", "AAA", "-bars", "120", "-sensitivity")
	if code != 0 || !strings.Contains(out, "Sensitivity analysis") || !strings.Contains(out, "Baseline: return") {
		t.Errorf("sensitivity exited %d:\n%.400s\n%s", code, out, errOut)
	}

	// Walk-forward and sensitivity write no checkpoint (and refuse an
	// explicit -checkpoint), so the temp working directory must not have
	// gained a default one either.
	if _, err := os.Stat(filepath.Join(dir, "optimize_results.json")); err == nil {
		t.Error("a run wrote the default optimize_results.json despite -checkpoint")
	}
}

// TestMainRefusesBadInput guards the usage errors: no data source and a
// flag the mode ignores exit 2, an unreadable CSV or a symbol list with no
// symbols exits 1 — each with the reason on stderr, before any search time
// is spent.
func TestMainRefusesBadInput(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "missing.yaml")

	for _, tc := range []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"-config", cfg}, 2, "provide -csv"},
		{[]string{"-config", cfg, "-generate", "AAA", "-sensitivity", "-checkpoint", "x.json"}, 2, "-checkpoint has no effect with -sensitivity"},
		{[]string{"-config", cfg, "-csv", filepath.Join(dir, "nope.csv")}, 1, "load csv"},
		{[]string{"-config", cfg, "-generate", " , "}, 1, "no candle data"},
	} {
		_, errOut, code := runMain(t, dir, tc.args...)
		if code != tc.code || !strings.Contains(errOut, tc.msg) {
			t.Errorf("%v: exit %d, stderr %q; want exit %d mentioning %q", tc.args, code, errOut, tc.code, tc.msg)
		}
	}
}

// trial is a ranked search result with the given return, score and trade
// count, for the printer and dedupe tests.
func trial(ret, score float64, trades int) backtest.Trial {
	return backtest.Trial{
		Detectors: []string{"ema_cross", "rsi"}, Combine: "majority", MinStrength: 0.25,
		CapitalFrac: 0.5, StopLoss: 0.05, TrailingStop: 0.08, TakeProfit: 0.2, Score: score,
		Result: backtest.Result{TotalReturnPct: ret, NumTrades: trades, BuyHoldReturnPct: 1.5},
	}
}

// TestDedupeKeepsTheFirstOfEachOutcome guards the leaderboard: trials with
// an identical result (exit parameters that never triggered) collapse to
// the first — the best-scoring, since the input is ranked — so the top N
// rows are N genuinely different outcomes.
func TestDedupeKeepsTheFirstOfEachOutcome(t *testing.T) {
	t.Parallel()

	a, b, c := trial(5, 3, 4), trial(5, 2, 4), trial(1, 1, 2)
	b.StopLoss = 0.1

	got := dedupe([]backtest.Trial{a, b, c})
	if len(got) != 2 || got[0].Score != 3 || got[1].Score != 1 {
		t.Errorf("dedupe = %+v", got)
	}
}

// TestDescribeNamesTheKeyParameters guards the one-line label every table
// prints: a reader must be able to tell two configs apart from it.
func TestDescribeNamesTheKeyParameters(t *testing.T) {
	t.Parallel()

	want := "[ema_cross+rsi] majority ms=0.25 deploy=50% sl=0.05 ts=0.08 tp=0.20"
	if got := describe(trial(1, 1, 1)); got != want {
		t.Errorf("describe = %q, want %q", got, want)
	}
}

// TestSplitSymbols guards the -generate list: trimmed, upper-cased, blanks
// dropped.
func TestSplitSymbols(t *testing.T) {
	t.Parallel()

	if got := strings.Join(splitSymbols(" aapl,,msft "), "|"); got != "AAPL|MSFT" {
		t.Errorf("splitSymbols = %q", got)
	}

	if splitSymbols(",") != nil {
		t.Error("an all-blank list produced symbols")
	}
}

// TestReportPrinters guards the human output of each mode: the leaderboard
// dedupes, caps at -top (never past the list) and shows the buy & hold
// benchmark; the refinement table reports a change or says nothing
// improved; walk-forward lists each fold and calls out a negative
// out-of-sample mean; sensitivity marks the baseline row and shows each
// row's change against it; and emitJSON writes indented JSON.
//
// Not parallel: captureStdout swaps os.Stdout.
func TestReportPrinters(t *testing.T) {
	trials := []backtest.Trial{trial(5, 3, 4), trial(5, 2, 4), trial(1, 1, 2)}

	out := captureStdout(t, func() { printLeaderboard(trials, 10) })
	if !strings.Contains(out, "Ranked 3 valid configs (2 distinct outcomes). Top 2:") ||
		!strings.Contains(out, "Buy & hold benchmark: 1.50%") {
		t.Errorf("leaderboard:\n%s", out)
	}

	out = captureStdout(t, func() { printRefinements(nil, trials[0], trials[0]) })
	if !strings.Contains(out, "No axis improved") {
		t.Errorf("empty refinement:\n%s", out)
	}

	after := trial(7, 4, 6)
	out = captureStdout(t, func() {
		printRefinements([]backtest.Refinement{{Axis: "confirm_buy", From: "1", To: "2", Improved: 0.5}},
			trials[0], after)
	})

	if !strings.Contains(out, "confirm_buy") || !strings.Contains(out, "+0.500") ||
		!strings.Contains(out, "Score 3.000 -> 4.000") || !strings.Contains(out, "trades 4 -> 6") {
		t.Errorf("refinement table:\n%s", out)
	}

	out = captureStdout(t, func() { printWalkForward(backtest.WalkForward{}) })
	if !strings.Contains(out, "Not enough data") {
		t.Errorf("empty walk-forward:\n%s", out)
	}

	wf := backtest.WalkForward{
		Folds:         []backtest.WFFold{{TrainBars: 200, TestBars: 50, Best: trials[0], TestScore: -1}},
		MeanOOSReturn: -2, MeanOOSScore: -1,
	}

	out = captureStdout(t, func() { printWalkForward(wf) })
	if !strings.Contains(out, "Mean out-of-sample return: -2.00%") ||
		!strings.Contains(out, "did not generalise") || !strings.Contains(out, describe(trials[0])) {
		t.Errorf("walk-forward:\n%s", out)
	}

	base := backtest.Result{TotalReturnPct: 2}
	axes := []backtest.AxisSweep{{Axis: "stop_loss_pct", Rows: []backtest.SweepRow{
		{Label: "0.05", Baseline: true, Result: base},
		{Label: "0.10", Result: backtest.Result{TotalReturnPct: 5}},
	}}}

	out = captureStdout(t, func() { printSensitivity(base, axes) })
	if !strings.Contains(out, "stop_loss_pct:") || !strings.Contains(out, "* 0.05") ||
		!strings.Contains(out, "Δret  +3.00") {
		t.Errorf("sensitivity:\n%s", out)
	}

	out = captureStdout(t, func() { emitJSON(map[string]int{"a": 1}) })
	if out != "{\n  \"a\": 1\n}\n" {
		t.Errorf("emitJSON = %q", out)
	}
}
