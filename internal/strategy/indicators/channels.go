package indicators

import "math"

// DonchianLast returns the upper and lower Donchian channel bounds computed over
// the `period` bars ending one bar before the last (the *prior* channel), so the
// current bar breaking above upper / below lower is a fresh breakout. ok is false
// with too few bars.
func DonchianLast(high, low []float64, period int) (upper, lower float64, ok bool) {
	n := len(high)
	if period <= 0 || n < period+1 {
		return 0, 0, false
	}

	upper, lower = math.Inf(-1), math.Inf(1)
	for i := n - 1 - period; i < n-1; i++ {
		if high[i] > upper {
			upper = high[i]
		}

		if low[i] < lower {
			lower = low[i]
		}
	}

	return upper, lower, true
}

// SupertrendLast returns the final Supertrend direction (+1 up-trend, -1
// down-trend) and band level over the given ATR period and multiplier. The
// Supertrend flips when price closes through the opposing band; between flips the
// band ratchets in the trend's favour. ok is false before ATR is defined, and
// also — see below — when the window never saw a real band crossover, or a
// NaN ATR reading poisoned the ratchet partway through.
//
// The ATR is advanced inside the walk rather than materialised first. Both are
// forward recurrences over the same bars, so one pass computes both — and the
// two full-length float64 slices the separate ATR() call used to allocate were,
// on a backtest of any size, the largest single source of garbage in the
// strategy: one per bar, per symbol, discarded immediately.
//
//nolint:cyclop // the band-ratchet walk is one cohesive algorithm
func SupertrendLast(
	high, low, closes []float64,
	period int,
	mult float64,
) (dir int, level float64, ok bool) {
	n := len(closes)
	if period <= 0 || n <= period {
		return 0, 0, false
	}

	// Seed the ATR exactly as ATR() does: the simple mean of the first `period`
	// true ranges, which is the value it reports at bar `period`.
	var sum float64

	for i := 1; i <= period; i++ {
		sum += trueRange(high, low, closes, i)
	}

	atr := sum / float64(period)
	if math.IsNaN(atr) {
		return 0, 0, false
	}

	// Bar `period` is the first with a defined ATR, so it seeds the bands.
	mid := (high[period] + low[period]) / 2
	finalUpper, finalLower := mid+mult*atr, mid-mult*atr

	// dir starts undefined (0), not a default up-trend: without an actual
	// close through one of the bands, there is no directional signal at
	// all, and defaulting to "up" produced a confident BUY vote from a
	// detector on a series that never crossed a band in either direction.
	// See CODE_REVIEW_FINDINGS.md's supertrendDetector-never-holds finding.
	dir = 0

	for i := period + 1; i < n; i++ {
		atr = (atr*float64(period-1) + trueRange(high, low, closes, i)) / float64(period)
		if math.IsNaN(atr) {
			// NaN is sticky through this recursion (atr feeds its own next
			// value), so one bad true-range reading poisons every ATR value
			// for the rest of the series — silently `continue`-ing here
			// used to freeze the bands (and dir) at their last valid state
			// while still reporting ok: true, indistinguishable from a
			// genuinely current reading. Report undefined instead of a
			// stale ratchet. See CODE_REVIEW_FINDINGS.md's
			// SupertrendLast-NaN-freeze finding.
			return 0, 0, false
		}

		mid = (high[i] + low[i]) / 2

		basicUpper := mid + mult*atr
		basicLower := mid - mult*atr

		if basicUpper < finalUpper || closes[i-1] > finalUpper {
			finalUpper = basicUpper
		}

		if basicLower > finalLower || closes[i-1] < finalLower {
			finalLower = basicLower
		}

		dir = supertrendFlip(dir, closes[i], finalUpper, finalLower)
	}

	switch {
	case dir > 0:
		return 1, finalLower, true
	case dir < 0:
		return -1, finalUpper, true
	default:
		// Never crossed either band within the window: no directional
		// signal, not a default up-trend one.
		return 0, 0, false
	}
}

// supertrendFlip is the textbook Supertrend state transition: an up-trend
// ends only on a close below the lower band, a down-trend only on a close
// above the upper one, and an undefined direction takes whichever band is
// crossed.
//
// Testing "above the upper band" first regardless of direction disagreed with
// that once the bands had crossed (after a very wide bar): a close between
// them kept an up-trend whose support band sat ABOVE the price, which the
// detector reported as a full-strength BUY. 81/55/20 of 110,929 bars at
// multipliers 0.5/1/1.5 — the last in the optimizer's grid (IN2-1).
func supertrendFlip(dir int, closeNow, finalUpper, finalLower float64) int {
	switch dir {
	case 1:
		if closeNow < finalLower {
			return -1
		}

	case -1:
		if closeNow > finalUpper {
			return 1
		}

	default:
		switch {
		case closeNow > finalUpper:
			return 1
		case closeNow < finalLower:
			return -1
		}
	}

	return dir
}
