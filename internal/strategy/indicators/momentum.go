package indicators

// StochasticLast returns the final %K and %D of the Stochastic oscillator over
// the given periods. %K = 100·(closes − lowestLow) / (highestHigh − lowestLow)
// across the last kPeriod bars; %D is the SMA of %K over the last dPeriod bars.
// Both are 0..100. ok is false with too few bars or a flat range.
//
//nolint:cyclop // %K scan + %D smoothing is one pass
func StochasticLast(high, low, closes []float64, kPeriod, dPeriod int) (k, d float64, ok bool) {
	n := len(closes)
	if kPeriod <= 0 || dPeriod <= 0 || n < kPeriod+dPeriod-1 {
		return 0, 0, false
	}

	// %K at bar index i (needs kPeriod bars of history ending at i).
	kAt := func(i int) (float64, bool) {
		lo, hi := low[i], high[i]
		for j := i - kPeriod + 1; j <= i; j++ {
			if low[j] < lo {
				lo = low[j]
			}

			if high[j] > hi {
				hi = high[j]
			}
		}

		rng := hi - lo
		if rng <= 0 {
			return 0, false
		}

		return 100 * (closes[i] - lo) / rng, true
	}
	last, ok := kAt(n - 1)

	if !ok {
		return 0, 0, false
	}

	// %D averages only the defined %K values: a flat window has no meaningful
	// position-in-range, so it is skipped rather than counted as 0 (which would
	// bias %D toward oversold). At least the final window is defined (checked).
	var sum float64

	defined := 0

	for i := n - dPeriod; i < n; i++ {
		if kv, kok := kAt(i); kok {
			sum += kv
			defined++
		}
	}

	return last, sum / float64(defined), true
}
