package indicators

import "math"

// VWAP returns the rolling Volume-Weighted Average Price over the given period
// window, using the typical price (H+L+C)/3 for each bar.
func VWAP(high, low, closes, volume []float64, period int) []float64 {
	n := len(closes)
	out := fillNaN(make([]float64, n))

	if period <= 0 || n < period {
		return out
	}

	tp := make([]float64, n)
	for i := range n {
		tp[i] = (high[i] + low[i] + closes[i]) / 3
	}

	for i := period - 1; i < n; i++ {
		var pv, vol float64

		for j := i - period + 1; j <= i; j++ {
			pv += tp[j] * volume[j]
			vol += volume[j]
		}

		if vol > 0 {
			out[i] = pv / vol
		}
	}

	return out
}

// VWAPLast returns only the final rolling-VWAP value, matching Last(VWAP(...))
// in O(period) instead of O(n·period).
func VWAPLast(high, low, closes, volume []float64, period int) (float64, bool) {
	n := len(closes)
	if period <= 0 || n < period {
		return 0, false
	}

	var pv, vol float64

	for j := n - period; j < n; j++ {
		tp := (high[j] + low[j] + closes[j]) / 3

		pv += tp * volume[j]
		vol += volume[j]
	}

	if vol <= 0 {
		return 0, false
	}

	return pv / vol, true
}

// RVOLLast returns only the final relative-volume value, matching Last(RVOL(...))
// in O(period) instead of O(n·period).
func RVOLLast(volume []float64, period int) (float64, bool) {
	n := len(volume)
	if period <= 0 || n <= period {
		return 0, false
	}

	var sum float64

	for j := n - 1 - period; j < n-1; j++ {
		sum += volume[j]
	}

	avg := sum / float64(period)
	if avg <= 0 {
		return 0, false
	}

	return volume[n-1] / avg, true
}

// RVOL returns Relative Volume: each bar's volume divided by the average volume
// over the trailing `period` bars (excluding the current bar).
func RVOL(volume []float64, period int) []float64 {
	n := len(volume)
	out := fillNaN(make([]float64, n))

	if period <= 0 || n <= period {
		return out
	}

	for i := period; i < n; i++ {
		var sum float64

		for j := i - period; j < i; j++ {
			sum += volume[j]
		}

		avg := sum / float64(period)
		if avg > 0 {
			out[i] = volume[i] / avg
		}
	}

	return out
}

// VolumeProfile summarises traded volume by price level over a window.
type VolumeProfile struct {
	POC           float64 // price level with the most volume (point of control)
	ValueAreaHigh float64 // upper bound of the value area
	ValueAreaLow  float64 // lower bound of the value area
}

// BuildVolumeProfile buckets the close prices of the last `window` bars into
// `buckets` price bins weighted by volume, then derives the point of control and
// the value area covering `valueAreaPct` (0..1) of total volume around the POC.
//
//nolint:cyclop // bucketing, POC and value-area growth are one cohesive computation
func BuildVolumeProfile(
	closes, volume []float64,
	window, buckets int,
	valueAreaPct float64,
) (VolumeProfile, bool) {
	n := len(closes)
	if window <= 0 || window > n {
		window = n
	}

	if buckets < 2 || window < 2 {
		return VolumeProfile{}, false
	}

	start := n - window
	lo, hi := math.Inf(1), math.Inf(-1)

	for i := start; i < n; i++ {
		lo = math.Min(lo, closes[i])
		hi = math.Max(hi, closes[i])
	}

	if !(hi > lo) {
		return VolumeProfile{}, false
	}

	width := (hi - lo) / float64(buckets)

	vol := make([]float64, buckets)

	var total float64

	for i := start; i < n; i++ {
		b := int((closes[i] - lo) / width)
		if b >= buckets {
			b = buckets - 1
		}

		vol[b] += volume[i]
		total += volume[i]
	}

	if total <= 0 {
		return VolumeProfile{}, false
	}

	// Point of control = densest bucket.
	pocIdx := 0
	for b := 1; b < buckets; b++ {
		if vol[b] > vol[pocIdx] {
			pocIdx = b
		}
	}

	priceOf := func(b int) float64 { return lo + (float64(b)+0.5)*width }

	// Grow the value area outward from the POC until it covers valueAreaPct.
	if valueAreaPct <= 0 || valueAreaPct > 1 {
		valueAreaPct = 0.7
	}

	target := total * valueAreaPct
	covered := vol[pocIdx]
	low, high := pocIdx, pocIdx

	for covered < target && (low > 0 || high < buckets-1) {
		below := 0.0
		if low > 0 {
			below = vol[low-1]
		}

		above := 0.0
		if high < buckets-1 {
			above = vol[high+1]
		}

		// Take the denser neighbour; when the bottom is exhausted the loop guard
		// guarantees the top can still grow, so growing upward is always valid.
		if (above >= below && high < buckets-1) || low <= 0 {
			high++

			covered += vol[high]
		} else {
			low--

			covered += vol[low]
		}
	}

	return VolumeProfile{
		POC:           priceOf(pocIdx),
		ValueAreaHigh: priceOf(high),
		ValueAreaLow:  priceOf(low),
	}, true
}
