package indicators

import (
	"math"
	"testing"
)

// TestSupertrendFlipFollowsTheCurrentDirection is the regression for IN2-1,
// with the exact bar the review found (seed 33, period 7, mult 0.5, bar 118):
// close 100.56, final upper 99.15, final lower 107.17 — the bands have
// crossed — and the trend was up. Textbook Supertrend flips an up-trend only
// on a close below the LOWER band, which 100.56 < 107.17 is: the trend turns
// down. Testing "close above the upper band" first kept it up, with its
// support band above the price, and the detector voted a full-strength BUY.
func TestSupertrendFlipFollowsTheCurrentDirection(t *testing.T) {
	t.Parallel()

	if got := supertrendFlip(1, 100.56, 99.15, 107.17); got != -1 {
		t.Errorf("up-trend closing below the lower band (bands crossed): dir = %d, want -1", got)
	}

	if got := supertrendFlip(-1, 100.56, 107.17, 99.15); got != -1 {
		t.Errorf("down-trend closing between ordinary bands: dir = %d, want -1 (no flip)", got)
	}

	if got := supertrendFlip(-1, 108, 107.17, 99.15); got != 1 {
		t.Errorf("down-trend closing above the upper band: dir = %d, want 1", got)
	}
}

// TestHighLowSpreadOfNonFiniteBarsIsUnmeasured is the regression for SC2-7.
// "x <= 0" is false for NaN, so NaN highs and lows passed every guard, and the
// NaN estimate was then returned as a MEASURED 0% spread — an all-NaN series
// passed max_spread_pct as the tightest instrument there is. The earlier fix
// claimed to cover NaN; this path still failed open.
func TestHighLowSpreadOfNonFiniteBarsIsUnmeasured(t *testing.T) {
	t.Parallel()

	for _, v := range []float64{math.NaN(), math.Inf(1)} {
		h := []float64{v, v, v}
		l := []float64{v, v, v}

		if s, ok := HighLowSpread(h, l); ok {
			t.Errorf("HighLowSpread of %v bars = (%v, measured), want unmeasured", v, s)
		}
	}
}

// TestRSIOfAFlatSeriesIsNeutral is the regression for IN2-2: with no movement
// at all, both average gain and loss are 0, and the formula's 0/0 was read as
// RSI 100 — maximally overbought — so a halted or frozen instrument drew a
// full-strength SELL from the RSI detector.
func TestRSIOfAFlatSeriesIsNeutral(t *testing.T) {
	t.Parallel()

	flat := make([]float64, 40)
	for i := range flat {
		flat[i] = 100
	}

	if v, ok := RSILast(flat, 14); !ok || v != 50 {
		t.Errorf("RSI of a flat series = %v (ok=%v), want 50", v, ok)
	}
}

// TestADXLastIsDefinedAtTwicePeriod is the regression for IN2-3: the guard
// refused at n == 2·period, one bar after ADX is defined.
func TestADXLastIsDefinedAtTwicePeriod(t *testing.T) {
	t.Parallel()

	const period = 5

	n := 2 * period
	high, low, closes := make([]float64, n), make([]float64, n), make([]float64, n)

	for i := range n {
		closes[i] = 100 + float64(i)
		high[i], low[i] = closes[i]+1, closes[i]-1
	}

	if _, _, _, ok := ADXLast(high, low, closes, period); !ok {
		t.Errorf("ADXLast undefined at n = 2·period = %d", n)
	}
}
