package indicators

import "math"

// ATR returns the Average True Range over the given period (Wilder's smoothing),
// a volatility measure. Inputs are aligned high/low/close series.
func ATR(high, low, closes []float64, period int) []float64 {
	n := len(closes)
	out := fillNaN(make([]float64, n))

	if period <= 0 || n <= period {
		return out
	}

	tr := make([]float64, n)

	tr[0] = high[0] - low[0]

	for i := 1; i < n; i++ {
		hl := high[i] - low[i]
		hc := math.Abs(high[i] - closes[i-1])
		lc := math.Abs(low[i] - closes[i-1])

		tr[i] = math.Max(hl, math.Max(hc, lc))
	}

	// Seed with the simple average of the first `period` true ranges.
	var sum float64

	for i := 1; i <= period; i++ {
		sum += tr[i]
	}

	prev := sum / float64(period)

	out[period] = prev

	for i := period + 1; i < n; i++ {
		prev = (prev*float64(period-1) + tr[i]) / float64(period)
		out[i] = prev
	}

	return out
}

// ATRLast returns only the final ATR value, matching Last(ATR(...)) without
// allocating the true-range or output slices. ATR is defined recursively from a
// seed at index `period`, so this still scans the whole series, but in O(1)
// space.
func ATRLast(high, low, closes []float64, period int) (float64, bool) {
	n := len(closes)
	if period <= 0 || n <= period {
		return 0, false
	}

	trAt := func(i int) float64 {
		hl := high[i] - low[i]
		hc := math.Abs(high[i] - closes[i-1])
		lc := math.Abs(low[i] - closes[i-1])
		return math.Max(hl, math.Max(hc, lc))
	}

	var sum float64

	for i := 1; i <= period; i++ {
		sum += trAt(i)
	}

	prev := sum / float64(period)
	for i := period + 1; i < n; i++ {
		prev = (prev*float64(period-1) + trAt(i)) / float64(period)
	}

	return prev, true
}

// BollingerLast returns only the final middle/upper/lower band values, matching
// Last() of each BollingerBands series but in O(period) instead of O(n·period).
//
//nolint:revive // four results (mid/upper/lower/ok) are clearer than a struct here
func BollingerLast(values []float64, period int, k float64) (mid, upper, lower float64, ok bool) {
	n := len(values)
	if period <= 0 || n < period {
		return 0, 0, 0, false
	}

	var sum float64

	for i := n - period; i < n; i++ {
		sum += values[i]
	}

	mid = sum / float64(period)

	var sumSq float64

	for i := n - period; i < n; i++ {
		d := values[i] - mid

		sumSq += d * d
	}

	sd := math.Sqrt(sumSq / float64(period))

	return mid, mid + k*sd, mid - k*sd, true
}

// BollingerBands returns the middle (SMA), upper and lower bands for the given
// period and standard-deviation multiplier.
func BollingerBands(values []float64, period int, k float64) (mid, upper, lower []float64) {
	n := len(values)

	mid = SMA(values, period)
	upper = fillNaN(make([]float64, n))
	lower = fillNaN(make([]float64, n))

	if period <= 0 || n < period {
		return mid, upper, lower
	}

	for i := period - 1; i < n; i++ {
		m := mid[i]

		var sumSq float64

		for j := i - period + 1; j <= i; j++ {
			d := values[j] - m

			sumSq += d * d
		}

		sd := math.Sqrt(sumSq / float64(period))

		upper[i] = m + k*sd
		lower[i] = m - k*sd
	}

	return mid, upper, lower
}
