package backtest

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"slices"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// ProgressOutput is where progressBar writes. Defaults to stderr for
// cmd/optimize's own process, where a long grid/guided search visibly
// advancing rather than looking hung is the whole point. cmd/bot's process
// (the only other importer of this package, via internal/api's per-symbol
// optimizer) swaps this to io.Discard in its own init — see
// internal/api/symbol_optimize.go — since a `\r`-redrawn CLI progress line
// has no reader on a running server's stderr, only a log spam problem.
// Swapping it is safe: the two are always separate OS processes, never
// sharing this package-level state.
var ProgressOutput io.Writer = os.Stderr //nolint:gochecknoglobals // see doc comment

// progressBar prints a throttled progress line to ProgressOutput, so long
// optimisation runs visibly advance rather than looking hung.
type progressBar struct {
	total int
	last  time.Time
}

// newProgress creates a progress line over total steps.
func newProgress(total int) *progressBar { return &progressBar{total: total} }

// tick redraws the progress line, throttled to twice per second.
func (p *progressBar) tick(done int) {
	if p == nil || p.total <= 0 {
		return
	}

	if done < p.total && time.Since(p.last) < 500*time.Millisecond {
		return
	}

	p.last = time.Now()
	fmt.Fprintf(
		ProgressOutput,
		"\r  progress %d/%d (%.0f%%)   ",
		done,
		p.total,
		100*float64(done)/float64(p.total),
	)

	if done >= p.total {
		fmt.Fprintln(ProgressOutput)
	}
}

// ParamGrid defines the axes the optimizer sweeps. Every combination of the
// listed values is backtested. A 0 in TrailingStop/TakeProfit disables that
// exit (letting a trend run). CapitalFrac is the per-trade order size as a
// fraction of starting cash — the dominant lever for total return.
type ParamGrid struct {
	DetectorSets [][]string
	Combine      []string
	MinStrength  []float64
	CapitalFrac  []float64
	StopLoss     []float64
	TrailingStop []float64
	TakeProfit   []float64
}

// Trial is one parameter combination and its backtest outcome.
//
// Params carries the whole strategy configuration, including the SELL-side
// settings (detectors_sell, combine_sell, min_strength_sell) that the grid
// leaves alone and Refine explores. Exits and the behaviour fields carry the
// settings that live outside StrategyConfig, so a trial fully describes a
// runnable configuration rather than just the axes the grid happened to sweep.
type Trial struct {
	Detectors    []string              `json:"detectors"`
	Combine      string                `json:"combine"`
	MinStrength  float64               `json:"min_strength"`
	CapitalFrac  float64               `json:"capital_frac"`
	OrderSize    float64               `json:"order_size"`
	StopLoss     float64               `json:"stop_loss"`
	TrailingStop float64               `json:"trailing_stop"`
	TakeProfit   float64               `json:"take_profit"`
	Params       config.StrategyConfig `json:"params"` // full strategy params incl. indicator periods
	Result       Result                `json:"result"`
	Score        float64               `json:"score"`

	// Exits carries the full exit rules, so the ATR-adaptive stops and any ROI
	// ladder survive into the emitted config rather than being flattened to the
	// three percentages above.
	Exits config.ExitsConfig `json:"exits"`
	// Engine behaviour that Refine tunes.
	ConfirmBuy   int           `json:"confirm_buy"`
	ConfirmSell  int           `json:"confirm_sell"`
	KeepInterval time.Duration `json:"keep_interval"`
}

// relaxedLimits returns money limits wide enough not to cap the deployment being
// searched, so the optimiser measures the strategy rather than the caps. The
// caller applies real limits before trading.
func relaxedLimits(orderSize, cash float64) config.Limits {
	return config.Limits{
		MaxTotalInvested: cash,
		MaxPerPosition:   orderSize * 1.05,
		MaxOrderValue:    orderSize * 1.05,
		MaxDailySpend:    cash * 100,
		CashReserve:      0,
		MaxOpenPositions: 10000,
	}
}

// runTrialConfig backtests a fully-specified trial, honouring every field it
// carries — including the ones the grid never varies. Refine uses it so a
// refined trial is measured under exactly the settings it will be emitted with.
func runTrialConfig(
	base config.Config,
	data map[string][]market.Candle,
	t Trial,
	cash float64,
	respectLimits bool,
) Result {
	lim := base.Limits
	if !respectLimits {
		lim = relaxedLimits(t.OrderSize, cash)
	}

	bt := &Backtester{
		Strategy:        strategy.New(t.Params),
		Limits:          lim,
		Exits:           t.Exits,
		Costs:           base.Costs,
		Cash:            cash,
		OrderSize:       t.OrderSize,
		AllowPyramiding: base.Engine.AllowPyramiding,
		ScaleIn:         base.Engine.ScaleIn,
		ConfirmBuy:      t.ConfirmBuy,
		ConfirmSell:     t.ConfirmSell,
		KeepInterval:    t.KeepInterval,
		MaxRiskSizeUSD:  base.Engine.MaxRiskSizeUSD,
		MinOrderSizeUSD: base.Engine.MinOrderSizeUSD,
		Portfolio:       base.Portfolio,
		Sectors:         base.Sectors,
		AssetClasses:    base.AssetClasses,
		Protections:     base.Protections,
	}

	return dropHeavyFields(bt.Run(data))
}

// DefaultGrid returns a sensible, bounded search space (~1300 combos).
func DefaultGrid() ParamGrid {
	return ParamGrid{
		DetectorSets: [][]string{
			{"ema_cross", "rsi", "macd"},
			{"ema_cross", "macd"},
			{"rsi", "macd", "bollinger"},
			{"ema_cross", "rsi", "macd", "vwap"},
			{"ema_cross", "rsi", "macd", "vwap", "trend"},
			{"macd", "bollinger", "vwap"},
			{"ema_cross", "adx", "supertrend"},
			{"rsi", "stochastic", "macd"},
			{"donchian", "atr", "trend"},
			{
				"ema_cross",
				"rsi",
				"macd",
				"bollinger",
				"vwap",
				"atr",
				"rvol",
				"volume_profile",
				"trend",
				"adx",
				"stochastic",
				"supertrend",
				"donchian",
			},
		},
		Combine:      []string{"majority", "weighted", "unanimous"},
		MinStrength:  []float64{0.10, 0.20, 0.30},
		CapitalFrac:  []float64{0.10, 0.25, 0.50}, // 10% / 25% / 50% of cash per trade
		StopLoss:     []float64{0.05, 0.10},
		TrailingStop: []float64{0.08, 0}, // 0 = no trailing stop
		TakeProfit:   []float64{0.20, 0}, // 0 = no take-profit (ride the trend)
	}
}

// Scorer ranks a backtest Result; larger is better.
type Scorer func(Result) float64

// ScoreBlend rewards earnings and win rate, penalises drawdown — the default.
func ScoreBlend(r Result) float64 {
	return r.TotalReturnPct + (r.WinRatePct-50)*0.04 - r.MaxDrawdownPct*0.1
}

// ScoreReturn ranks purely by total return ("most earnings").
func ScoreReturn(r Result) float64 { return r.TotalReturnPct }

// ScoreWinRate ranks purely by win rate ("most wins").
func ScoreWinRate(r Result) float64 { return r.WinRatePct }

// ScoreSharpe ranks by risk-adjusted return.
func ScoreSharpe(r Result) float64 { return r.Sharpe }

// ScoreSortino ranks by return per unit of *downside* deviation, so a strategy
// is not punished for its good days the way Sharpe punishes them.
func ScoreSortino(r Result) float64 { return r.Sortino }

// ScoreCalmar ranks by annual return per unit of worst drawdown — the objective
// to use when surviving the equity curve matters as much as its endpoint.
func ScoreCalmar(r Result) float64 { return r.Calmar }

// ScoreProfitFactor ranks by gross profit over gross loss, a pure trade-quality
// objective that ignores how much capital was deployed.
func ScoreProfitFactor(r Result) float64 { return r.ProfitFactor }

// ScoreExpectancy ranks by mean P&L per round trip, which is what actually
// compounds: it rewards a modest win rate with large winners over a high win
// rate with ruinous losers.
func ScoreExpectancy(r Result) float64 { return r.ExpectancyPct }

// ScorerNames lists the objectives ScorerByName accepts, for CLI help.
var ScorerNames = []string{
	"blend", "return", "winrate", "sharpe", "sortino", "calmar",
	"profitfactor", "expectancy",
}

// ScorerByName maps a CLI name to a Scorer. ok reports whether name was
// actually recognized; an unrecognized name still returns ScoreBlend (the
// historical default, so a caller that ignores ok keeps working exactly as
// before), but a caller that checks ok can warn about a typo instead of
// silently substituting a different ranking objective than the one
// requested — see cmd/optimize/main.go's own -rank validation, added
// because a typo used to fall back to blend while the run's own banner
// kept printing the (wrong, never-actually-used) requested name. See
// CODE_REVIEW_FINDINGS.md's "-rank typos silently fall back to the blend
// scorer" finding.
func ScorerByName(name string) (score Scorer, ok bool) {
	switch name {
	case "return", "earnings":
		return ScoreReturn, true
	case "winrate", "wins":
		return ScoreWinRate, true
	case "sharpe":
		return ScoreSharpe, true
	case "sortino":
		return ScoreSortino, true
	case "calmar":
		return ScoreCalmar, true
	case "profitfactor", "pf":
		return ScoreProfitFactor, true
	case "expectancy":
		return ScoreExpectancy, true
	case "blend":
		return ScoreBlend, true
	default:
		return ScoreBlend, false
	}
}

// Optimize backtests every grid combination against data and returns the trials
// sorted best-first by score. Trials with fewer than minTrades trades are
// excluded so a lucky 1-trade run cannot top the board. When respectLimits is
// false (the default for return optimisation), the base money limits are
// relaxed so they don't cap capital deployment — the optimizer then measures the
// strategy's potential, and the caller applies real safety limits live.
//
// ctx is checked once per combination; on cancellation, Optimize returns
// immediately with whatever trials it has already found (sorted), rather
// than running to completion or leaving the caller no way to stop a
// multi-hour grid early and keep what it already has. A nil ctx runs to
// completion exactly as before. See CODE_REVIEW_FINDINGS.md's "cmd/optimize:
// no cancellation, no incremental result persistence".
func Optimize(ctx context.Context, base config.Config, data map[string][]market.Candle, grid ParamGrid,
	cash float64, minTrades int, score Scorer, respectLimits bool,
) []Trial {
	var trials []Trial

	prog := newProgress(grid.Combos())
	done := 0

	for _, ds := range grid.DetectorSets {
		for _, cm := range grid.Combine {
			for _, ms := range grid.MinStrength {
				for _, frac := range grid.CapitalFrac {
					for _, sl := range grid.StopLoss {
						for _, ts := range grid.TrailingStop {
							for _, tp := range grid.TakeProfit {
								if ctx != nil && ctx.Err() != nil {
									sortTrials(trials)

									return trials
								}

								sc := base.Strategy

								sc.Detectors = ds
								sc.Combine = cm
								sc.MinStrength = ms

								if t, ok := runTrial(
									base,
									data,
									sc,
									frac,
									sl,
									ts,
									tp,
									cash,
									minTrades,
									score,
									respectLimits,
								); ok {
									trials = append(trials, t)
								}

								done++
								prog.tick(done)
							}
						}
					}
				}
			}
		}
	}

	sortTrials(trials)

	return trials
}

// dropHeavyFields clears the per-bar/per-trade fields of a Result that a
// search trial never needs again once it has been scored: nothing in the
// grid search, random/guided search, sensitivity sweep, walk-forward or
// Refine ever reads Trades, Equity or RoundTrips from an intermediate
// trial — only the scalar summary fields (score, return, drawdown, ...)
// drive ranking and the leaderboard, and Refine re-backtests a candidate
// from scratch (via runTrialConfig) rather than reusing an earlier Result.
//
// Without this, a search keeps EVERY trial's full equity curve (one
// EquityPoint per bar) and trade/round-trip log in memory for as long as
// the run lasts — a few thousand trials over a few hundred thousand bars
// each is enough to exhaust available RAM. cmd/backtest's single-run report
// needs these fields and calls Backtester.Run directly, so it is
// unaffected: this only trims the optimizer-internal entry points
// (runConfig, runTrialConfig).
func dropHeavyFields(res Result) Result {
	res.Trades = nil
	res.Equity = nil
	res.RoundTrips = nil

	return res
}

// runConfig backtests one fully-specified parameter set and returns its Result
// (no filtering). Shared by the optimisers and the sensitivity analysis.
func runConfig(base config.Config, data map[string][]market.Candle, sc config.StrategyConfig,
	frac, sl, ts, tp float64, costs config.CostsConfig, cash float64, respectLimits bool,
) Result {
	orderSize := cash * frac
	lim := base.Limits

	if !respectLimits {
		lim = relaxedLimits(orderSize, cash)
	}

	ex := base.Exits

	ex.Enabled = true
	ex.StopLossPct = sl
	ex.TrailingStopPct = ts
	ex.TakeProfitPct = tp

	bt := &Backtester{
		Strategy:         strategy.New(sc),
		SymbolStrategies: nil, // the optimizer tunes the shared config, not overrides
		Limits:           lim,
		Exits:            ex,
		Costs:            costs,
		Cash:             cash,
		OrderSize:        orderSize,
		AllowPyramiding:  base.Engine.AllowPyramiding,
		ScaleIn:          base.Engine.ScaleIn,
		ConfirmBuy:       base.Engine.ConfirmBuy,
		ConfirmSell:      base.Engine.ConfirmSell,
		KeepInterval:     base.Engine.KeepInterval,
		MaxRiskSizeUSD:   base.Engine.MaxRiskSizeUSD,
		MinOrderSizeUSD:  base.Engine.MinOrderSizeUSD,
		Portfolio:        base.Portfolio,
		Sectors:          base.Sectors,
		AssetClasses:     base.AssetClasses,
		Protections:      base.Protections,
	}

	return dropHeavyFields(bt.Run(data))
}

// runTrial backtests one fully-specified parameter set and builds a Trial,
// dropping it when it has fewer than minTrades trades.
func runTrial(base config.Config, data map[string][]market.Candle, sc config.StrategyConfig,
	frac, sl, ts, tp, cash float64, minTrades int, score Scorer, respectLimits bool,
) (Trial, bool) {
	res := runConfig(base, data, sc, frac, sl, ts, tp, base.Costs, cash, respectLimits)
	if res.NumTrades < minTrades {
		return Trial{}, false
	}

	// Carry the settings the grid does not vary, so the trial fully describes a
	// runnable configuration and refinement has a complete starting point.
	ex := base.Exits

	ex.Enabled = true
	ex.StopLossPct = sl
	ex.TrailingStopPct = ts
	ex.TakeProfitPct = tp

	return Trial{
		Detectors: sc.Detectors, Combine: sc.Combine, MinStrength: sc.MinStrength,
		CapitalFrac: frac, OrderSize: cash * frac,
		StopLoss: sl, TrailingStop: ts, TakeProfit: tp,
		Params: sc, Result: res, Score: score(res),
		Exits:        ex,
		ConfirmBuy:   base.Engine.ConfirmBuy,
		ConfirmSell:  base.Engine.ConfirmSell,
		KeepInterval: base.Engine.KeepInterval,
	}, true
}

// sortTrials orders trials by score, tie-breaking on win rate then fewer trades.
func sortTrials(trials []Trial) {
	slices.SortStableFunc(trials, func(a, b Trial) int {
		if c := cmp.Compare(b.Score, a.Score); c != 0 {
			return c
		}

		// Tie-break: prefer higher win rate, then fewer trades (simpler).
		if c := cmp.Compare(b.Result.WinRatePct, a.Result.WinRatePct); c != 0 {
			return c
		}

		return cmp.Compare(a.Result.NumTrades, b.Result.NumTrades)
	})
}

// OptimizeRandom samples `n` random parameter sets across the full space —
// including the indicator periods/thresholds — and returns the best-scoring
// trials. Random search handles the high-dimensional indicator space that a
// full grid cannot (the grid would be millions of combinations). Deterministic
// for a given seed.
//
// ctx cancellation is honored the same way Optimize's is — see its doc
// comment. A nil ctx runs to completion exactly as before.
//
// pins collapses the deployment axes a caller cannot apply to a single value
// — see Pins. Its zero value searches everything, the historical behaviour.
func OptimizeRandom(ctx context.Context, base config.Config, data map[string][]market.Candle,
	cash float64, n, minTrades int, score Scorer, respectLimits bool, seed int64,
	pins Pins,
) []Trial {
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic sampling
	space := defaultSpace(pins, base.Strategy)
	r := newRunner(ctx, base, data, cash, minTrades, score, respectLimits, n)

	for range n {
		r.eval(space.sample(rng, base.Strategy))
	}

	return r.results()
}

// eliteFraction is the share of a generation kept as parents for the next one.
// Small enough to concentrate the search on what worked, large enough that one
// lucky trial cannot dictate the rest of the run.
const eliteFraction = 0.2

// OptimizeGuided runs a guided search: one random generation, then successive
// generations sampled around the best configurations found so far.
//
// Random search treats every sample as independent, so it keeps re-testing
// regions it has already learned are bad. This keeps the elite fraction of each
// generation and breeds the next from them, resampling each parameter
// independently with probability `rate` — high at first (broad exploration),
// decaying each generation (refinement around what works). It concentrates a
// fixed backtest budget near the configurations that scored well, without the
// machinery — or the surrogate-model assumptions — of a full Bayesian
// optimiser.
//
// It is not a free lunch. Concentrating the budget only helps when the
// objective has structure worth refining toward; on noise it will happily
// converge on noise, and converge harder than random search would. Judge the
// result with -walkforward, not by the leaderboard it prints.
//
// Deterministic for a given seed. n is the total number of backtests across all
// generations.
//
// ctx cancellation is honored the same way Optimize's is — see its doc
// comment. A nil ctx runs to completion exactly as before.
//
// pins collapses the deployment axes a caller cannot apply to a single value
// — see Pins. Its zero value searches everything, the historical behaviour.
func OptimizeGuided(ctx context.Context, base config.Config, data map[string][]market.Candle,
	cash float64, n, generations, minTrades int, score Scorer,
	respectLimits bool, seed int64, pins Pins,
) []Trial {
	if generations < 2 {
		// A single generation IS random search; don't pretend otherwise.
		return OptimizeRandom(ctx, base, data, cash, n, minTrades, score, respectLimits, seed, pins)
	}

	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic sampling
	space := defaultSpace(pins, base.Strategy)
	r := newRunner(ctx, base, data, cash, minTrades, score, respectLimits, n)

	perGen := max(n/generations, 1)

	// Generation 0 is random: with no information yet, uniform sampling is the
	// best available guess.
	for range perGen {
		r.eval(space.sample(rng, base.Strategy))
	}

	for gen := 1; gen < generations; gen++ {
		parents := elites(r.results(), perGen)
		if len(parents) == 0 {
			// Nothing has cleared min-trades yet, so there is nothing to learn
			// from; keep exploring randomly rather than breeding from noise.
			for range perGen {
				r.eval(space.sample(rng, base.Strategy))
			}

			continue
		}

		// The mutation rate decays across generations: explore first, refine later.
		rate := max(0.5*(1-float64(gen-1)/float64(generations-1)), 0.08)

		for range perGen {
			parent := parents[rng.Intn(len(parents))]
			r.eval(space.mutate(rng, toCandidate(parent), rate))
		}
	}

	return r.results()
}

// elites returns the best trials to breed the next generation from.
func elites(sorted []Trial, genSize int) []Trial {
	n := max(int(float64(genSize)*eliteFraction), 1)
	if n > len(sorted) {
		n = len(sorted)
	}

	return sorted[:n]
}

// pick returns a uniformly-random element of xs.
func pick[T any](rng *rand.Rand, xs []T) T { return xs[rng.Intn(len(xs))] }

// SweepRow is one tested value of a parameter and its backtest result.
type SweepRow struct {
	Label    string `json:"label"`
	Baseline bool   `json:"baseline"`
	Result   Result `json:"result"`
}

// AxisSweep holds every value tested for one parameter (others held at baseline).
type AxisSweep struct {
	Axis string     `json:"axis"`
	Rows []SweepRow `json:"rows"`
}

// Sensitivity runs a one-factor-at-a-time analysis: starting from the base
// config, it changes a single parameter across its candidate values (holding
// everything else fixed), records the result, then moves to the next parameter.
// It makes plain that many distinct configs were tested and how each parameter
// moves the outcome. Returns the baseline result and the per-axis sweeps.
func Sensitivity(base config.Config, data map[string][]market.Candle,
	cash, baseFrac float64, respectLimits bool,
) (Result, []AxisSweep) {
	return sensitivity(base, data, cash, baseFrac, respectLimits, "")
}

// SensitivityAxis runs the one-factor-at-a-time analysis for a single named
// axis (see AxisSweep.Axis for the names Sensitivity reports), skipping the
// rest of the sweep. It exists for callers that only want one parameter's
// answer — an interactive UI, say — and would rather not pay for the other
// two dozen axes to get it. An unknown axis returns a nil sweep.
func SensitivityAxis(base config.Config, data map[string][]market.Candle,
	cash, baseFrac float64, respectLimits bool, axis string,
) (Result, *AxisSweep) {
	baseline, axes := sensitivity(base, data, cash, baseFrac, respectLimits, axis)
	if len(axes) == 0 {
		return baseline, nil
	}

	return baseline, &axes[0]
}

// sensitivity is the shared implementation behind Sensitivity and
// SensitivityAxis. only, when non-empty, restricts the sweep to the axis of
// that name; empty runs every axis.
//
//nolint:funlen // one sweep block per parameter axis
func sensitivity(base config.Config, data map[string][]market.Candle,
	cash, baseFrac float64, respectLimits bool, only string,
) (Result, []AxisSweep) {
	b := base.Strategy
	sl0, ts0, tp0 := base.Exits.StopLossPct, base.Exits.TrailingStopPct, base.Exits.TakeProfitPct
	c0 := base.Costs
	baseline := runConfig(base, data, b, baseFrac, sl0, ts0, tp0, c0, cash, respectLimits)

	var axes []AxisSweep

	addStr := func(axis string, vals []string, baseVal string, mut func(string) config.StrategyConfig) {
		if only != "" && only != axis {
			return
		}

		rows := make([]SweepRow, 0, len(vals))
		for _, v := range vals {
			res := runConfig(base, data, mut(v), baseFrac, sl0, ts0, tp0, c0, cash, respectLimits)

			rows = append(rows, SweepRow{Label: v, Baseline: v == baseVal, Result: res})
		}

		axes = append(axes, AxisSweep{Axis: axis, Rows: rows})
	}
	addFloatSC := func(axis string, vals []float64, baseVal float64, mut func(float64) config.StrategyConfig) {
		if only != "" && only != axis {
			return
		}

		rows := make([]SweepRow, 0, len(vals))
		for _, v := range vals {
			res := runConfig(base, data, mut(v), baseFrac, sl0, ts0, tp0, c0, cash, respectLimits)

			rows = append(
				rows,
				SweepRow{Label: fmt.Sprintf("%.2f", v), Baseline: v == baseVal, Result: res},
			)
		}

		axes = append(axes, AxisSweep{Axis: axis, Rows: rows})
	}
	addIntSC := func(axis string, vals []int, baseVal int, mut func(int) config.StrategyConfig) {
		if only != "" && only != axis {
			return
		}

		rows := make([]SweepRow, 0, len(vals))
		for _, v := range vals {
			res := runConfig(base, data, mut(v), baseFrac, sl0, ts0, tp0, c0, cash, respectLimits)

			rows = append(
				rows,
				SweepRow{Label: fmt.Sprintf("%d", v), Baseline: v == baseVal, Result: res},
			)
		}

		axes = append(axes, AxisSweep{Axis: axis, Rows: rows})
	}
	// exits/deployment vary the backtest knobs, not the strategy config.
	addExit := func(axis string, vals []float64, baseVal float64, which int) {
		if only != "" && only != axis {
			return
		}

		rows := make([]SweepRow, 0, len(vals))
		for _, v := range vals {
			sl, ts, tp := sl0, ts0, tp0
			switch which {
			case 0:
				sl = v
			case 1:
				ts = v
			case 2:
				tp = v
			}

			res := runConfig(base, data, b, baseFrac, sl, ts, tp, c0, cash, respectLimits)

			rows = append(
				rows,
				SweepRow{Label: fmt.Sprintf("%.2f", v), Baseline: v == baseVal, Result: res},
			)
		}

		axes = append(axes, AxisSweep{Axis: axis, Rows: rows})
	}
	// costs vary the friction model, not the strategy config or the exit
	// rules — same shape as addExit, one field of CostsConfig at a time.
	addCost := func(axis string, vals []float64, baseVal float64, which int) {
		if only != "" && only != axis {
			return
		}

		rows := make([]SweepRow, 0, len(vals))
		for _, v := range vals {
			costs := c0
			switch which {
			case 0:
				costs.CommissionPct = v
			case 1:
				costs.CommissionFlat = v
			case 2:
				costs.CommissionMin = v
			case 3:
				costs.SpreadPct = v
			case 4:
				costs.SlippagePct = v
			}

			res := runConfig(base, data, b, baseFrac, sl0, ts0, tp0, costs, cash, respectLimits)

			rows = append(
				rows,
				SweepRow{Label: fmt.Sprintf("%.4f", v), Baseline: v == baseVal, Result: res},
			)
		}

		axes = append(axes, AxisSweep{Axis: axis, Rows: rows})
	}

	addStr("combine", []string{"majority", "weighted", "unanimous"}, b.Combine,
		func(v string) config.StrategyConfig { c := b; c.Combine = v; return c })
	addFloatSC("min_strength", []float64{0.10, 0.20, 0.30, 0.40}, b.MinStrength,
		func(v float64) config.StrategyConfig { c := b; c.MinStrength = v; return c })

	// capital deployment (varies order size, not strategy config).
	if only == "" || only == "capital_frac" {
		vals := []float64{0.10, 0.25, 0.50}
		rows := make([]SweepRow, 0, len(vals))

		for _, v := range vals {
			res := runConfig(base, data, b, v, sl0, ts0, tp0, c0, cash, respectLimits)

			rows = append(
				rows,
				SweepRow{Label: fmt.Sprintf("%.0f%%", v*100), Baseline: v == baseFrac, Result: res},
			)
		}

		axes = append(axes, AxisSweep{Axis: "capital_frac", Rows: rows})
	}

	addExit("stop_loss", []float64{0.03, 0.05, 0.10}, sl0, 0)
	addExit("trailing_stop", []float64{0.0, 0.05, 0.08}, ts0, 1)
	addExit("take_profit", []float64{0.0, 0.10, 0.20, 0.40}, tp0, 2)

	// Friction, not signal — but it eats into the same P&L the other axes
	// tune for, and a config sized for one venue's costs can look very
	// different once slippage or a real commission schedule is charged.
	addCost("commission_pct", []float64{0.0005, 0.0010, 0.0015, 0.0020}, c0.CommissionPct, 0)
	addCost("commission_flat", []float64{0.0, 0.5, 1.0}, c0.CommissionFlat, 1)
	addCost("commission_min", []float64{0.0, 0.001, 0.005, 0.01}, c0.CommissionMin, 2)
	addCost("spread_pct", []float64{0.0002, 0.0005, 0.0010, 0.0020}, c0.SpreadPct, 3)
	addCost("slippage_pct", []float64{0.0, 0.0005, 0.0010, 0.0020}, c0.SlippagePct, 4)

	addIntSC("fast_ma", []int{8, 10, 12, 20}, b.FastMA,
		func(v int) config.StrategyConfig { c := b; c.FastMA = v; return c })
	addIntSC("slow_ma", []int{21, 26, 34, 50}, b.SlowMA,
		func(v int) config.StrategyConfig { c := b; c.SlowMA = v; return c })
	addIntSC("rsi_period", []int{7, 9, 14, 21}, b.RSIPeriod,
		func(v int) config.StrategyConfig { c := b; c.RSIPeriod = v; return c })
	addIntSC("macd_fast", []int{5, 8, 12}, b.MACDFast,
		func(v int) config.StrategyConfig { c := b; c.MACDFast = v; return c })
	addIntSC("macd_slow", []int{21, 26, 35}, b.MACDSlow,
		func(v int) config.StrategyConfig { c := b; c.MACDSlow = v; return c })
	addIntSC("macd_signal", []int{5, 7, 9}, b.MACDSignal,
		func(v int) config.StrategyConfig { c := b; c.MACDSignal = v; return c })
	addIntSC("bb_period", []int{14, 20, 30}, b.BBPeriod,
		func(v int) config.StrategyConfig { c := b; c.BBPeriod = v; return c })
	addFloatSC("bb_stddev", []float64{1.5, 2.0, 2.5}, b.BBStdDev,
		func(v float64) config.StrategyConfig { c := b; c.BBStdDev = v; return c })
	addIntSC("vwap_period", []int{14, 20, 50}, b.VWAPPeriod,
		func(v int) config.StrategyConfig { c := b; c.VWAPPeriod = v; return c })
	addIntSC("atr_period", []int{10, 14, 21}, b.ATRPeriod,
		func(v int) config.StrategyConfig { c := b; c.ATRPeriod = v; return c })
	addFloatSC("atr_mult", []float64{1.0, 1.5, 2.0, 2.5}, b.ATRMult,
		func(v float64) config.StrategyConfig { c := b; c.ATRMult = v; return c })
	addIntSC("rvol_period", []int{10, 20, 30}, b.RVOLPeriod,
		func(v int) config.StrategyConfig { c := b; c.RVOLPeriod = v; return c })
	addFloatSC("rvol_threshold", []float64{1.2, 1.5, 2.0}, b.RVOLThreshold,
		func(v float64) config.StrategyConfig { c := b; c.RVOLThreshold = v; return c })

	// ADX, Stochastic, Supertrend, Donchian and the trend gate. These were
	// missing for a long time, which made the sweep actively misleading for any
	// config built on them: it would report "+0.00 for everything" across
	// sixteen axes while never touching the parameters the strategy actually
	// ran on.
	addIntSC("adx_period", []int{10, 14, 20}, b.ADXPeriod,
		func(v int) config.StrategyConfig { c := b; c.ADXPeriod = v; return c })
	addFloatSC("adx_threshold", []float64{15, 20, 25, 30, 35}, b.ADXThreshold,
		func(v float64) config.StrategyConfig { c := b; c.ADXThreshold = v; return c })
	addIntSC("stoch_k_period", []int{9, 14, 21}, b.StochKPeriod,
		func(v int) config.StrategyConfig { c := b; c.StochKPeriod = v; return c })
	addIntSC("stoch_d_period", []int{3, 5}, b.StochDPeriod,
		func(v int) config.StrategyConfig { c := b; c.StochDPeriod = v; return c })
	addFloatSC("stoch_overbought", []float64{75, 80, 85}, b.StochOverbought,
		func(v float64) config.StrategyConfig { c := b; c.StochOverbought = v; return c })
	addFloatSC("stoch_oversold", []float64{15, 20, 25}, b.StochOversold,
		func(v float64) config.StrategyConfig { c := b; c.StochOversold = v; return c })
	addIntSC("supertrend_period", []int{7, 10, 14, 21}, b.SupertrendPeriod,
		func(v int) config.StrategyConfig { c := b; c.SupertrendPeriod = v; return c })
	addFloatSC("supertrend_mult", []float64{1.5, 2.0, 3.0, 4.0}, b.SupertrendMult,
		func(v float64) config.StrategyConfig { c := b; c.SupertrendMult = v; return c })
	addIntSC("donchian_period", []int{10, 20, 55}, b.DonchianPeriod,
		func(v int) config.StrategyConfig { c := b; c.DonchianPeriod = v; return c })

	// The trend gate is swept too: it is independent of the detector set, so it
	// affects every configuration whether or not "trend" is listed.
	addIntSC("trend_period", []int{390, 1050, 1950}, b.TrendPeriod,
		func(v int) config.StrategyConfig { c := b; c.TrendPeriod = v; return c })
	addFloatSC("trend_threshold", []float64{0.002, 0.005, 0.01, 0.02}, b.TrendThreshold,
		func(v float64) config.StrategyConfig { c := b; c.TrendThreshold = v; return c })
	addStr("trend_gate", []string{"on", "off"}, boolLabel(b.TrendGate),
		func(v string) config.StrategyConfig { c := b; c.TrendGate = v == "on"; return c })

	// The sell-side conviction gate, which is often left inheriting the buy
	// side without anyone checking whether exiting should be readier.
	addFloatSC("min_strength_sell", []float64{0.05, 0.10, 0.20, 0.30}, b.MinStrengthSell,
		func(v float64) config.StrategyConfig { c := b; c.MinStrengthSell = v; return c })
	addStr("combine_sell", []string{"", "majority", "unanimous", "weighted"}, b.CombineSell,
		func(v string) config.StrategyConfig { c := b; c.CombineSell = v; return c })

	// RSI's own overbought/oversold levels and voting mode — swept
	// independently of rsi_period, same as every other detector's period and
	// threshold axes are swept independently of each other.
	addStr("rsi_mode", []string{"midline", "extremes"}, b.RSIMode,
		func(v string) config.StrategyConfig { c := b; c.RSIMode = v; return c })
	addFloatSC("rsi_overbought", []float64{65, 70, 75, 80}, b.RSIOverbought,
		func(v float64) config.StrategyConfig { c := b; c.RSIOverbought = v; return c })
	addFloatSC("rsi_oversold", []float64{20, 25, 30, 35}, b.RSIOversold,
		func(v float64) config.StrategyConfig { c := b; c.RSIOversold = v; return c })

	// Volume profile's own axes (only meaningful when "volume_profile" is an
	// active detector, same caveat as every other detector-specific axis
	// above: the sweep runs regardless of the active detector set).
	addIntSC("vp_window", []int{60, 120, 240}, b.VPWindow,
		func(v int) config.StrategyConfig { c := b; c.VPWindow = v; return c })
	addIntSC("vp_buckets", []int{12, 24, 48}, b.VPBuckets,
		func(v int) config.StrategyConfig { c := b; c.VPBuckets = v; return c })
	addFloatSC("vp_value_area_pct", []float64{0.6, 0.7, 0.8}, b.VPValueAreaPct,
		func(v float64) config.StrategyConfig { c := b; c.VPValueAreaPct = v; return c })

	return baseline, axes
}

// boolLabel renders a boolean axis value as the string the sweep compares on.
func boolLabel(v bool) string {
	if v {
		return "on"
	}

	return "off"
}

// Combos returns the number of combinations the grid will evaluate.
func (g ParamGrid) Combos() int {
	n := len(g.DetectorSets) * len(g.Combine) * len(g.MinStrength) * len(g.CapitalFrac)

	n *= len(g.StopLoss) * len(g.TrailingStop) * len(g.TakeProfit)
	return n
}
