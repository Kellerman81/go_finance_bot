// Command optimize grid-searches strategy + exit parameters against historical
// data, ranks every combination, and prints a leaderboard plus the best config
// as ready-to-paste YAML.
//
//	go run ./cmd/optimize -generate AAPL,MSFT,NVDA -bars 500 -rank blend
//	go run ./cmd/optimize -csv data.csv -rank winrate -top 20
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Kellerman81/go_finance_bot/internal/backtest"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// main parses flags, loads config and data, and runs the grid, random or
// sensitivity search, printing the ranked results.
//
//nolint:cyclop,funlen // sequential CLI flow: flags → data → search → output
func main() {
	cfgPath := flag.String(
		"config",
		"data/config.yaml",
		"base config (fixed indicator periods, limits, costs)",
	)
	csvPath := flag.String("csv", "", "CSV file of OHLCV candles")
	gen := flag.String("generate", "", "comma-separated symbols to synthesise instead of CSV")
	bars := flag.Int("bars", 500, "synthetic bars when using -generate")
	cash := flag.Float64("cash", 50000, "starting cash")
	rank := flag.String("rank", "blend", "ranking: blend | return | winrate | sharpe")
	minTrades := flag.Int("min-trades", 5, "ignore configs with fewer trades than this")
	top := flag.Int("top", 15, "leaderboard rows to print")
	respectLimits := flag.Bool(
		"respect-limits",
		false,
		"keep config money limits (else relax so deployment isn't capped)",
	)
	random := flag.Int(
		"random",
		0,
		"random-search N samples over the full space INCL. indicator periods (0 = grid search)",
	)
	seed := flag.Int64("seed", 1, "random seed (for reproducible -random runs)")
	sensitivity := flag.Bool(
		"sensitivity",
		false,
		"one-factor-at-a-time: change each parameter alone and show the result",
	)
	deploy := flag.Float64("deploy", 0.25, "baseline capital fraction for -sensitivity")
	jsonOut := flag.Bool("json", false, "emit all ranked trials as JSON")

	flag.Parse()

	cfg := backtest.LoadConfig(*cfgPath, os.Stderr)

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
		data = backtest.Generate(splitSymbols(*gen), *bars)
	default:
		fmt.Fprintln(os.Stderr, "provide -csv <file> or -generate <symbols>")
		flag.Usage()
		os.Exit(2)
	}

	if len(data) == 0 {
		fmt.Fprintln(os.Stderr, "no candle data loaded")
		os.Exit(1)
	}

	limMode := "relaxed (exploring deployment)"
	if *respectLimits {
		limMode = "config limits respected"
	}

	if *sensitivity {
		fmt.Printf(
			"Sensitivity analysis (one parameter changed at a time, baseline deploy=%.0f%%, limits=%s)\n\n",
			*deploy*100,
			limMode,
		)

		base, axes := backtest.Sensitivity(cfg, data, *cash, *deploy, *respectLimits)
		printSensitivity(base, axes)

		return
	}

	var trials []backtest.Trial

	if *random > 0 {
		fmt.Printf(
			"Random search: %d samples over the full space incl. indicator periods (rank=%s, min-trades=%d, limits=%s, seed=%d)...\n\n",
			*random,
			*rank,
			*minTrades,
			limMode,
			*seed,
		)

		trials = backtest.OptimizeRandom(
			cfg,
			data,
			*cash,
			*random,
			*minTrades,
			backtest.ScorerByName(*rank),
			*respectLimits,
			*seed,
		)
	} else {
		grid := backtest.DefaultGrid()
		fmt.Printf("Grid search: %d combinations (rank=%s, min-trades=%d, limits=%s)...\n\n",
			grid.Combos(), *rank, *minTrades, limMode)

		trials = backtest.Optimize(
			cfg,
			data,
			grid,
			*cash,
			*minTrades,
			backtest.ScorerByName(*rank),
			*respectLimits,
		)
	}

	if len(trials) == 0 {
		fmt.Println(
			"No configurations met the minimum-trades threshold. Try -min-trades 1 or different data.",
		)

		return
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")

		if err := enc.Encode(trials); err != nil {
			fmt.Fprintln(os.Stderr, "encode json:", err)
			os.Exit(1)
		}

		return
	}

	printLeaderboard(trials, *top)
	printBest(trials[0], *cash)
}

// dedupe collapses trials that produced an identical result (e.g. exit params
// that never triggered), keeping the best-scoring representative, so the
// leaderboard shows genuinely distinct outcomes.
func dedupe(trials []backtest.Trial) []backtest.Trial {
	seen := make(map[string]bool)
	out := make([]backtest.Trial, 0, len(trials))

	for _, t := range trials {
		key := fmt.Sprintf("%.4f|%.2f|%d|%.4f|%.4f",
			t.Result.TotalReturnPct, t.Result.WinRatePct, t.Result.NumTrades,
			t.Result.MaxDrawdownPct, t.Result.Sharpe)
		if seen[key] {
			continue
		}

		seen[key] = true

		out = append(out, t)
	}

	return out
}

// printLeaderboard prints the top-ranked distinct trials as a table.
func printLeaderboard(all []backtest.Trial, top int) {
	trials := dedupe(all)
	if top > len(trials) {
		top = len(trials)
	}

	fmt.Printf(
		"Ranked %d valid configs (%d distinct outcomes). Top %d:\n\n",
		len(all),
		len(trials),
		top,
	)
	fmt.Printf("%-3s %8s %8s %7s %7s %7s %6s  %s\n",
		"#", "score", "return%", "win%", "trades", "maxDD%", "sharpe", "config")
	fmt.Println(strings.Repeat("-", 110))

	for i := range top {
		t := trials[i]
		fmt.Printf("%-3d %8.2f %8.2f %7.1f %7d %7.2f %6.2f  %s\n",
			i+1, t.Score, t.Result.TotalReturnPct, t.Result.WinRatePct, t.Result.NumTrades,
			t.Result.MaxDrawdownPct, t.Result.Sharpe, describe(t))
	}

	if len(trials) > 0 {
		fmt.Printf("\nBuy & hold benchmark: %.2f%%\n", trials[0].Result.BuyHoldReturnPct)
	}
}

// printSensitivity prints each one-factor-at-a-time sweep with the return
// change vs the baseline config.
func printSensitivity(base backtest.Result, axes []backtest.AxisSweep) {
	fmt.Printf("Baseline: return %.2f%%  win %.1f%%  trades %d  maxDD %.2f%%  sharpe %.2f\n",
		base.TotalReturnPct, base.WinRatePct, base.NumTrades, base.MaxDrawdownPct, base.Sharpe)
	fmt.Println("(Δret = change in total return vs baseline when ONLY this parameter changes)")

	for _, ax := range axes {
		fmt.Printf("\n%s:\n", ax.Axis)

		for _, r := range ax.Rows {
			mark := "  "
			if r.Baseline {
				mark = "* " // baseline value
			}

			fmt.Printf(
				"  %s%-8s  return %7.2f%%  Δret %+6.2f  win %5.1f%%  trades %3d  sharpe %5.2f\n",
				mark,
				r.Label,
				r.Result.TotalReturnPct,
				r.Result.TotalReturnPct-base.TotalReturnPct,
				r.Result.WinRatePct,
				r.Result.NumTrades,
				r.Result.Sharpe,
			)
		}
	}

	fmt.Println(
		"\n(* = baseline value; rows show that many distinct configs were tested per parameter)",
	)
}

// describe renders a trial's key parameters as a one-line leaderboard label.
func describe(t backtest.Trial) string {
	return fmt.Sprintf("[%s] %s ms=%.2f deploy=%.0f%% sl=%.2f ts=%.2f tp=%.2f",
		strings.Join(t.Detectors, "+"), t.Combine, t.MinStrength, t.CapitalFrac*100,
		t.StopLoss, t.TrailingStop, t.TakeProfit)
}

// printBest prints the winning trial as paste-ready config.yaml YAML.
func printBest(t backtest.Trial, cash float64) {
	fmt.Println("\n================ BEST CONFIG (paste into config.yaml) ================")

	p := t.Params

	fmt.Println("strategy:")
	fmt.Printf("  detectors: [%s]\n", quoteJoin(t.Detectors))
	fmt.Printf("  combine: %q\n", t.Combine)
	fmt.Printf("  min_strength: %.2f\n", t.MinStrength)
	fmt.Printf("  fast_ma: %d\n  slow_ma: %d\n", p.FastMA, p.SlowMA)
	fmt.Printf("  rsi_period: %d\n  rsi_overbought: %.0f\n  rsi_oversold: %.0f\n  rsi_mode: %q\n",
		p.RSIPeriod, p.RSIOverbought, p.RSIOversold, p.RSIMode)
	fmt.Printf(
		"  macd_fast: %d\n  macd_slow: %d\n  macd_signal: %d\n",
		p.MACDFast,
		p.MACDSlow,
		p.MACDSignal,
	)
	fmt.Printf("  bb_period: %d\n  bb_stddev: %.1f\n", p.BBPeriod, p.BBStdDev)
	fmt.Printf("  vwap_period: %d\n", p.VWAPPeriod)
	fmt.Printf("  atr_period: %d\n  atr_mult: %.1f\n", p.ATRPeriod, p.ATRMult)
	fmt.Printf("  rvol_period: %d\n  rvol_threshold: %.1f\n", p.RVOLPeriod, p.RVOLThreshold)
	fmt.Printf(
		"  vp_window: %d\n  vp_buckets: %d\n  vp_value_area_pct: %.2f\n",
		p.VPWindow,
		p.VPBuckets,
		p.VPValueAreaPct,
	)
	fmt.Printf(
		"  trend_period: %d\n  trend_threshold: %.2f\n  trend_gate: %v\n",
		p.TrendPeriod,
		p.TrendThreshold,
		p.TrendGate,
	)
	fmt.Println("engine:")
	fmt.Printf(
		"  order_size_usd: %.0f   # = %.0f%% of %.0f cash\n",
		t.OrderSize,
		t.CapitalFrac*100,
		cash,
	)
	fmt.Println("exits:")
	fmt.Println("  enabled: true")
	fmt.Printf("  stop_loss_pct: %.2f\n", t.StopLoss)
	fmt.Printf("  trailing_stop_pct: %.2f%s\n", t.TrailingStop, disabledNote(t.TrailingStop))
	fmt.Printf("  take_profit_pct: %.2f%s\n", t.TakeProfit, disabledNote(t.TakeProfit))
	fmt.Println("# To realise this, money limits must allow the deployment, e.g.:")
	fmt.Printf(
		"# limits: { max_order_value: %.0f, max_per_position: %.0f, max_total_invested: <your cap> }\n",
		t.OrderSize*1.05,
		t.OrderSize*1.05,
	)
	fmt.Printf(
		"\nResult: return %.2f%%  (buy&hold %.2f%%), win %.1f%%, %d trades, maxDD %.2f%%, sharpe %.2f\n",
		t.Result.TotalReturnPct,
		t.Result.BuyHoldReturnPct,
		t.Result.WinRatePct,
		t.Result.NumTrades,
		t.Result.MaxDrawdownPct,
		t.Result.Sharpe,
	)
}

// disabledNote annotates a zero exit level as disabled in the YAML output.
func disabledNote(v float64) string {
	if v == 0 {
		return "   # disabled"
	}

	return ""
}

// quoteJoin renders a string slice as a quoted, comma-separated YAML list.
func quoteJoin(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = fmt.Sprintf("%q", s)
	}

	return strings.Join(q, ", ")
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
