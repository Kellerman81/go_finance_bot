package indicators

import "math"

// trueRange returns the true range at bar i: the greatest of the bar's own
// range and its two gaps against the previous close. i must be >= 1.
//
// It is a function rather than three inlined copies because ATR, ATRLast and
// SupertrendLast each need exactly this and had drifted into carrying their own.
func trueRange(high, low, closes []float64, i int) float64 {
	hl := high[i] - low[i]
	hc := math.Abs(high[i] - closes[i-1])
	lc := math.Abs(low[i] - closes[i-1])

	return math.Max(hl, math.Max(hc, lc))
}

// ATR returns the Average True Range over the given period (Wilder's smoothing),
// a volatility measure. Inputs are aligned high/low/close series.
func ATR(high, low, closes []float64, period int) []float64 {
	n := len(closes)
	out := fillNaN(make([]float64, n))

	if period <= 0 || n <= period {
		return out
	}

	// Seed with the simple average of the first `period` true ranges.
	var sum float64

	for i := 1; i <= period; i++ {
		sum += trueRange(high, low, closes, i)
	}

	prev := sum / float64(period)

	out[period] = prev

	for i := period + 1; i < n; i++ {
		prev = (prev*float64(period-1) + trueRange(high, low, closes, i)) / float64(period)
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

	var sum float64

	for i := 1; i <= period; i++ {
		sum += trueRange(high, low, closes, i)
	}

	prev := sum / float64(period)
	for i := period + 1; i < n; i++ {
		prev = (prev*float64(period-1) + trueRange(high, low, closes, i)) / float64(period)
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

// csDenom is the constant 3 − 2√2 from Corwin & Schultz (2012); it appears twice
// in the α expression and is worth naming rather than repeating.
var csDenom = 3 - 2*math.Sqrt2

// HighLowSpread estimates the effective bid-ask spread as a fraction of price
// from consecutive high/low pairs, using the Corwin–Schultz (2012) estimator.
//
// It exists because the spread is a real trading cost that no free candle feed
// reports. A single configured spread_pct has to stand in for every instrument,
// and on a mixed book — a megacap and a thinly-traded European ETF listing — one
// number is wrong for both by a wide margin. The estimator separates the two
// sources of a bar's high-low range: volatility, which grows with the square
// root of time, and the spread, which does not. Comparing one bar's range with
// the range over two bars therefore isolates the spread.
//
// Daily bars are what the method was derived and validated on. It is usable on
// coarser intraday bars and increasingly noisy as they get shorter, so callers
// should feed it the longest bars they have. Negative single-pair estimates
// (which volatility noise produces regularly) are treated as zero rather than
// allowed to cancel real ones, as the authors recommend. ok is false when there
// is not enough usable data.
func HighLowSpread(high, low []float64) (float64, bool) {
	n := min(len(high), len(low))
	if n < 2 {
		return 0, false
	}

	var (
		sum float64
		got int
	)

	for i := 1; i < n; i++ {
		s, ok := csPair(high[i-1], low[i-1], high[i], low[i])
		if !ok {
			continue
		}

		sum += s
		got++
	}

	if got == 0 {
		return 0, false
	}

	return sum / float64(got), true
}

// csPair returns the Corwin–Schultz spread estimate for one pair of consecutive
// bars, clamped at zero.
func csPair(h1, l1, h2, l2 float64) (float64, bool) {
	if h1 <= 0 || l1 <= 0 || h2 <= 0 || l2 <= 0 || h1 < l1 || h2 < l2 {
		return 0, false
	}

	// A genuine gap — bar 2 trades entirely above bar 1's high, or entirely
	// below bar 1's low — leaves the two bars with no overlapping price
	// range at all. The Corwin-Schultz estimator only detects a *tight*
	// spread by finding the two-bar joint range SMALLER than the sum of the
	// bars' own ranges — i.e. by the bars bouncing back into shared
	// territory. Two non-overlapping bars can never do that: their joint
	// range is at best (if pushed together with the gap discarded) exactly
	// the sum of their own log-ranges, which makes the joint range's square
	// (gamma) provably >= the sum of their squares (beta) whenever either
	// bar has nonzero range — so alpha, and the resulting spread, can never
	// come out positive for a genuinely gapped pair, no matter how the
	// gap itself is sized or adjusted for. Before this fix such a pair still
	// went through the formula and was clamped to a *measured* 0% spread by
	// the negative-alpha guard below — silently reporting "confirmed
	// tightest possible" for exactly the bars (largest gaps) that most
	// likely belong to the widest-spread, least liquid names. Reporting it
	// as unmeasurable instead lets HighLowSpread average only the
	// overlapping, informative pairs rather than being dragged toward zero
	// by gap pairs that were never evidence of tightness at all. See
	// CODE_REVIEW_FINDINGS.md's "Corwin-Schultz isn't gap-adjusted".
	if l2 > h1 || h2 < l1 {
		return 0, false
	}

	b1 := math.Log(h1 / l1)
	b2 := math.Log(h2 / l2)
	beta := b1*b1 + b2*b2

	g := math.Log(math.Max(h1, h2) / math.Min(l1, l2))
	gamma := g * g

	alpha := (math.Sqrt(2*beta)-math.Sqrt(beta))/csDenom - math.Sqrt(gamma/csDenom)

	spread := 2 * (math.Exp(alpha) - 1) / (1 + math.Exp(alpha))
	if math.IsNaN(spread) || math.IsInf(spread, 0) || spread < 0 {
		return 0, true // a negative estimate is noise, not a negative spread
	}

	return spread, true
}
