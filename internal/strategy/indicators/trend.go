package indicators

import "math"

// LinReg fits a least-squares line y = intercept + slope*x to the last `period`
// values (x = 0..n-1, oldest to newest). When fewer than `period` values are
// available it uses all of them. It returns the slope, the intercept, and the
// R² goodness-of-fit (0..1); ok is false when there are too few points or x has
// no variance. Slope is in price units per bar; R² says how cleanly the values
// follow that line (1 = a perfect straight trend, ~0 = noise around a level).
//
//nolint:revive // four results (slope/intercept/r2/ok) are clearer than a struct here
func LinReg(values []float64, period int) (slope, intercept, r2 float64, ok bool) {
	n := len(values)
	if period > 0 && n > period {
		values = values[n-period:]
		n = period
	}

	if n < 2 {
		return 0, 0, 0, false
	}

	var sx, sy, sxx, sxy float64

	for i := range n {
		x := float64(i)
		y := values[i]

		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}

	fn := float64(n)
	denom := fn*sxx - sx*sx

	if denom == 0 {
		return 0, 0, 0, false
	}

	slope = (fn*sxy - sx*sy) / denom
	intercept = (sy - slope*sx) / fn

	meanY := sy / fn

	var ssRes, ssTot float64

	for i := range n {
		pred := intercept + slope*float64(i)
		d := values[i] - pred

		ssRes += d * d

		dm := values[i] - meanY

		ssTot += dm * dm
	}

	if ssTot == 0 {
		// A perfectly flat series: the line fits exactly (slope ~0).
		return slope, intercept, 1, true
	}

	r2 = 1 - ssRes/ssTot
	if r2 < 0 {
		r2 = 0
	}

	if math.IsNaN(r2) {
		r2 = 0
	}

	return slope, intercept, r2, true
}

// ADXLast returns the final ADX, +DI and -DI values (Wilder's Average
// Directional Index) over the given period. ADX (0..100) measures trend
// *strength* regardless of direction; +DI vs -DI gives the direction. It needs
// roughly 2·period bars to become defined. ok is false before then.
//
//nolint:revive,cyclop // four results beat a struct; Wilder's smoothing is one pass
func ADXLast(high, low, closes []float64, period int) (adx, plusDI, minusDI float64, ok bool) {
	// The ADX seed needs DX readings at indices period..2·period-1, so 2·period
	// bars are enough. "n <= 2·period" refused at exactly that length, one bar
	// after ADX is defined — every disagreement with a textbook ADX was that
	// case (IN2-3).
	n := len(closes)
	if period <= 0 || n < 2*period {
		return 0, 0, 0, false
	}

	trAt := func(i int) float64 {
		hl := high[i] - low[i]
		hc := math.Abs(high[i] - closes[i-1])
		lc := math.Abs(low[i] - closes[i-1])
		return math.Max(hl, math.Max(hc, lc))
	}
	dmAt := func(i int) (plus, minus float64) {
		up := high[i] - high[i-1]
		down := low[i-1] - low[i]

		if up > down && up > 0 {
			plus = up
		}

		if down > up && down > 0 {
			minus = down
		}

		return plus, minus
	}
	// Wilder-smoothed TR / +DM / -DM, seeded with the sum of the first `period`.
	var trS, plusS, minusS float64

	for i := 1; i <= period; i++ {
		trS += trAt(i)

		p, m := dmAt(i)

		plusS += p
		minusS += m
	}

	diFrom := func(tr, dm float64) float64 {
		if tr == 0 {
			return 0
		}

		return 100 * dm / tr
	}
	dxFrom := func(pdi, mdi float64) float64 {
		sum := pdi + mdi
		if sum == 0 {
			return 0
		}

		return 100 * math.Abs(pdi-mdi) / sum
	}
	// Accumulate DX over the first `period` DI readings (indices period..2·period-1)
	// to seed ADX, then Wilder-smooth DX for the remainder.
	//
	// The first DI/DX reading (at index `period`) comes directly from the
	// seed sums above, with no recursive Wilder update applied yet —
	// Wilder's own construction takes the plain sum over bars 1..period as
	// already being the smoothed value AT index period; the recursive
	// update below is what advances from index period to period+1 and
	// onward. Running that update once before taking this first reading
	// (as a prior version of this function did) shifts the whole DI/DX/ADX
	// series one bar later than Wilder's definition — small in practice
	// (it converges away within about period bars) but real, and avoidable.
	// See CODE_REVIEW_FINDINGS.md's ADXLast-seed-offset finding.
	pdi := diFrom(trS, plusS)
	mdi := diFrom(trS, minusS)
	dx := dxFrom(pdi, mdi)

	var dxSum, adxVal float64

	dxSum = dx
	plusDI, minusDI = pdi, mdi

	// period == 1 is a degenerate case: the seed window (indices period ..
	// 2·period-1, i.e. just index 1) is already complete before the loop
	// below ever runs, since the loop only starts at period+1 = 2. Without
	// this, ADX would never be marked seeded for period == 1.
	seeded := period == 1
	if seeded {
		adxVal = dxSum
	}

	for i := period + 1; i < n; i++ {
		trS = trS - trS/float64(period) + trAt(i)

		p, m := dmAt(i)

		plusS = plusS - plusS/float64(period) + p
		minusS = minusS - minusS/float64(period) + m

		pdi = diFrom(trS, plusS)
		mdi = diFrom(trS, minusS)
		dx = dxFrom(pdi, mdi)

		switch {
		case i < 2*period:
			dxSum += dx
			if i == 2*period-1 {
				adxVal = dxSum / float64(period)
				seeded = true
			}

		default:
			adxVal = (adxVal*float64(period-1) + dx) / float64(period)
		}

		plusDI, minusDI = pdi, mdi
	}

	if !seeded {
		return 0, 0, 0, false
	}

	return adxVal, plusDI, minusDI, true
}
