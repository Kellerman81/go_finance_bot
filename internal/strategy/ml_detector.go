package strategy

import (
	"github.com/Kellerman81/go_finance_bot/internal/ai"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/ml"
)

// predictor is the slice of *ml.Predictor the detector needs, as an interface so
// tests can supply a deterministic stand-in.
type predictor interface {
	// VerdictWith returns the model's read on symbol from its candle
	// history, on features built with params.
	VerdictWith(symbol string, candles []market.Candle, params ai.Params) (ml.Verdict, bool)
	// WarmupBarsFor is the history needed before a model on params' features
	// can be fitted.
	WarmupBarsFor(params ai.Params) int
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
	// params are this strategy's indicator periods, handed to the predictor
	// with every verdict so the model's features follow the strategy it
	// votes in — the process-wide predictor's own were fixed at startup
	// (ML3-5).
	params ai.Params
}

// newMLDetector builds the ML detector, wired to the process-wide predictor
// installed by ml.Configure at startup. With none configured the detector still
// exists (so the UI can list it) and always votes HOLD.
func newMLDetector(c config.StrategyConfig) *mlDetector {
	d := &mlDetector{pred: ml.Default(), params: ml.ParamsFor(c)}

	// The warmup is the predictor's, floored at the indicator warmup so a
	// disabled predictor still reports something sane. +1 because DetectFor
	// always drops the newest candle before predicting (see its doc
	// comment), so it needs one more bar of input than the predictor itself
	// requires.
	d.warmup = max(d.pred.WarmupBarsFor(d.params), c.SlowMA+c.MACDSignal+2) + 1

	return d
}

// UsePredictor points every "ml" detector in s — buy, sell and
// higher-timeframe sets — at p. A nil p, or a strategy that is not a
// *Combined, is left alone.
func UsePredictor(s Strategy, p *ml.Predictor) {
	c, ok := s.(*Combined)
	if !ok || p == nil {
		return
	}

	rebind := func(dets []Detector) {
		for _, d := range dets {
			if md, ok := d.(*mlDetector); ok {
				md.pred = p
			}
		}
	}

	rebind(c.detectors)
	rebind(c.sellDetectors)

	if c.htf != nil {
		rebind(c.htf.detectors)
	}
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

	// The predictor needs candles, WITH their times: the features and labels
	// work on bar index, but the retrain schedule counts bars newer than the
	// last fit by Time. Rebuilt from the columns alone every Time was zero, so
	// that count was always 0 and a model, once fitted, was never refitted —
	// retrain_every did nothing, live or in backtest, and a symbol whose first
	// fit failed never got a model at all (ML2-1, CRIT-8 still open).
	candles := candlesFrom(symbol, s)
	if len(candles) < d.warmup {
		return Hold, 0, "ml: not enough history to train"
	}

	candles = candles[:len(candles)-1]

	v, ok := d.pred.VerdictWith(symbol, candles, d.params)
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

// candlesFrom returns the series' candles as a copy the predictor may keep:
// the source slice when the series carries one (bar times included), else a
// reconstruction from the OHLCV columns.
func candlesFrom(symbol string, s series) []market.Candle {
	out := make([]market.Candle, len(s.close))
	for i := range s.close {
		out[i] = market.Candle{
			Symbol: symbol,
			Open:   s.open[i], High: s.high[i], Low: s.low[i],
			Close: s.close[i], Volume: s.volume[i],
		}

		if i < len(s.src) {
			out[i].Time = s.src[i].Time
		}
	}

	return out
}
