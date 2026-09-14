package indicators

import "testing"

// A genuine overnight gap — bar 2 trades entirely above bar 1's high — means
// the two bars share no overlapping price range at all. The Corwin-Schultz
// estimator only detects tightness through overlap (the joint range coming
// in smaller than the sum of the bars' own ranges); a non-overlapping pair
// can never produce that, so it carries no valid spread signal. Before the
// fix, csPair still ran such a pair through the formula and the
// negative-alpha guard clamped it to a *measured* 0% spread — reporting
// "confirmed tightest possible" for exactly the bars most likely to belong
// to a wide-spread, illiquid name. The fix marks it unmeasurable instead.
func TestCsPairAGenuineGapUpIsUnmeasurable(t *testing.T) {
	t.Parallel()

	h1, l1 := 102.0, 98.0
	h2, l2 := 150.0, 140.0

	if !(l2 > h1) {
		t.Fatalf("test fixture is not actually gapped: l2=%v h1=%v", l2, h1)
	}

	got, ok := csPair(h1, l1, h2, l2)
	if ok {
		t.Errorf("csPair(%v,%v,%v,%v) ok=true (spread=%v), want ok=false — "+
			"a non-overlapping pair carries no Corwin-Schultz signal and must "+
			"not be silently counted as a measured 0%% spread", h1, l1, h2, l2, got)
	}
}

// The symmetric case: bar 2 trades entirely below bar 1's low.
func TestCsPairAGenuineGapDownIsUnmeasurable(t *testing.T) {
	t.Parallel()

	h1, l1 := 150.0, 140.0
	h2, l2 := 102.0, 98.0

	if !(h2 < l1) {
		t.Fatalf("test fixture is not actually gapped: h2=%v l1=%v", h2, l1)
	}

	got, ok := csPair(h1, l1, h2, l2)
	if ok {
		t.Errorf("csPair(%v,%v,%v,%v) ok=true (spread=%v), want ok=false", h1, l1, h2, l2, got)
	}
}

// A pair that merely touches at the boundary (bar 2's low exactly equals
// bar 1's high) has zero overlap width but is not a gap — the formula
// should still run for it, matching its existing zero-overlap behavior.
func TestCsPairExactlyTouchingIsStillMeasured(t *testing.T) {
	t.Parallel()

	h1, l1 := 100.0, 90.0
	h2, l2 := 110.0, 100.0 // l2 == h1 exactly: touching, not gapped

	_, ok := csPair(h1, l1, h2, l2)
	if !ok {
		t.Errorf("csPair(%v,%v,%v,%v) ok=false, want true — touching at the "+
			"boundary is not a gap and should still produce a measurement",
			h1, l1, h2, l2)
	}
}

// An overlapping (non-gapped) pair is unaffected by this fix and must keep
// producing a real measurement.
func TestCsPairOverlappingPairIsUnaffected(t *testing.T) {
	t.Parallel()

	h1, l1 := 102.0, 98.0
	h2, l2 := 103.0, 99.0 // overlaps [98,102] and [99,103]

	got, ok := csPair(h1, l1, h2, l2)
	if !ok {
		t.Fatalf("csPair(%v,%v,%v,%v) ok=false, want true", h1, l1, h2, l2)
	}

	if got < 0 {
		t.Errorf("csPair(%v,%v,%v,%v) = %v, want >= 0", h1, l1, h2, l2, got)
	}
}

// HighLowSpread averages over the *measured* pairs only. A gapped pair
// must not contribute a phantom 0% that drags a genuinely wide-spread,
// gap-prone name's average toward the tightest possible reading.
func TestHighLowSpreadExcludesAGenuinelyGappedPairFromItsAverage(t *testing.T) {
	t.Parallel()

	// Two overlapping, informative pairs (indices 0-1 and 1-2), then a
	// clean gap up to bar 3 (index 2-3).
	high := []float64{102, 103, 104, 150}
	low := []float64{98, 99, 100, 140}

	full, ok := HighLowSpread(high, low)
	if !ok {
		t.Fatalf("HighLowSpread ok=false, want true (informative pairs remain)")
	}

	// Same series minus the gapped bar — should equal the average once the
	// gapped pair is excluded rather than counted as 0.
	trimmedHigh := high[:3]
	trimmedLow := low[:3]

	trimmed, ok := HighLowSpread(trimmedHigh, trimmedLow)
	if !ok {
		t.Fatalf("HighLowSpread (trimmed) ok=false, want true")
	}

	if diff := full - trimmed; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("HighLowSpread with a trailing gapped bar = %v, want %v "+
			"(same as excluding the gapped pair entirely) — a gapped pair must "+
			"not be averaged in as a measured 0%% spread", full, trimmed)
	}
}

// When every consecutive pair is gapped, there is no informative pair left
// at all — HighLowSpread must report unmeasurable rather than a false 0%.
func TestHighLowSpreadIsUnmeasurableWhenEveryPairIsGapped(t *testing.T) {
	t.Parallel()

	high := []float64{102, 204, 408}
	low := []float64{98, 196, 392}

	got, ok := HighLowSpread(high, low)
	if ok {
		t.Errorf("HighLowSpread = %v, ok=true; want ok=false — every consecutive "+
			"pair is a clean gap with no shared price range, so nothing here is "+
			"actually measured", got)
	}
}
