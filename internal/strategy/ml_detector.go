package strategy

import (
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/ml"
)

// predictor is the slice of *ml.Predictor the detector needs, as an interface so
// tests can supply a deterministic stand-in.
type predictor interface {
	// Verdict returns the model's read on symbol from its candle history.
	Verdict(symbol string, candles []market.Candle) (ml.Verdict, bool)
	// WarmupBars is the history needed before a model can be fitted.
	WarmupBars() int
	// Horizon is the forward window the model predicts over.
	Horizon() int
}

// mlDetector votes with a learned classifier's estimate of whether the next
// `horizon` bars are up.
//
// It is a detector like any other — weightable, mutable, subject to
// min_strength — with the same fail-safe posture as the LLM: no model, too little
// history, or a model with no measurable skill all yield HOLD.
//
// Unlike every other detector it needs the *candles*, not just the extracted
// series, because it retrains from history. That is why it takes the
// symbol-aware path and keeps its own copy of the bars.
type mlDetector struct {
	pred   predictor
	warmup int
}

// newMLDetector builds the ML detector, wired to the process-wide predictor
// installed by ml.Configure at startup. With none configured the detector still
// exists (so the UI can list it) and always votes HOLD.
func newMLDetector(c config.StrategyConfig) *mlDetector {
	d := &mlDetector{pred: ml.Default()}

	// The warmup is the predictor's, floored at the indicator warmup so a
	// disabled predictor still reports something sane. +1 because DetectFor
	// always drops the newest candle before predicting (see its doc
	// comment), so it needs one more bar of input than the predictor itself
	// requires.
	d.warmup = max(d.pred.WarmupBars(), c.SlowMA+c.MACDSignal+2) + 1

	return d
}

// Name returns the canonical detector name.
func (*mlDetector) Name() string { return "ml" }

// WarmupBars reports the bars needed before the model can be fitted and used.
// It is much larger than a technical detector's: a training window plus the
// forward horizon whose labels cannot be known yet.
func (d *mlDetector) WarmupBars() int { return d.warmup }

// MaxLookback reports the history the detector uses, so the backtester sizes its
// per-bar window to cover the whole training window rather than silently
// truncating it.
func (d *mlDetector) MaxLookback() int { return d.warmup }

// Detect without a symbol cannot address the per-symbol model, and the plain
// series carries no candle timestamps to train from. Every real evaluation path
// goes through DetectFor.
func (*mlDetector) Detect(series) (Action, float64, string) {
	return Hold, 0, "ml: no symbol context"
}

// DetectFor votes with the model's prediction for symbol.
//
// It always drops the newest candle before predicting. The series a live
// evaluation is handed always ends in the current, still-accumulating bar
// (see engine's barStore.tail), but every training row in ml.BuildSamples is
// built exclusively from bars that had already closed — handing the model a
// partial last bar here evaluates it on a systematically different feature
// distribution than the one it was calibrated against (rvol depressed by
// partial volume, ret_1/bb_pos biased toward a still-moving close). Dropping
// it unconditionally, in backtest too (where the "newest" candle is always
// already complete), keeps live and backtest consistent with each other:
// backtest simply evaluates one bar "as of" earlier than it otherwise would
// — a small, uniform staleness, not a distribution mismatch. See
// CODE_REVIEW_FINDINGS.md's "Predictions computed on the in-progress bar".
func (d *mlDetector) DetectFor(symbol string, s series) (Action, float64, string) {
	if d.pred == nil || d.pred.Horizon() == 0 {
		return Hold, 0, "ml: predictor disabled"
	}

	// The predictor needs candles; rebuild them from the series columns. The
	// times are not used by the feature or label logic (both work on bar index),
	// so the reconstruction is faithful for this purpose.
	candles := candlesFrom(symbol, s)
	if len(candles) < d.warmup {
		return Hold, 0, "ml: not enough history to train"
	}

	candles = candles[:len(candles)-1]

	v, ok := d.pred.Verdict(symbol, candles)
	if !ok {
		return Hold, 0, "ml: no model yet"
	}

	switch v.Action {
	case ml.Buy:
		return Buy, clamp01(v.Strength), v.Reason
	case ml.Sell:
		return Sell, clamp01(v.Strength), v.Reason
	case ml.Hold:
	}

	return Hold, 0, v.Reason
}

// candlesFrom rebuilds candles from the extracted OHLCV columns.
func candlesFrom(symbol string, s series) []market.Candle {
	out := make([]market.Candle, len(s.close))
	for i := range s.close {
		out[i] = market.Candle{
			Symbol: symbol,
			Open:   s.open[i], High: s.high[i], Low: s.low[i],
			Close: s.close[i], Volume: s.volume[i],
		}
	}

	return out
}
