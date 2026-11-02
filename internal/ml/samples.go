package ml

import (
	"github.com/Kellerman81/go_finance_bot/internal/ai"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// BuildSamples turns candles into labelled training rows.
//
// The label is the forward return: row i is positive when the close `horizon`
// bars later is at least `threshold` above the close at bar i. That is a
// deliberate use of future data — supervised learning has no other kind of label
// — and it is exactly where a lookahead bug lives, so the discipline is explicit:
//
//   - A row is only emitted when its whole forward window exists. The last
//     `horizon` bars of the input produce no rows, because their labels are not
//     knowable yet.
//   - The features for row i are computed from candles[0..i] only. Nothing after
//     bar i touches the input side.
//   - The caller must therefore pass only the history available at decision
//     time. Passing the full dataset and predicting inside it would train on the
//     future and score against it, which reports an accuracy no live bot can
//     reproduce. Predictor enforces the split; direct callers must too.
//
// The threshold matters more than it looks. With threshold 0 the label is "did
// price tick up", which after costs is not a tradeable question and produces a
// near-50/50 target dominated by noise. A threshold around one ATR of typical
// movement asks something the strategy could actually act on.
func BuildSamples(
	candles []market.Candle,
	p ai.Params,
	horizon int,
	threshold float64,
) []Sample {
	if horizon < 1 || len(candles) <= horizon+1 {
		return nil
	}

	return buildSamples(candles, 0, p, horizon, threshold)
}

// buildSamples is BuildSamples emitting rows from index from on (and never
// before the feature warmup), with every row's features still computed over
// all of candles[0..i] — so a caller can hand in history before the rows it
// wants labelled and have the features read it (see Predictor.train).
func buildSamples(
	candles []market.Candle,
	from int,
	p ai.Params,
	horizon int,
	threshold float64,
) []Sample {
	if horizon < 1 || len(candles) <= horizon+1 {
		return nil
	}

	p = p.WithDefaults()

	// Features need enough history to be defined; labels need enough future.
	first := max(p.WarmupBars(), from)
	last := len(candles) - horizon - 1

	if first > last {
		return nil
	}

	high, low, closes, volume := columns(candles)
	out := make([]Sample, 0, last-first+1)

	for i := first; i <= last; i++ {
		// Features from history up to and including bar i — never beyond.
		f, ok := ai.BuildFeatures("", high[:i+1], low[:i+1], closes[:i+1], volume[:i+1], p)
		if !ok || !f.Defined() {
			continue
		}

		entry := closes[i]
		exit := closes[i+horizon]

		if entry <= 0 {
			continue
		}

		label := 0.0
		if exit/entry-1 >= threshold {
			label = 1
		}

		out = append(out, Sample{X: f.Vector(), Y: label})
	}

	return out
}

// FeaturesAt builds the prediction input for the most recent bar of candles, using
// only that history. It is the inference-time counterpart of BuildSamples: same
// feature computation, no label, no future.
func FeaturesAt(candles []market.Candle, symbol string, p ai.Params) (ai.Features, bool) {
	if len(candles) == 0 {
		return ai.Features{}, false
	}

	p = p.WithDefaults()

	high, low, closes, volume := columns(candles)

	f, ok := ai.BuildFeatures(symbol, high, low, closes, volume, p)
	if !ok || !f.Defined() {
		return ai.Features{}, false
	}

	return f, true
}

// columns splits candles into the aligned series the indicators take, from one
// backing allocation.
//
//nolint:revive // four aligned OHLCV columns; a struct would only rename them
func columns(candles []market.Candle) (high, low, closes, volume []float64) {
	n := len(candles)
	buf := make([]float64, 4*n)

	high, low, closes, volume = buf[0:n:n], buf[n:2*n:2*n], buf[2*n:3*n:3*n], buf[3*n:4*n:4*n]

	for i, c := range candles {
		high[i], low[i], closes[i], volume[i] = c.High, c.Low, c.Close, c.Volume
	}

	return high, low, closes, volume
}
