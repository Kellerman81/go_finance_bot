package indicators

import (
	"math"
	"math/rand"
	"testing"
)

// IN3-1: a zero-range bar (one print) shows neither bid nor ask, but the
// formula returns exactly 0 for any pair containing one, and that 0 was
// averaged in as a MEASURED tightest spread — a 1%-spread name with 60%
// single-print minutes read 0.26% and passed a 0.40% cap.
func TestAZeroRangeBarIsNotAMeasuredTightSpread(t *testing.T) {
	t.Parallel()

	if s, ok := csPair(100, 100, 101, 99); ok {
		t.Errorf("csPair with a single-print first bar = (%v, true), want unmeasurable", s)
	}

	high := []float64{100, 102, 100, 102, 100, 102}
	low := []float64{100, 100, 100, 100, 100, 100}

	if s, ok := HighLowSpread(high, low); ok {
		t.Errorf("alternating single-print / 2%%-range bars measured a %v spread, want unmeasurable", s)
	}
}

// simulateBars draws n bars of tradesPerBar prints each around a mid that
// random-walks within the bar and jumps by gapSD between bars; every print
// is at the bid or the ask, spread apart.
func simulateBars(n, tradesPerBar int, spread, intraSD, gapSD float64, seed int64) (high, low, closes []float64) {
	r := rand.New(rand.NewSource(seed)) //nolint:gosec // a reproducible fixture
	mid := 100.0

	for range n {
		mid *= math.Exp(r.NormFloat64() * gapSD)
		h, l, c := 0.0, math.Inf(1), 0.0

		for range tradesPerBar {
			mid *= math.Exp(r.NormFloat64() * intraSD)

			px := mid * (1 - spread/2)
			if r.Intn(2) == 0 {
				px = mid * (1 + spread/2)
			}

			h, l, c = math.Max(h, px), math.Min(l, px), px
		}

		high, low, closes = append(high, h), append(low, l), append(closes, c)
	}

	return high, low, closes
}

// IN3-2: the estimator skipped Corwin & Schultz's adjustment for moves
// between bars, so a second bar that opened away from the first's close but
// still overlapped it inflated the two-bar range — about a quarter too low on
// daily bars. With closes, the pair is shifted to the close and the estimate
// moves toward the true spread.
func TestTheOvernightAdjustmentReducesTheDownwardBias(t *testing.T) {
	t.Parallel()

	const truth = 0.01

	high, low, closes := simulateBars(5000, 50, truth, 0.001, 0.02, 3)

	raw, ok1 := HighLowSpread(high, low)
	adj, ok2 := HighLowSpreadAdjusted(high, low, closes)

	if !ok1 || !ok2 {
		t.Fatalf("estimates not measurable: raw ok=%v adjusted ok=%v", ok1, ok2)
	}

	if math.Abs(adj-truth) >= math.Abs(raw-truth)*0.9 {
		t.Errorf("true spread %.3f%%: unadjusted %.3f%%, adjusted %.3f%% — the adjustment did not "+
			"reduce the bias", truth*100, raw*100, adj*100)
	}
}

// IN3-3: an infinite close passed the !(hi > lo) guard, made the bucket width
// infinite and the bucket index int(NaN), and indexing with it panicked on
// the evaluation path.
func TestAnInfiniteCloseDoesNotPanicTheVolumeProfile(t *testing.T) {
	t.Parallel()

	if _, ok := BuildVolumeProfile([]float64{10, 11, 12, math.Inf(1)}, []float64{1, 1, 1, 1}, 4, 5, 0.7); ok {
		t.Error("a profile was built over an infinite close")
	}
}
