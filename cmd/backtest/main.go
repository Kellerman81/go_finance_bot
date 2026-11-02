// Command backtest replays historical candles through the bot's strategy, risk
// limits and protective exits, then prints a performance report. Data comes
// from a CSV file (-csv) or is synthesised for quick experiments (-generate).
//
//	go run ./cmd/backtest -generate AAPL,MSFT -bars 500
//	go run ./cmd/backtest -csv data/aapl.csv
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	_ "time/tzdata" // bundle the IANA tz database, same as cmd/bot — see CODE_REVIEW_FINDINGS.md's tzdata finding

	"github.com/Kellerman81/go_finance_bot/internal/backtest"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/storage"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// options are the command's parsed flags.
type options struct {
	cfgPath    *string
	csvPath    *string
	gen        *string
	bars       *int
	cash       *float64
	orderSize  *float64
	save       *string
	montecarlo *int
	ruin       *float64
	seed       *int64
	jsonOut    *bool
	dbPath     *string
}

// parseFlags declares and parses the command line.
func parseFlags() options {
	o := options{
		cfgPath: flag.String("config", "data/config.yaml",
			"config file for strategy/limits/exits"),
		csvPath: flag.String("csv", "", "CSV file of OHLCV candles"),
		gen: flag.String("generate", "",
			"comma-separated symbols to synthesise instead of CSV"),
		bars: flag.Int("bars", 500, "number of synthetic bars when using -generate"),
		cash: flag.Float64("cash", 100000, "starting cash"),
		orderSize: flag.Float64("order-size", 0,
			"per-trade notional ceiling (0 = none; the money limits size the order)"),
		save: flag.String("save", "",
			"save the loaded/generated candles to this CSV for reuse with -csv"),
		montecarlo: flag.Int("montecarlo", 0,
			"bootstrap N resamplings of the round trips for return/drawdown "+
				"confidence intervals (0 = off)"),
		ruin: flag.Float64("ruin", 0.30,
			"drawdown treated as ruin when reporting -montecarlo probabilities"),
		seed:    flag.Int64("seed", 1, "random seed (reproducible -montecarlo runs)"),
		jsonOut: flag.Bool("json", false, "emit full result (incl. trades) as JSON"),
		dbPath: flag.String("db", "",
			"bot database (a COPY is safest; opened read-only) to take the auto-learned "+
				"asset classes and sectors from, so portfolio caps bucket symbols as live does"),
	}

	flag.Parse()

	return o
}

// validateFlags rejects a non-positive -cash outright, and a non-positive
// -bars specifically when -generate is in use (bars is meaningless with
// -csv, which reads however many rows the file has).
func validateFlags(cash float64, bars int, gen string) error {
	if cash <= 0 {
		return fmt.Errorf("-cash must be > 0, got %v", cash)
	}

	if gen != "" && bars <= 0 {
		return fmt.Errorf("-bars must be > 0, got %v", bars)
	}

	return nil
}

// main parses flags, loads config and data, runs the backtest and prints the report.
//
//nolint:cyclop // sequential CLI flow: flags → data → run → output
func main() {
	opts := parseFlags()
	cfgPath, csvPath, gen := opts.cfgPath, opts.csvPath, opts.gen
	bars, cash, orderSize, save := opts.bars, opts.cash, opts.orderSize, opts.save
	montecarlo, ruin, seed, jsonOut := opts.montecarlo, opts.ruin, opts.seed, opts.jsonOut

	// A non-positive -cash or -bars doesn't crash — it flows straight into
	// the run and produces a misleading "strategy made no trades" report
	// (or a Generate call that returns symbols mapped to empty candle
	// slices, which len(data) == 0 below does not catch, since the map
	// itself is non-empty) instead of an invalid-input error naming the
	// actual problem. See CODE_REVIEW_FINDINGS.md's "-cash 0 and -bars too
	// low don't crash but produce a misleading report".
	if err := validateFlags(*cash, *bars, *gen); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	cfg := backtest.LoadConfig(*cfgPath, os.Stderr)
	backtest.ConfigureAI(cfg, os.Stderr)

	var (
		err  error
		data map[string][]market.Candle
	)

	switch {
	case *csvPath != "":
		data, err = backtest.LoadCSV(*csvPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "load csv:", err)
			os.Exit(1)
		}

	case *gen != "":
		syms := splitSymbols(*gen)

		data = backtest.Generate(syms, *bars)

	default:
		fmt.Fprintln(os.Stderr, "provide -csv <file> or -generate <symbols>")
		flag.Usage()
		os.Exit(2)
	}

	if len(data) == 0 {
		fmt.Fprintln(os.Stderr, "no candle data loaded")
		os.Exit(1)
	}

	if *save != "" {
		if err := backtest.SaveCSV(*save, data); err != nil {
			fmt.Fprintln(os.Stderr, "save csv:", err)
			os.Exit(1)
		}

		var rows int

		for _, c := range data {
			rows += len(c)
		}

		fmt.Fprintf(os.Stderr, "saved %d symbols / %d bars to %s (reuse with -csv %s)\n",
			len(data), rows, *save, *save)
	}

	// 0 means "no extra per-trade ceiling": sizing comes from the money
	// limits and the risk scaling, exactly as it does live. -order-size is
	// now a cap to search or probe under, not the target it used to be.
	size := *orderSize

	if *opts.dbPath != "" {
		sectors, classes, err := storage.ReadLearnedLabels(*opts.dbPath)
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "read -db:", err)

			os.Exit(1)
		}

		cfg.Sectors = layerLabels(sectors, cfg.Sectors)
		cfg.AssetClasses = layerLabels(classes, cfg.AssetClasses)
	}

	for _, w := range unbucketedWarnings(cfg, data) {
		_, _ = fmt.Fprintln(os.Stderr, "warning:", w)
	}

	res := backtesterFor(cfg, *cash, size).Run(data)

	if *montecarlo > 0 {
		// Closed trips, not tranches: a scale-out ladder's rungs are one trade,
		// not independent draws (BT3-2).
		res.MonteCarlo = backtest.Bootstrap(res.Trips, *cash, *montecarlo, *ruin, *seed)
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")

		if err := enc.Encode(res); err != nil {
			fmt.Fprintln(os.Stderr, "encode json:", err)
			os.Exit(1)
		}

		return
	}

	printReport(res, data)
}

// backtesterFor builds the backtester from config, mirroring the settings the
// live engine trades with so the two describe the same bot.
func backtesterFor(cfg config.Config, cash, orderSize float64) *backtest.Backtester {
	return &backtest.Backtester{
		Strategy:         strategy.New(cfg.Strategy),
		SymbolStrategies: symbolStrategies(cfg),
		Limits:           cfg.Limits,
		Exits:            cfg.Exits,
		Costs:            cfg.Costs,
		Cash:             cash,
		OrderSize:        orderSize,
		AllowPyramiding:  cfg.Engine.AllowPyramiding,
		ScaleIn:          cfg.Engine.ScaleIn,
		ConfirmBuy:       cfg.Engine.ConfirmBuy,
		ConfirmSell:      cfg.Engine.ConfirmSell,
		KeepInterval:     cfg.Engine.KeepInterval,
		MaxRiskSizeUSD:   cfg.Engine.MaxRiskSizeUSD,
		MinOrderSizeUSD:  cfg.Engine.MinOrderSizeUSD,
		MinPositions:     cfg.Engine.MinPositions,
		Portfolio:        cfg.Portfolio,
		Sectors:          cfg.Sectors,
		AssetClasses:     cfg.AssetClasses,
		Protections:      cfg.Protections,
	}
}

// symbolStrategies builds a strategy per symbol that has parameter overrides, so
// the backtest measures the same per-symbol behaviour the live engine will run.
func symbolStrategies(cfg config.Config) map[string]strategy.Strategy {
	if len(cfg.SymbolStrategy) == 0 {
		return nil
	}

	out := make(map[string]strategy.Strategy, len(cfg.SymbolStrategy))
	for sym, sc := range cfg.SymbolStrategy {
		out[sym] = strategy.New(sc)
	}

	fmt.Fprintf(os.Stderr, "per-symbol strategy overrides: %d symbols\n", len(out))

	return out
}

// printReport prints the human-readable metrics summary to stdout.
func printReport(r backtest.Result, data map[string][]market.Candle) {
	syms := make([]string, 0, len(data))
	for s := range data {
		syms = append(syms, s)
	}

	fmt.Println("================ BACKTEST REPORT ================")
	fmt.Printf("Symbols          : %s\n", strings.Join(syms, ", "))
	fmt.Printf("Bars             : %d\n", r.Bars)

	if r.Misaligned != "" {
		fmt.Printf("WARNING          : symbols' bars do not line up in time (%s)\n", r.Misaligned)
	}

	fmt.Printf("Starting cash    : %12.2f\n", r.StartingCash)
	fmt.Printf("Final equity     : %12.2f\n", r.FinalEquity)

	// Annualising a short run scales its noise up with it — compounding a 25%
	// gain over two days gives a CAGR of 1e17%. The arithmetic is right and the
	// number is worse than useless, so short spans print no annualised figure at
	// all rather than a misleading one.
	const minMeaningfulSpanDays = 30

	short := r.SpanDays > 0 && r.SpanDays < minMeaningfulSpanDays

	fmt.Printf("Total return     : %11.2f%%\n", r.TotalReturnPct)
	fmt.Printf("CAGR             : %s\n", annualised(r.CAGRPct, short, "%"))
	fmt.Printf("Buy & hold       : %11.2f%%  (equal-weighted benchmark)\n", r.BuyHoldReturnPct)
	fmt.Printf("Max drawdown     : %11.2f%%\n", r.MaxDrawdownPct)
	fmt.Printf("Exposure         : %11.2f%%  (share of time holding)\n", r.ExposurePct)

	label := "---- risk-adjusted (annualised) ----"
	if short {
		label = fmt.Sprintf(
			"---- risk-adjusted (span is only %.1f days — annualised figures suppressed) ----",
			r.SpanDays)
	}

	fmt.Println(label)
	fmt.Printf("Sharpe           : %s\n", annualised(r.Sharpe, short, ""))
	fmt.Printf("Sortino          : %s  (downside deviation only)\n",
		annualised(r.Sortino, short, ""))
	fmt.Printf("Calmar           : %s  (CAGR / max drawdown)\n", annualised(r.Calmar, short, ""))
	fmt.Println("---- trade quality ----")
	fmt.Printf("Trades           : %12d  (%d wins / %d losses)\n", r.NumTrades, r.Wins, r.Losses)
	fmt.Printf("Win rate         : %11.2f%%\n", r.WinRatePct)
	fmt.Printf("Profit factor    : %12.2f  (gross profit / gross loss)\n", r.ProfitFactor)
	fmt.Printf("Expectancy       : %11.2f%%  per round trip\n", r.ExpectancyPct)
	fmt.Printf("Avg win / loss   : %10.2f%% / %.2f%%\n", r.AvgWinPct, r.AvgLossPct)
	fmt.Println("================================================")

	printBreakdowns(r)

	if r.MonteCarlo != nil {
		printMonteCarlo(*r.MonteCarlo)
	}

	if r.NumTrades == 0 {
		return
	}

	fmt.Println("Last trades:")

	start := 0
	if len(r.Trades) > 10 {
		start = len(r.Trades) - 10
	}

	for _, t := range r.Trades[start:] {
		fmt.Printf("  %s  %-4s %-6s qty %.4f @ %.2f  (%s)\n",
			t.Time.Format("2006-01-02 15:04"), t.Side, t.Symbol, t.Qty, t.Price, t.Reason)
	}
}

// splitSymbols parses a comma-separated symbol list, trimmed and upper-cased.
func splitSymbols(s string) []string {
	var out []string

	for _, p := range strings.Split(s, ",") {
		if p = strings.ToUpper(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}

	return out
}

// layerLabels puts the hand-written map over the learned one — config wins,
// the same order the live engine layers them in.
func layerLabels(learned, hand map[string]string) map[string]string {
	out := make(map[string]string, len(learned)+len(hand))
	for k, v := range learned {
		out[strings.ToUpper(strings.TrimSpace(k))] = v
	}

	for k, v := range hand {
		out[strings.ToUpper(strings.TrimSpace(k))] = v
	}

	return out
}

// unbucketedWarnings names the backtested symbols a portfolio cap cannot see.
// Live layers the auto-learned classes and sectors under config.yaml's maps;
// this command has only the hand-written ones unless -db is given, so with
// classes left to the classifier every symbol fell in "unclassified" and ran
// uncapped and unslotted — sizing and exposure that live never allows, with
// nothing saying so (CM2-4).
func unbucketedWarnings(cfg config.Config, data map[string][]market.Candle) []string {
	if !cfg.Portfolio.Enabled {
		return nil
	}

	var out []string

	classCapped := len(cfg.Portfolio.MaxPctPerAssetClass) > 0 ||
		len(cfg.Portfolio.MinPositionsPerAssetClass) > 0
	if missing := unlabelled(data, cfg.AssetClasses); classCapped && len(missing) > 0 {
		out = append(
			out,
			fmt.Sprintf("asset-class caps are on but %d symbol(s) have no class here (%s) "+
				"— they count as unclassified, which live would not; pass -db with a copy of the bot "+
				"database or list them under asset_classes:", len(missing), strings.Join(missing, ", ")),
		)
	}

	sectorCapped := cfg.Portfolio.MaxPerSector > 0 || cfg.Portfolio.MaxPositionsPerSector > 0
	if missing := unlabelled(data, cfg.Sectors); sectorCapped && len(missing) > 0 {
		out = append(
			out,
			fmt.Sprintf("sector caps are on but %d symbol(s) have no sector here (%s) "+
				"— pass -db or list them under sectors:", len(missing), strings.Join(missing, ", ")),
		)
	}

	return out
}

// unlabelled lists, sorted, the symbols of data that labels has no entry for.
func unlabelled(data map[string][]market.Candle, labels map[string]string) []string {
	var out []string

	for sym := range data {
		if _, ok := labels[strings.ToUpper(strings.TrimSpace(sym))]; !ok {
			out = append(out, sym)
		}
	}

	slices.Sort(out)

	return out
}
