package backtest

import (
	"cmp"
	"fmt"
	"math/rand"
	"os"
	"slices"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// progressBar prints a throttled progress line to stderr, so long optimisation
// runs visibly advance rather than looking hung.
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
		os.Stderr,
		"\r  progress %d/%d (%.0f%%)   ",
		done,
		p.total,
		100*float64(done)/float64(p.total),
	)

	if done >= p.total {
		fmt.Fprintln(os.Stderr)
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

// ScorerByName maps a CLI name to a Scorer.
func ScorerByName(name string) Scorer {
	switch name {
	case "return", "earnings":
		return ScoreReturn
	case "winrate", "wins":
		return ScoreWinRate
	case "sharpe":
		return ScoreSharpe
	default:
		return ScoreBlend
	}
}

// Optimize backtests every grid combination against data and returns the trials
// sorted best-first by score. Trials with fewer than minTrades trades are
// excluded so a lucky 1-trade run cannot top the board. When respectLimits is
// false (the default for return optimisation), the base money limits are
// relaxed so they don't cap capital deployment — the optimizer then measures the
// strategy's potential, and the caller applies real safety limits live.
func Optimize(base config.Config, data map[string][]market.Candle, grid ParamGrid,
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

// runConfig backtests one fully-specified parameter set and returns its Result
// (no filtering). Shared by the optimisers and the sensitivity analysis.
func runConfig(base config.Config, data map[string][]market.Candle, sc config.StrategyConfig,
	frac, sl, ts, tp, cash float64, respectLimits bool,
) Result {
	orderSize := cash * frac
	lim := base.Limits

	if !respectLimits {
		lim = config.Limits{
			MaxTotalInvested: cash,
			MaxPerPosition:   orderSize * 1.05,
			MaxOrderValue:    orderSize * 1.05,
			MaxDailySpend:    cash * 100,
			CashReserve:      0,
			MaxOpenPositions: 10000,
		}
	}

	ex := base.Exits

	ex.Enabled = true
	ex.StopLossPct = sl
	ex.TrailingStopPct = ts
	ex.TakeProfitPct = tp

	bt := &Backtester{
		Strategy:        strategy.New(sc),
		Limits:          lim,
		Exits:           ex,
		Costs:           base.Costs,
		Cash:            cash,
		OrderSize:       orderSize,
		AllowPyramiding: base.Engine.AllowPyramiding,
		ConfirmBuy:      base.Engine.ConfirmBuy,
		ConfirmSell:     base.Engine.ConfirmSell,
		KeepInterval:    base.Engine.KeepInterval,
	}

	return bt.Run(data)
}

// runTrial backtests one fully-specified parameter set and builds a Trial,
// dropping it when it has fewer than minTrades trades.
func runTrial(base config.Config, data map[string][]market.Candle, sc config.StrategyConfig,
	frac, sl, ts, tp, cash float64, minTrades int, score Scorer, respectLimits bool,
) (Trial, bool) {
	res := runConfig(base, data, sc, frac, sl, ts, tp, cash, respectLimits)
	if res.NumTrades < minTrades {
		return Trial{}, false
	}

	return Trial{
		Detectors: sc.Detectors, Combine: sc.Combine, MinStrength: sc.MinStrength,
		CapitalFrac: frac, OrderSize: cash * frac,
		StopLoss: sl, TrailingStop: ts, TakeProfit: tp,
		Params: sc, Result: res, Score: score(res),
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
//nolint:funlen // one candidate-value table per tunable parameter
func OptimizeRandom(base config.Config, data map[string][]market.Candle,
	cash float64, n, minTrades int, score Scorer, respectLimits bool, seed int64,
) []Trial {
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic sampling
	g := DefaultGrid()

	// Candidate values for each indicator parameter.
	maPairs := [][2]int{{8, 21}, {10, 30}, {12, 26}, {20, 50}}
	rsiPeriods := []int{7, 9, 14, 21}
	rsiOB := []float64{70, 75, 80}
	rsiOS := []float64{20, 25, 30}
	macdSets := [][3]int{{12, 26, 9}, {5, 35, 5}, {8, 21, 5}, {10, 20, 7}}
	bbPeriods := []int{14, 20, 30}
	bbStd := []float64{1.5, 2.0, 2.5}
	vwapPeriods := []int{14, 20, 50}
	atrPeriods := []int{10, 14, 21}
	atrMults := []float64{1.0, 1.5, 2.0, 2.5}
	rvolPeriods := []int{10, 20, 30}
	rvolThr := []float64{1.2, 1.5, 2.0}
	vpWindows := []int{60, 120, 200}
	vpBuckets := []int{16, 24, 32}
	adxPeriods := []int{10, 14, 20}
	adxThr := []float64{20, 25, 30}
	stochK := []int{9, 14, 21}
	stochD := []int{3, 5}
	stochOB := []float64{75, 80, 85}
	stochOS := []float64{15, 20, 25}
	stPeriods := []int{7, 10, 14}
	stMults := []float64{2.0, 3.0, 4.0}
	dcPeriods := []int{10, 20, 55}

	seen := make(map[string]bool)

	var trials []Trial

	prog := newProgress(n)

	for i := range n {
		prog.tick(i + 1)

		sc := base.Strategy

		sc.Detectors = pick(rng, g.DetectorSets)
		sc.Combine = pick(rng, g.Combine)
		sc.MinStrength = pick(rng, g.MinStrength)

		ma := pick(rng, maPairs)

		sc.FastMA, sc.SlowMA = ma[0], ma[1]
		sc.RSIPeriod = pick(rng, rsiPeriods)
		sc.RSIOverbought = pick(rng, rsiOB)
		sc.RSIOversold = pick(rng, rsiOS)

		md := pick(rng, macdSets)

		sc.MACDFast, sc.MACDSlow, sc.MACDSignal = md[0], md[1], md[2]
		sc.BBPeriod = pick(rng, bbPeriods)
		sc.BBStdDev = pick(rng, bbStd)
		sc.VWAPPeriod = pick(rng, vwapPeriods)
		sc.ATRPeriod = pick(rng, atrPeriods)
		sc.ATRMult = pick(rng, atrMults)
		sc.RVOLPeriod = pick(rng, rvolPeriods)
		sc.RVOLThreshold = pick(rng, rvolThr)
		sc.VPWindow = pick(rng, vpWindows)
		sc.VPBuckets = pick(rng, vpBuckets)
		sc.ADXPeriod = pick(rng, adxPeriods)
		sc.ADXThreshold = pick(rng, adxThr)
		sc.StochKPeriod = pick(rng, stochK)
		sc.StochDPeriod = pick(rng, stochD)
		sc.StochOverbought = pick(rng, stochOB)
		sc.StochOversold = pick(rng, stochOS)
		sc.SupertrendPeriod = pick(rng, stPeriods)
		sc.SupertrendMult = pick(rng, stMults)
		sc.DonchianPeriod = pick(rng, dcPeriods)

		frac := pick(rng, g.CapitalFrac)
		sl := pick(rng, g.StopLoss)
		ts := pick(rng, g.TrailingStop)
		tp := pick(rng, g.TakeProfit)

		sig := fmt.Sprintf(
			"%v|%s|%.2f|%d-%d|%d|%.0f/%.0f|%d-%d-%d|%d/%.1f|%d|%d/%.1f|%d/%.1f|%d/%d|%d/%.0f|%d/%d/%.0f/%.0f|%d/%.1f|%d|%.2f|%.2f-%.2f-%.2f",
			sc.Detectors,
			sc.Combine,
			sc.MinStrength,
			sc.FastMA,
			sc.SlowMA,
			sc.RSIPeriod,
			sc.RSIOverbought,
			sc.RSIOversold,
			sc.MACDFast,
			sc.MACDSlow,
			sc.MACDSignal,
			sc.BBPeriod,
			sc.BBStdDev,
			sc.VWAPPeriod,
			sc.ATRPeriod,
			sc.ATRMult,
			sc.RVOLPeriod,
			sc.RVOLThreshold,
			sc.VPWindow,
			sc.VPBuckets,
			sc.ADXPeriod,
			sc.ADXThreshold,
			sc.StochKPeriod,
			sc.StochDPeriod,
			sc.StochOverbought,
			sc.StochOversold,
			sc.SupertrendPeriod,
			sc.SupertrendMult,
			sc.DonchianPeriod,
			frac,
			sl,
			ts,
			tp,
		)
		if seen[sig] {
			continue
		}

		seen[sig] = true

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
	}

	sortTrials(trials)

	return trials
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
//
//nolint:funlen // one sweep block per parameter axis
func Sensitivity(base config.Config, data map[string][]market.Candle,
	cash, baseFrac float64, respectLimits bool,
) (Result, []AxisSweep) {
	b := base.Strategy
	sl0, ts0, tp0 := base.Exits.StopLossPct, base.Exits.TrailingStopPct, base.Exits.TakeProfitPct
	baseline := runConfig(base, data, b, baseFrac, sl0, ts0, tp0, cash, respectLimits)

	var axes []AxisSweep

	addStr := func(axis string, vals []string, baseVal string, mut func(string) config.StrategyConfig) {
		rows := make([]SweepRow, 0, len(vals))
		for _, v := range vals {
			res := runConfig(base, data, mut(v), baseFrac, sl0, ts0, tp0, cash, respectLimits)

			rows = append(rows, SweepRow{Label: v, Baseline: v == baseVal, Result: res})
		}

		axes = append(axes, AxisSweep{Axis: axis, Rows: rows})
	}
	addFloatSC := func(axis string, vals []float64, baseVal float64, mut func(float64) config.StrategyConfig) {
		rows := make([]SweepRow, 0, len(vals))
		for _, v := range vals {
			res := runConfig(base, data, mut(v), baseFrac, sl0, ts0, tp0, cash, respectLimits)

			rows = append(
				rows,
				SweepRow{Label: fmt.Sprintf("%.2f", v), Baseline: v == baseVal, Result: res},
			)
		}

		axes = append(axes, AxisSweep{Axis: axis, Rows: rows})
	}
	addIntSC := func(axis string, vals []int, baseVal int, mut func(int) config.StrategyConfig) {
		rows := make([]SweepRow, 0, len(vals))
		for _, v := range vals {
			res := runConfig(base, data, mut(v), baseFrac, sl0, ts0, tp0, cash, respectLimits)

			rows = append(
				rows,
				SweepRow{Label: fmt.Sprintf("%d", v), Baseline: v == baseVal, Result: res},
			)
		}

		axes = append(axes, AxisSweep{Axis: axis, Rows: rows})
	}
	// exits/deployment vary the backtest knobs, not the strategy config.
	addExit := func(axis string, vals []float64, baseVal float64, which int) {
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

			res := runConfig(base, data, b, baseFrac, sl, ts, tp, cash, respectLimits)

			rows = append(
				rows,
				SweepRow{Label: fmt.Sprintf("%.2f", v), Baseline: v == baseVal, Result: res},
			)
		}

		axes = append(axes, AxisSweep{Axis: axis, Rows: rows})
	}

	addStr("combine", []string{"majority", "weighted", "unanimous"}, b.Combine,
		func(v string) config.StrategyConfig { c := b; c.Combine = v; return c })
	addFloatSC("min_strength", []float64{0.10, 0.20, 0.30, 0.40}, b.MinStrength,
		func(v float64) config.StrategyConfig { c := b; c.MinStrength = v; return c })

	// capital deployment (varies order size, not strategy config).
	{
		vals := []float64{0.10, 0.25, 0.50}
		rows := make([]SweepRow, 0, len(vals))

		for _, v := range vals {
			res := runConfig(base, data, b, v, sl0, ts0, tp0, cash, respectLimits)

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

	return baseline, axes
}

// Combos returns the number of combinations the grid will evaluate.
func (g ParamGrid) Combos() int {
	n := len(g.DetectorSets) * len(g.Combine) * len(g.MinStrength) * len(g.CapitalFrac)

	n *= len(g.StopLoss) * len(g.TrailingStop) * len(g.TakeProfit)
	return n
}
