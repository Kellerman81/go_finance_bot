package indicators

import (
	"math"
	"math/rand/v2"
	"testing"
)

// The allocation-free "Last" variants must agree with the full-series ones they
// replace. These are the tests that make the optimisation safe: a faster
// indicator that quietly reads differently changes what the bot trades.

// walk builds a deterministic OHLC random walk.
func walk(n int, seed uint64) (high, low, closes []float64) {
	rng := rand.New(rand.NewPCG(seed, seed*7+1)) //nolint:gosec // deterministic test data
	high = make([]float64, n)
	low = make([]float64, n)
	closes = make([]float64, n)

	price := 100.0

	for i := range n {
		price *= 1 + (rng.Float64()-0.5)*0.02

		spread := price * (0.001 + rng.Float64()*0.01)
		high[i] = price + spread
		low[i] = price - spread
		closes[i] = price + (rng.Float64()-0.5)*spread
	}

	return high, low, closes
}

// supertrendReference is the previous implementation: materialise the whole ATR
// series, then walk it. Kept here as the oracle the fast path is checked against.
//
// Updated alongside SupertrendLast's own fix for the never-holds/NaN-freeze
// findings (see CODE_REVIEW_FINDINGS.md) — an oracle that still had the old
// bugs would make the equivalence test below fail against the NOW-correct
// fast path, not because the two disagree, but because this one is stale.
func supertrendReference(
	high, low, closes []float64,
	period int,
	mult float64,
) (dir int, level float64, ok bool) {
	n := len(closes)
	if period <= 0 || n <= period {
		return 0, 0, false
	}

	atr := ATR(high, low, closes, period)

	var finalUpper, finalLower float64

	dir = 0
	started := false

	for i := period; i < n; i++ {
		if math.IsNaN(atr[i]) {
			return 0, 0, false
		}

		mid := (high[i] + low[i]) / 2
		basicUpper := mid + mult*atr[i]
		basicLower := mid - mult*atr[i]

		if !started {
			finalUpper, finalLower = basicUpper, basicLower
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

	switch {
	case dir > 0:
		return 1, finalLower, true
	case dir < 0:
		return -1, finalUpper, true
	default:
		return 0, 0, false
	}
}

func TestSupertrendLastMatchesTheFullSeriesVersion(t *testing.T) {
	t.Parallel()

	for _, n := range []int{15, 40, 200, 1000} {
		for _, period := range []int{5, 10, 14} {
			for _, mult := range []float64{1.5, 3} {
				for seed := uint64(1); seed <= 5; seed++ {
					high, low, closes := walk(n, seed)

					gotDir, gotLevel, gotOK := SupertrendLast(high, low, closes, period, mult)
					wantDir, wantLevel, wantOK := supertrendReference(
						high,
						low,
						closes,
						period,
						mult,
					)

					if gotOK != wantOK || gotDir != wantDir {
						t.Fatalf("n=%d period=%d mult=%v seed=%d: got (%d,%v) want (%d,%v)",
							n, period, mult, seed, gotDir, gotOK, wantDir, wantOK)
					}

					if math.Abs(gotLevel-wantLevel) > 1e-9 {
						t.Fatalf("n=%d period=%d mult=%v seed=%d: level %v, want %v",
							n, period, mult, seed, gotLevel, wantLevel)
					}
				}
			}
		}
	}
}

// The refactor that gave ATR, ATRLast and Supertrend a shared trueRange must not
// have changed what ATR reports.
func TestATRLastMatchesATR(t *testing.T) {
	t.Parallel()

	for _, n := range []int{20, 300} {
		for _, period := range []int{5, 14} {
			high, low, closes := walk(n, 3)

			full := ATR(high, low, closes, period)
			last, ok := ATRLast(high, low, closes, period)

			if !ok {
				t.Fatalf("n=%d period=%d: ATRLast reported no value", n, period)
			}

			if math.Abs(last-full[n-1]) > 1e-9 {
				t.Errorf("n=%d period=%d: ATRLast = %v, ATR[last] = %v", n, period, last, full[n-1])
			}
		}
	}
}

// Degenerate inputs must not panic or produce a value where there is none.
func TestSupertrendLastEdgeCases(t *testing.T) {
	t.Parallel()

	high, low, closes := walk(30, 9)

	for _, tc := range []struct {
		name   string
		n      int
		period int
	}{
		{"no bars", 0, 14},
		{"one bar", 1, 14},
		{"exactly period", 14, 14},
		{"zero period", 30, 0},
		{"negative period", 30, -1},
	} {
		if _, _, ok := SupertrendLast(high[:tc.n], low[:tc.n], closes[:tc.n], tc.period, 3); ok {
			t.Errorf("%s: reported a value it cannot have", tc.name)
		}
	}
}

// SupertrendLast must allocate nothing: it runs once per detector, per symbol,
// per bar, and the whole point of the rewrite was to stop it allocating two
// full-length slices each time.
func TestSupertrendLastDoesNotAllocate(t *testing.T) {
	high, low, closes := walk(1000, 4)

	if avg := testing.AllocsPerRun(50, func() {
		SupertrendLast(high, low, closes, 10, 3)
	}); avg != 0 {
		t.Errorf("SupertrendLast allocated %.1f times per call, want 0", avg)
	}
}

func TestATRLastDoesNotAllocate(t *testing.T) {
	high, low, closes := walk(1000, 5)

	if avg := testing.AllocsPerRun(50, func() {
		ATRLast(high, low, closes, 14)
	}); avg != 0 {
		t.Errorf("ATRLast allocated %.1f times per call, want 0", avg)
	}
}

// BenchmarkSupertrendReference and BenchmarkSupertrendLast pair the previous
// implementation (materialise the full ATR series, then walk it) against the
// current one (advance the ATR inside the walk).
func BenchmarkSupertrendReference(b *testing.B) {
	high, low, closes := walk(1000, 2)

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		supertrendReference(high, low, closes, 10, 3)
	}
}

func BenchmarkSupertrendLast(b *testing.B) {
	high, low, closes := walk(1000, 2)

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		SupertrendLast(high, low, closes, 10, 3)
	}
}
