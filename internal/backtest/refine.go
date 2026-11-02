package backtest

import (
	"fmt"
	"io"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// Refinement is one axis explored during refinement, and what it changed.
type Refinement struct {
	Axis     string  `json:"axis"`
	From     string  `json:"from"`
	To       string  `json:"to"`
	Improved float64 `json:"improved"` // score delta; 0 when the baseline won
}

// Refine takes the winner of the main search and coordinate-descends over the
// axes the grid does not cover: the SELL side, the churn-damping behaviour, the
// volatility-adaptive exits and risk-based sizing.
//
// Why a second stage rather than more grid dimensions: the grid is already ~2000
// combinations, and each axis added multiplies it. Sweeping one axis at a time
// around a good configuration costs the sum of the axis lengths instead of their
// product — a few dozen backtests rather than tens of thousands. The trade-off is
// real: coordinate descent finds a local optimum and cannot discover that two
// parameters only work when changed together. It is the right trade here because
// these axes are close to independent of the buy-side signal.
//
// What is deliberately NOT searched:
//
//   - costs (spread, slippage, commission). These are measurements of your
//     broker and your market, not choices. An optimiser handed them would set
//     them to zero and report a better strategy, which is simply lying to
//     yourself with extra steps.
//   - protections (cooldowns, stop guard, drawdown halt). They can only ever
//     cost return in-sample, so any return-seeking search switches them off.
//     They are *applied* during every backtest instead, so a configuration the
//     live bot would halt does not look tradeable here.
//
// strategyOnly restricts the pass to the axes that live in StrategyConfig —
// the sell side and the trend gate — skipping behaviour(), adaptiveExits() and
// which tune EngineConfig/ExitsConfig fields instead. It is the
// Refine-side counterpart of Pins: a caller that can only write a per-symbol
// strategy_overrides entry (internal/autotune) cannot apply a confirm_buy,
// keep_interval or atr_stop_mult change, so letting those
// axes win here produces a trial that is then rejected wholesale for depending
// on a setting that cannot be written back. cmd/optimize passes false: it
// applies its result by hand to the shared engine:/exits: config, where every
// one of those axes IS settable. See CODE_REVIEW_FINDINGS.md's AT-11 finding.
//
// The trade-off is real and deliberate: a strategyOnly pass will not discover
// that a symbol wants a different confirm_buy, so it cannot report that to an
// operator either. Exploring it and then discarding the result was strictly
// worse — the finding was destroyed either way, and the search wasted its
// budget arriving at a proposal it could not make.
func Refine(base config.Config, data map[string][]market.Candle, best Trial,
	cash float64, minTrades int, score Scorer, respectLimits bool, progress io.Writer,
	strategyOnly bool,
) (Trial, []Refinement) {
	r := refiner{
		base: base, data: data, cash: cash, minTrades: minTrades,
		score: score, respectLimits: respectLimits, out: progress,
		best: best,
	}

	r.sellSide()

	if !strategyOnly {
		r.behaviour()
		r.adaptiveExits()
	}

	r.trendGate()

	return r.best, r.log
}

// refiner carries the state of one refinement pass.
type refiner struct {
	base          config.Config
	data          map[string][]market.Candle
	cash          float64
	minTrades     int
	score         Scorer
	respectLimits bool
	out           io.Writer

	best Trial
	log  []Refinement
}

// try evaluates one variant of the current best and adopts it when it scores
// higher. Variants that fall below the minimum trade count are ignored, exactly
// as in the main search.
func (r *refiner) try(axis, label string, mutate func(*Trial)) {
	cand := r.best
	// Deep-copy the slices a mutation might replace, so a rejected variant
	// cannot leave the incumbent altered.
	cand.Detectors = append([]string(nil), r.best.Detectors...)
	cand.Params.DetectorsSell = append([]string(nil), r.best.Params.DetectorsSell...)

	mutate(&cand)

	res := runTrialConfig(r.base, r.data, cand, r.cash, r.respectLimits)
	if res.ClosedTrades < r.minTrades {
		return
	}

	s := r.score(res)
	if s <= r.best.Score {
		return
	}

	cand.Result = res
	cand.Score = s

	r.log = append(r.log, Refinement{
		Axis: axis, From: r.describeAxis(axis), To: label, Improved: s - r.best.Score,
	})

	r.best = cand

	if r.out != nil {
		fmt.Fprintf(r.out, "  refine %-18s -> %-22s score %+.3f\n", axis, label, s)
	}
}

// describeAxis renders the incumbent's value on an axis, for the change log.
//
//nolint:cyclop // one label per axis; a lookup table would be less readable
func (r *refiner) describeAxis(axis string) string {
	p := r.best.Params

	switch axis {
	case "detectors_sell":
		if len(p.DetectorsSell) == 0 {
			return "same as buy"
		}

		return fmt.Sprint(p.DetectorsSell)

	case "combine_sell":
		if p.CombineSell == "" {
			return "same as buy"
		}

		return p.CombineSell

	case "min_strength_sell":
		return fmt.Sprintf("%.2f", p.MinStrengthSell)
	case "confirm_buy":
		return fmt.Sprint(r.best.ConfirmBuy)
	case "confirm_sell":
		return fmt.Sprint(r.best.ConfirmSell)
	case "keep_interval":
		return r.best.KeepInterval.String()
	case "atr_stop_mult":
		return fmt.Sprintf("%.1f", r.best.Exits.ATRStopMult)
	case "atr_trail_mult":
		return fmt.Sprintf("%.1f", r.best.Exits.ATRTrailMult)
	case "trend_gate":
		return boolLabel(p.TrendGate)
	}

	return ""
}

// sellSide asks the question the grid never does: are the detectors that are
// best at deciding when to BUY also the best at deciding when to SELL?
//
// There is no reason they should be. Entering wants confirmation from several
// agreeing indicators; exiting wants whichever indicator notices first that the
// move is over. A config that enters on a slow trend agreement and exits on the
// same slow agreement gives back most of the move waiting for it to turn.
func (r *refiner) sellSide() {
	r.section("sell side (detectors_sell / combine_sell / min_strength_sell)")

	// "nil" means inherit the buy set — kept as a candidate so the search can
	// conclude that sharing really is best.
	candidates := [][]string{
		nil,
		{"rsi"},
		{"macd"},
		{"supertrend"},
		{"ema_cross"},
		{"trend"},
		{"bollinger"},
		{"rsi", "macd"},
		{"macd", "supertrend"},
		{"atr", "rvol"},
	}

	for _, ds := range candidates {
		label := "same as buy"
		if len(ds) > 0 {
			label = fmt.Sprint(ds)
		}

		r.try("detectors_sell", label, func(t *Trial) {
			t.Params.DetectorsSell = append([]string(nil), ds...)
		})
	}

	for _, cm := range []string{"", "majority", "unanimous", "weighted"} {
		label := cm
		if cm == "" {
			label = "same as buy"
		}

		r.try("combine_sell", label, func(t *Trial) { t.Params.CombineSell = cm })
	}

	for _, ms := range []float64{-1, 0.05, 0.10, 0.20, 0.30, 0.40} {
		label := fmt.Sprintf("%.2f", ms)
		if ms < 0 {
			label = "inherit buy"
		}

		r.try("min_strength_sell", label, func(t *Trial) { t.Params.MinStrengthSell = ms })
	}
}

// behaviour sweeps the churn damping: how many consecutive agreeing bars are
// required before acting, and how long a position must be held before a strategy
// sell is allowed.
func (r *refiner) behaviour() {
	r.section("behaviour (confirm_buy / confirm_sell / keep_interval)")

	for _, n := range []int{1, 2, 3, 4} {
		r.try("confirm_buy", fmt.Sprint(n), func(t *Trial) { t.ConfirmBuy = n })
	}

	for _, n := range []int{1, 2, 3, 4} {
		r.try("confirm_sell", fmt.Sprint(n), func(t *Trial) { t.ConfirmSell = n })
	}

	for _, d := range []time.Duration{0, 30 * time.Minute, 2 * time.Hour, 8 * time.Hour, 24 * time.Hour} {
		r.try("keep_interval", d.String(), func(t *Trial) { t.KeepInterval = d })
	}
}

// adaptiveExits sweeps the ATR-based stops, which scale the exit distance with
// each instrument's own volatility rather than applying one percentage to all.
func (r *refiner) adaptiveExits() {
	r.section("adaptive exits (atr_stop_mult / atr_trail_mult)")

	for _, m := range []float64{0, 1.5, 2, 3, 4} {
		r.try("atr_stop_mult", fmt.Sprintf("%.1f", m), func(t *Trial) { t.Exits.ATRStopMult = m })
	}

	for _, m := range []float64{0, 2, 3, 4, 6} {
		r.try("atr_trail_mult", fmt.Sprintf("%.1f", m), func(t *Trial) { t.Exits.ATRTrailMult = m })
	}
}

// trendGate asks whether the weekly-trend directional veto is worth keeping
// for the winning buy-side config the search settled on. TrendGate (like
// TrendPeriod/TrendThreshold) is part of the base config's StrategyConfig
// and so is carried into every trial correctly — but unlike the axes above,
// nothing else in the optimizer ever varies it: the main grid has no
// trend_gate dimension, and -sensitivity's independent one-factor sweep
// (which does vary it) is a separate report, not part of this search. So a
// trial's trend_gate is otherwise pinned to whatever config.yaml says for
// the whole run, exactly like a config field the optimizer never looked at.
func (r *refiner) trendGate() {
	r.section("trend gate (trend_gate)")

	for _, on := range []bool{false, true} {
		r.try("trend_gate", boolLabel(on), func(t *Trial) { t.Params.TrendGate = on })
	}
}

// section announces a refinement stage on the progress writer.
func (r *refiner) section(name string) {
	if r.out != nil {
		fmt.Fprintf(r.out, "\nrefining %s ...\n", name)
	}
}
