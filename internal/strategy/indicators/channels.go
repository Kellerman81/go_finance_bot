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
// band ratchets in the trend's favour. ok is false before ATR is defined.
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

	atr := ATR(high, low, closes, period)
	// Walk forward from the first defined ATR bar, carrying the final bands and
	// direction per the standard Supertrend rules.
	var finalUpper, finalLower float64

	dir = 1

	started := false

	for i := period; i < n; i++ {
		if math.IsNaN(atr[i]) {
			continue
		}

		mid := (high[i] + low[i]) / 2
		basicUpper := mid + mult*atr[i]
		basicLower := mid - mult*atr[i]

		if !started {
			finalUpper, finalLower = basicUpper, basicLower
			dir = 1
			started = true
			continue
		}

		if basicUpper < finalUpper || closes[i-1] > finalUpper {
			finalUpper = basicUpper
		}

		if basicLower > finalLower || closes[i-1] < finalLower {
			finalLower = basicLower
		}

		switch {
		case closes[i] > finalUpper:
			dir = 1
		case closes[i] < finalLower:
			dir = -1
		}
	}

	if !started {
		return 0, 0, false
	}

	if dir >= 0 {
		return 1, finalLower, true
	}

	return -1, finalUpper, true
}
