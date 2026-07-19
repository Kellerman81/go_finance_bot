// Package indicators provides pure technical-analysis functions over price
// series. Each function returns a slice aligned to the input length, with NaN
// filling the warmup region where the indicator is not yet defined.
package indicators

import "math"

// SMA returns the simple moving average over the given period.
func SMA(values []float64, period int) []float64 {
	out := make([]float64, len(values))
	if period <= 0 {
		return fillNaN(out)
	}

	var sum float64

	for i := range values {
		sum += values[i]
		if i >= period {
			sum -= values[i-period]
		}

		if i >= period-1 {
			out[i] = sum / float64(period)
		} else {
			out[i] = math.NaN()
		}
	}

	return out
}

// EMA returns the exponential moving average over the given period. The series
// is seeded with the SMA of the first `period` values.
//
// Note on windowing: because the seed is the SMA of the first `period` values of
// the slice it is given, the final EMA value depends on where the slice starts.
// Two callers that pass different-length windows of the same underlying series
// get slightly different last values until the seed's influence decays (~3–4
// periods). Backtests must therefore size their per-bar window with enough
// headroom over the EMA period (see backtest lookback) for windowed results to
// match a live series computed over full history.
func EMA(values []float64, period int) []float64 {
	out := make([]float64, len(values))
	if period <= 0 || len(values) < period {
		return fillNaN(out)
	}

	k := 2.0 / float64(period+1)

	var seed float64

	for i := range period {
		seed += values[i]

		out[i] = math.NaN()
	}

	prev := seed / float64(period)

	out[period-1] = prev

	for i := period; i < len(values); i++ {
		prev = (values[i]-prev)*k + prev
		out[i] = prev
	}

	return out
}

// EMALast returns only the final EMA value, matching Last(EMA(values, period))
// but without allocating the full output slice. ok is false when there are too
// few values for the indicator to be defined.
func EMALast(values []float64, period int) (float64, bool) {
	if period <= 0 || len(values) < period {
		return 0, false
	}

	k := 2.0 / float64(period+1)

	var seed float64

	for i := range period {
		seed += values[i]
	}

	prev := seed / float64(period)
	for i := period; i < len(values); i++ {
		prev = (values[i]-prev)*k + prev
	}

	return prev, true
}

// RSI returns the Relative Strength Index using Wilder's smoothing.
func RSI(values []float64, period int) []float64 {
	out := fillNaN(make([]float64, len(values)))
	if period <= 0 || len(values) <= period {
		return out
	}

	var gain, loss float64

	for i := 1; i <= period; i++ {
		ch := values[i] - values[i-1]
		if ch >= 0 {
			gain += ch
		} else {
			loss -= ch
		}
	}

	avgGain := gain / float64(period)
	avgLoss := loss / float64(period)

	out[period] = rsiFrom(avgGain, avgLoss)

	for i := period + 1; i < len(values); i++ {
		ch := values[i] - values[i-1]
		g, l := 0.0, 0.0

		if ch >= 0 {
			g = ch
		} else {
			l = -ch
		}

		avgGain = (avgGain*float64(period-1) + g) / float64(period)
		avgLoss = (avgLoss*float64(period-1) + l) / float64(period)
		out[i] = rsiFrom(avgGain, avgLoss)
	}

	return out
}

// RSILast returns only the final RSI value, matching Last(RSI(values, period))
// without allocating the full output slice.
func RSILast(values []float64, period int) (float64, bool) {
	if period <= 0 || len(values) <= period {
		return 0, false
	}

	var gain, loss float64

	for i := 1; i <= period; i++ {
		ch := values[i] - values[i-1]
		if ch >= 0 {
			gain += ch
		} else {
			loss -= ch
		}
	}

	avgGain := gain / float64(period)
	avgLoss := loss / float64(period)

	for i := period + 1; i < len(values); i++ {
		ch := values[i] - values[i-1]
		g, l := 0.0, 0.0

		if ch >= 0 {
			g = ch
		} else {
			l = -ch
		}

		avgGain = (avgGain*float64(period-1) + g) / float64(period)
		avgLoss = (avgLoss*float64(period-1) + l) / float64(period)
	}

	return rsiFrom(avgGain, avgLoss), true
}

// rsiFrom converts average gain/loss into the 0–100 RSI value.
func rsiFrom(avgGain, avgLoss float64) float64 {
	if avgLoss == 0 {
		return 100
	}

	rs := avgGain / avgLoss

	return 100 - (100 / (1 + rs))
}

// MACDResult holds the three MACD series, each aligned to the input length.
type MACDResult struct {
	MACD      []float64 // fast EMA - slow EMA
	Signal    []float64 // EMA of MACD
	Histogram []float64 // MACD - Signal
}

// MACD computes the Moving Average Convergence Divergence.
func MACD(values []float64, fast, slow, signal int) MACDResult {
	emaFast := EMA(values, fast)
	emaSlow := EMA(values, slow)
	macd := make([]float64, len(values))

	for i := range values {
		if math.IsNaN(emaFast[i]) || math.IsNaN(emaSlow[i]) {
			macd[i] = math.NaN()
		} else {
			macd[i] = emaFast[i] - emaSlow[i]
		}
	}

	// Signal line is the EMA of the defined portion of the MACD series.
	start := firstDefined(macd)
	sig := fillNaN(make([]float64, len(values)))

	if start >= 0 && len(macd)-start >= signal {
		defined := macd[start:]
		sigDefined := EMA(defined, signal)
		copy(sig[start:], sigDefined)
	}

	hist := make([]float64, len(values))
	for i := range values {
		if math.IsNaN(macd[i]) || math.IsNaN(sig[i]) {
			hist[i] = math.NaN()
		} else {
			hist[i] = macd[i] - sig[i]
		}
	}

	return MACDResult{MACD: macd, Signal: sig, Histogram: hist}
}

// MACDLast returns only the final MACD, signal and histogram values, matching
// Last() of each MACD(...) series but in O(1) space — no full-series
// allocations, which matters in the backtest/optimizer hot loop where the MACD
// detector runs once per bar per symbol. ok is false while the histogram is
// undefined (fewer than max(fast,slow)+signal-1 values).
//
//nolint:revive,cyclop // four results are clearer than a struct; the EMA seeding is one pass
func MACDLast(values []float64, fast, slow, signal int) (macd, sig, hist float64, ok bool) {
	n := len(values)

	if fast <= 0 || slow <= 0 || signal <= 0 {
		return 0, 0, 0, false
	}

	start := slow // first index where both EMAs (and thus MACD) are defined
	if fast > slow {
		start = fast
	}

	start--
	if n < start+signal {
		return 0, 0, 0, false
	}

	kf := 2.0 / float64(fast+1)
	ks := 2.0 / float64(slow+1)
	kg := 2.0 / float64(signal+1)

	var (
		fSum, sSum, fPrev, sPrev float64
		seedSum, sigPrev         float64
	)

	seedCount := 0
	sigDefined := false

	for i := range n {
		v := values[i]
		if i < fast {
			fSum += v
			if i == fast-1 {
				fPrev = fSum / float64(fast)
			}
		} else {
			fPrev = (v-fPrev)*kf + fPrev
		}

		if i < slow {
			sSum += v
			if i == slow-1 {
				sPrev = sSum / float64(slow)
			}
		} else {
			sPrev = (v-sPrev)*ks + sPrev
		}

		if i < start {
			continue
		}

		macd = fPrev - sPrev
		if !sigDefined {
			// Seed the signal EMA with the SMA of the first `signal` MACD values,
			// exactly as MACD() seeds EMA(macd[start:], signal).
			seedSum += macd
			seedCount++

			if seedCount == signal {
				sigPrev = seedSum / float64(signal)
				sigDefined = true
			}

			continue
		}

		sigPrev = (macd-sigPrev)*kg + sigPrev
	}

	if !sigDefined {
		return 0, 0, 0, false
	}

	return macd, sigPrev, macd - sigPrev, true
}

// firstDefined returns the index of the first non-NaN value, or -1.
func firstDefined(v []float64) int {
	for i := range v {
		if !math.IsNaN(v[i]) {
			return i
		}
	}

	return -1
}

// fillNaN sets every element to NaN and returns the slice.
func fillNaN(v []float64) []float64 {
	for i := range v {
		v[i] = math.NaN()
	}

	return v
}

// Last returns the last non-NaN value and true, or 0 and false if none exists.
func Last(v []float64) (float64, bool) {
	for i := len(v) - 1; i >= 0; i-- {
		if !math.IsNaN(v[i]) {
			return v[i], true
		}
	}

	return 0, false
}
