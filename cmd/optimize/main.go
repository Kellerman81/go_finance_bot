// Command optimize grid-searches strategy + exit parameters against historical
// data, ranks every combination, and prints a leaderboard plus the best config
// as ready-to-paste YAML.
//
//	go run ./cmd/optimize -generate AAPL,MSFT,NVDA -bars 500 -rank blend
//	go run ./cmd/optimize -csv data.csv -rank winrate -top 20
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	_ "time/tzdata" // bundle the IANA tz database, same as cmd/bot — see CODE_REVIEW_FINDINGS.md's tzdata finding

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
	rank := flag.String(
		"rank",
		"blend",
		"ranking objective: "+strings.Join(backtest.ScorerNames, " | "),
	)
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
	generations := flag.Int(
		"generations",
		1,
		"with -random N: run N backtests as G guided generations, each refining the best of the last (1 = plain random search)",
	)
	sensitivity := flag.Bool(
		"sensitivity",
		false,
		"one-factor-at-a-time: change each parameter alone and show the result",
	)
	deploy := flag.Float64("deploy", 0.25, "baseline capital fraction for -sensitivity")
	walkForward := flag.Int(
		"walkforward",
		0,
		"walk-forward folds: optimise on past data, score on the NEXT unseen segment (0 = off)",
	)
	refine := flag.Bool(
		"refine",
		true,
		"after the main search, sweep the axes the grid cannot afford to cross: the "+
			"SELL side (detectors_sell/combine_sell/min_strength_sell), confirm/keep "+
			"behaviour, ATR-adaptive exits and risk sizing",
	)
	jsonOut := flag.Bool("json", false, "emit all ranked trials as JSON")
	checkpoint := flag.String(
		"checkpoint",
		"optimize_results.json",
		"write ranked trials + best config here as JSON once the search finishes or is "+
			"interrupted (Ctrl+C), before the human-readable report — so a multi-hour run "+
			"loses nothing already computed to an interrupt or a broken stdout pipe; "+
			"empty disables",
	)

	flag.Parse()

	if conflicts := flagConflicts(activeFlags(flag.CommandLine)); len(conflicts) > 0 {
		for _, c := range conflicts {
			_, _ = fmt.Fprintln(os.Stderr, "error:", c)
		}

		os.Exit(2)
	}

	// A typo in -rank used to fall back to the blend scorer silently — the
	// run's own banner kept printing the (wrong, never-actually-used)
	// requested name for the rest of the run, with nothing anywhere saying
	// blend was substituted. Warn once, up front, so the mismatch is
	// visible before a multi-hour search runs on the wrong objective. See
	// CODE_REVIEW_FINDINGS.md's "-rank typos silently fall back to the
	// blend scorer" finding.
	score, rankOK := backtest.ScorerByName(*rank)
	if !rankOK {
		fmt.Fprintf(os.Stderr,
			"warning: unknown -rank %q; using \"blend\" instead. Valid values: %s\n",
			*rank, strings.Join(backtest.ScorerNames, ", "))
	}

	// A single Ctrl+C requests a graceful stop: the search functions check
	// ctx between trials/combinations/folds and return whatever they have
	// found so far instead of running to completion, and whatever they
	// found is exactly what still gets checkpointed and printed below — an
	// interrupted multi-hour run no longer loses everything it already
	// computed. See CODE_REVIEW_FINDINGS.md's "cmd/optimize: no
	// cancellation, no incremental result persistence".
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// After the first Ctrl+C, give SIGINT back its default meaning. While
	// NotifyContext stays registered a second Ctrl+C is swallowed, and the
	// only way out of a run still finishing its search was SIGKILL — which
	// loses the very result the checkpoint is there to save (CM2-6).
	go func() {
		<-ctx.Done()
		stop()
	}()

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

	if *walkForward > 0 {
		fmt.Printf(
			"Walk-forward analysis: %d folds (rank=%s, min-trades=%d, limits=%s)...\n\n",
			*walkForward, *rank, *minTrades, limMode,
		)

		wf := backtest.WalkForwardOptimize(ctx, cfg, data, backtest.DefaultGrid(), *cash,
			*minTrades, *walkForward, score, *respectLimits)

		if *jsonOut {
			emitJSON(wf)

			return
		}

		printWalkForward(wf)

		return
	}

	var trials []backtest.Trial

	if *random > 0 {
		mode := "Random search"
		if *generations > 1 {
			mode = fmt.Sprintf("Guided search (%d generations)", *generations)
		}

		fmt.Printf(
			"%s: %d samples over the full space incl. indicator periods (rank=%s, min-trades=%d, limits=%s, seed=%d)...\n\n",
			mode,
			*random,
			*rank,
			*minTrades,
			limMode,
			*seed,
		)

		trials = backtest.OptimizeGuided(
			ctx,
			cfg,
			data,
			*cash,
			*random,
			*generations,
			*minTrades,
			score,
			*respectLimits,
			*seed,
			// No pins: this CLI applies whatever it finds by hand to the shared
			// config, where every deployment axis is settable.
			backtest.Pins{},
		)
	} else {
		grid := backtest.DefaultGrid()
		fmt.Printf("Grid search: %d combinations (rank=%s, min-trades=%d, limits=%s)...\n\n",
			grid.Combos(), *rank, *minTrades, limMode)

		trials = backtest.Optimize(
			ctx,
			cfg,
			data,
			grid,
			*cash,
			*minTrades,
			score,
			*respectLimits,
		)
	}

	if len(trials) == 0 {
		fmt.Println(
			"No configurations met the minimum-trades threshold. Try -min-trades 1 or different data.",
		)

		return
	}

	best := trials[0]

	var changes []backtest.Refinement

	// The un-refined result goes to disk BEFORE Refine, which takes no
	// context and can run a long sweep: an interrupt during it used to lose
	// everything, contradicting the promise that the checkpoint captures the
	// un-refined winner (CM2-6). It is rewritten with the refinement after.
	if *checkpoint != "" && *refine {
		pre := searchOutput{trials, best, nil, []string{confirmCountWarning}}
		if err := writeCheckpoint(*checkpoint, pre); err != nil {
			_, _ = fmt.Fprintf(
				os.Stderr,
				"warning: could not write checkpoint %s: %v\n",
				*checkpoint,
				err,
			)
		}
	}

	if *refine {
		best, changes = refineUnlessCancelled(ctx, func() (backtest.Trial, []backtest.Refinement) {
			_, _ = fmt.Fprintln(
				os.Stderr,
				"\nRefining the winner over the axes the grid does not cover...",
			)

			return backtest.Refine(cfg, data, best, *cash, *minTrades,
				score, *respectLimits, os.Stderr,
				false) // every axis: this CLI's output is applied to the shared config by hand
		}, best)
	}

	out := searchOutput{trials, best, changes, []string{confirmCountWarning}}

	// Written before any human-readable printing: if stdout itself then
	// fails partway (a closed pipe, a full disk on a redirect) the already-
	// computed result set survives on disk regardless, rather than being
	// lost along with the print. See CODE_REVIEW_FINDINGS.md's "cmd/optimize:
	// no cancellation, no incremental result persistence".
	if *checkpoint != "" {
		if err := writeCheckpoint(*checkpoint, out); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not write checkpoint %s: %v\n", *checkpoint, err)
		}
	}

	if *jsonOut {
		emitJSON(out)

		return
	}

	printLeaderboard(trials, *top)
	printRefinements(changes, trials[0], best)
	printBest(best, *cash, cfg.Engine.MaxRiskSizeUSD)
}

// printRefinements reports what the refinement pass changed, and what it left
// alone. "Left alone" is the useful half: it means the grid's choice survived a
// direct challenge on that axis.
func printRefinements(changes []backtest.Refinement, before, after backtest.Trial) {
	fmt.Println("\n================ REFINEMENT ================")

	if len(changes) == 0 {
		fmt.Println("No axis improved on the grid winner — the sell side, behaviour," +
			" adaptive exits, risk sizing and the trend gate were all left at the base settings.")

		return
	}

	fmt.Printf("%-20s %-24s %-24s %s\n", "axis", "from", "to", "score gain")
	fmt.Println(strings.Repeat("-", 86))

	for _, c := range changes {
		fmt.Printf("%-20s %-24s %-24s %+.3f\n", c.Axis, c.From, c.To, c.Improved)
	}

	fmt.Printf("\nScore %.3f -> %.3f   return %.2f%% -> %.2f%%   trades %d -> %d\n",
		before.Score, after.Score,
		before.Result.TotalReturnPct, after.Result.TotalReturnPct,
		before.Result.NumTrades, after.Result.NumTrades)
	fmt.Println("Refinement is coordinate descent: it finds a local optimum and cannot " +
		"discover parameters that only work when changed together.")
}

// emitJSON writes v as indented JSON on stdout, exiting non-zero if it can't.
func emitJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(os.Stderr, "encode json:", err)
		os.Exit(1)
	}
}

// searchOutput is the ranked result of a grid/random/guided search, shared
// between -json's stdout output and -checkpoint's on-disk one so the two
// can never drift apart.
type searchOutput struct {
	Trials      []backtest.Trial      `json:"trials"`
	Best        backtest.Trial        `json:"best"`
	Refinements []backtest.Refinement `json:"refinements"`
	// Warnings carries the caveats printBest's YAML output states as
	// comments (invisible in -json/-checkpoint output, since both skip
	// printBest entirely) — a script or a recovered checkpoint file has no
	// other way to learn them. See CODE_REVIEW_FINDINGS.md's backtest/live
	// divergence cluster, "confirm_buy/confirm_sell count bars in backtest,
	// evaluation ticks live".
	Warnings []string `json:"warnings"`
}

// writeCheckpoint writes out as indented JSON to path, creating or
// truncating it. Called once the search finishes (or is interrupted) and
// before any human-readable printing, so an already-computed result set
// survives a stdout failure or a process kill during the print — see
// CODE_REVIEW_FINDINGS.md's "cmd/optimize: no cancellation, no incremental
// result persistence".
func writeCheckpoint(path string, out searchOutput) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")

	return enc.Encode(out)
}

// printWalkForward reports each fold's out-of-sample result and the averages.
//
// This is the number to believe. An ordinary search reports the best result on
// the data it searched, which is the definition of an in-sample optimum: some
// combination always fits the noise. Walk-forward picks parameters on past data
// only and scores them on the *next*, unseen segment — so a strategy that only
// works when you already know the answer is exposed as one.
func printWalkForward(wf backtest.WalkForward) {
	if len(wf.Folds) == 0 {
		fmt.Println(
			"Not enough data to split into folds. Use more bars or fewer -walkforward folds.",
		)

		return
	}

	fmt.Printf(
		"%-5s %9s %9s %10s %9s %8s %7s  %s\n",
		"fold",
		"trainBars",
		"testBars",
		"oosReturn%",
		"oosScore",
		"win%",
		"trades",
		"in-sample pick",
	)
	fmt.Println(strings.Repeat("-", 110))

	for i, f := range wf.Folds {
		fmt.Printf("%-5d %9d %9d %10.2f %9.3f %8.1f %7d  %s\n",
			i+1, f.TrainBars, f.TestBars, f.Test.TotalReturnPct, f.TestScore,
			f.Test.WinRatePct, f.Test.NumTrades, describe(f.Best))
	}

	fmt.Printf("\nMean out-of-sample return: %.2f%%   mean score: %.3f   over %d folds\n",
		wf.MeanOOSReturn, wf.MeanOOSScore, len(wf.Folds))

	if wf.MeanOOSReturn <= 0 {
		fmt.Println("Negative out-of-sample return: the in-sample winners did not generalise. " +
			"Treat any single-run leaderboard from this dataset as overfitted.")
	}
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
func printBest(t backtest.Trial, cash, baseCap float64) {
	fmt.Println("\n================ BEST CONFIG (paste into config.yaml) ================")

	p := t.Params

	fmt.Printf("strategy:\n  detectors: [%s]\n  combine: %q\n  min_strength: %.2f\n",
		quoteJoin(t.Detectors), t.Combine, t.MinStrength)

	// The SELL side. Emitted even when it inherits, so the file states what the
	// exit rule is rather than leaving it implicit.
	if len(p.DetectorsSell) > 0 {
		fmt.Printf("  detectors_sell: [%s]\n", quoteJoin(p.DetectorsSell))
	} else {
		fmt.Println("  detectors_sell: []   # inherits the buy detectors")
	}

	fmt.Printf("  combine_sell: %q", p.CombineSell)

	if p.CombineSell == "" {
		fmt.Print("   # inherits combine")
	}

	fmt.Println()

	if p.MinStrengthSell < 0 {
		fmt.Println("  min_strength_sell: -1   # inherits min_strength")
	} else {
		fmt.Printf("  min_strength_sell: %.2f\n", p.MinStrengthSell)
	}

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
	fmt.Printf("  adx_period: %d\n  adx_threshold: %.1f\n", p.ADXPeriod, p.ADXThreshold)
	fmt.Printf(
		"  stoch_k_period: %d\n  stoch_d_period: %d\n  stoch_overbought: %.1f\n  stoch_oversold: %.1f\n",
		p.StochKPeriod,
		p.StochDPeriod,
		p.StochOverbought,
		p.StochOversold,
	)
	fmt.Printf(
		"  supertrend_period: %d\n  supertrend_mult: %.1f\n",
		p.SupertrendPeriod,
		p.SupertrendMult,
	)
	fmt.Printf("  donchian_period: %d\n", p.DonchianPeriod)
	printBestEngine(t, cash, baseCap)
	printBestExits(t)

	fmt.Println("# To realise this, money limits must allow the deployment, e.g.:")
	fmt.Printf(
		"# limits: { max_order_value: %.0f, max_per_position: %.0f, max_total_invested: <your cap> }\n",
		t.OrderSize*1.05,
		t.OrderSize*1.05,
	)
	fmt.Println("#")
	fmt.Println("# NOT searched, and not searchable:")
	fmt.Println("#   costs.*       — spread, slippage and commission are measurements of")
	fmt.Println("#                   your broker, not choices. An optimiser given them")
	fmt.Println("#                   would set them to zero and report a better strategy.")
	fmt.Println("#   protections.* — they can only cost return in-sample, so any")
	fmt.Println("#                   return-seeking search switches them off. They were")
	fmt.Println("#                   APPLIED during every backtest instead.")

	fmt.Printf(
		"\nResult: return %.2f%%  (buy&hold %.2f%%), win %.1f%%, %d trades, maxDD %.2f%%, sharpe %.2f\n",
		t.Result.TotalReturnPct,
		t.Result.BuyHoldReturnPct,
		t.Result.WinRatePct,
		t.Result.NumTrades,
		t.Result.MaxDrawdownPct,
		t.Result.Sharpe,
	)

	if t.Result.HaltedAt != "" {
		fmt.Printf("\n⚠️  A protection would have HALTED the live bot during this run:\n    %s\n"+
			"    The equity curve continues past that point; the live one would not.\n",
			t.Result.HaltedAt)
	}
}

// printBestEngine emits the engine block, including the behaviour and sizing
// settings refinement tunes. baseCap is the config's own max_risk_size_usd,
// which every trial also ran under.
func printBestEngine(t backtest.Trial, cash, baseCap float64) {
	fmt.Println("engine:")
	// max_risk_size_usd, not order_size_usd: the latter was removed in the
	// sizing rework, and config.Load's non-strict decode drops an unknown key
	// silently, so the pasted block did not reproduce the deployment it
	// reported. The trial measured OrderSize as a per-trade sizing CAP
	// (Backtester.sizingConfig), which is what max_risk_size_usd is live
	// (CM2-5).
	//
	// The cap a trial ran under is min(OrderSize, the config's own
	// max_risk_size_usd) — Backtester.sizingConfig — so printing OrderSize
	// alone handed a 500-capped trial's results a pasted 12500 cap: 25× the
	// per-trade size that was measured (CM3-3). Print what bound.
	if baseCap > 0 && baseCap < t.OrderSize {
		fmt.Printf(
			"  max_risk_size_usd: %.0f   # your config's cap; it bound every trial "+
				"(the %.0f%% deployment = %.0f was never reached)\n",
			baseCap, t.CapitalFrac*100, t.OrderSize,
		)
	} else {
		fmt.Printf(
			"  max_risk_size_usd: %.0f   # = %.0f%% of %.0f cash\n",
			t.OrderSize,
			t.CapitalFrac*100,
			cash,
		)
	}
	fmt.Printf("  confirm_buy: %d\n", t.ConfirmBuy)
	fmt.Printf("  confirm_sell: %d\n", t.ConfirmSell)
	fmt.Printf("  keep_interval: %s\n", t.KeepInterval)

	// The confirmation cadence differs between live and backtest unless the two
	// intervals match, and a tuned confirm count is worth nothing if the live
	// bot applies a different one.
	fmt.Println("  # NOTE: " + confirmCountWarning)
}

// confirmCountWarning states the unit mismatch between what confirm_buy/
// confirm_sell measure in a backtest versus live: the search tunes a bar
// count, but the live engine counts evaluation ticks against the same
// field, so the winning value found here means something else entirely once
// pasted into a running bot (e.g. three days of daily-bar confirmation
// becomes three 30-second ticks against a 30s eval_interval — about 90
// seconds). Shared between printBestEngine's YAML comment and the -json
// warnings field, so a script consuming JSON output learns the same caveat
// a human reading the paste-ready YAML would see.
const confirmCountWarning = "confirm_buy/confirm_sell count BARS in the backtest but EVALUATIONS " +
	"live — set engine.eval_interval == the backtest's bar interval, or the live bot confirms " +
	"on a completely different timescale than what was tuned here."

// printBestExits emits the exits block, including the ATR-adaptive stops.
func printBestExits(t backtest.Trial) {
	fmt.Println("exits:")
	fmt.Println("  enabled: true")
	fmt.Printf("  stop_loss_pct: %.2f\n", t.Exits.StopLossPct)
	fmt.Printf("  trailing_stop_pct: %.2f%s\n",
		t.Exits.TrailingStopPct, disabledNote(t.Exits.TrailingStopPct))
	fmt.Printf("  take_profit_pct: %.2f%s\n",
		t.Exits.TakeProfitPct, disabledNote(t.Exits.TakeProfitPct))
	fmt.Printf("  atr_stop_mult: %.1f%s\n",
		t.Exits.ATRStopMult, disabledNote(t.Exits.ATRStopMult))
	fmt.Printf("  atr_trail_mult: %.1f%s\n",
		t.Exits.ATRTrailMult, disabledNote(t.Exits.ATRTrailMult))
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

// refineUnlessCancelled runs refine unless ctx is already done. An interrupted
// search returns its partial trials, and Refine — which takes no context —
// then swept them in full before anything was written, so Ctrl+C appeared to
// do nothing for as long as the sweep took (CM2-6).
func refineUnlessCancelled(
	ctx context.Context, refine func() (backtest.Trial, []backtest.Refinement), best backtest.Trial,
) (backtest.Trial, []backtest.Refinement) {
	if ctx.Err() != nil {
		_, _ = fmt.Fprintln(
			os.Stderr,
			"\ninterrupted: skipping the refinement pass; reporting the un-refined winner",
		)

		return best, nil
	}

	return refine()
}

// flagConflicts lists explicitly set flags the chosen mode would ignore.
// -sensitivity outranks -walkforward, which outranks the search flags, and
// the losing flags were dropped without a word: "-walkforward 5 -random 2000"
// ran the default grid per fold while the banner never mentioned random
// (CM2-7). Only flags the user actually typed count — a default is not a
// request.
// activeFlags lists the flags set on the command line, a mode flag only while
// its mode is ON. flag.Visit reports a flag that was typed whatever its value,
// so "-walkforward 0 -random 500" aborted with "-random has no effect with
// -walkforward" — a mode switched off claiming the others — while
// "-random 0 -seed 7" counted -random as on and ran a grid ignoring -seed
// without a word (CM3-5).
func activeFlags(fs *flag.FlagSet) map[string]bool {
	set := make(map[string]bool)

	fs.Visit(func(f *flag.Flag) {
		switch v := f.Value.String(); f.Name {
		case "sensitivity":
			set[f.Name] = v == "true"
		case "walkforward", "random":
			set[f.Name] = v != "0" && v != ""
		default:
			set[f.Name] = true
		}
	})

	return set
}

func flagConflicts(set map[string]bool) []string {
	var out []string

	ignored := func(mode string, names ...string) {
		for _, n := range names {
			if set[n] {
				out = append(out, fmt.Sprintf("-%s has no effect with -%s", n, mode))
			}
		}
	}

	switch {
	case set["sensitivity"]:
		ignored("sensitivity", "walkforward", "random", "generations", "seed", "refine", "json",
			"checkpoint", "min-trades", "rank", "top")

	case set["walkforward"]:
		ignored("walkforward", "random", "generations", "seed", "refine",
			"checkpoint", "top", "deploy")

	default:
		if set["deploy"] {
			out = append(out, "-deploy only applies to -sensitivity")
		}

		for _, n := range []string{"generations", "seed"} {
			if set[n] && !set["random"] {
				out = append(out, fmt.Sprintf("-%s only applies to -random searches", n))
			}
		}
	}

	return out
}
