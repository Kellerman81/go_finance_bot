package ai

import (
	"slices"
	"testing"
)

// TestVectorMatchesFeatureNames guards the schema a trained ml model is tied
// to: Vector and FeatureNames are two hand-written lists that must stay the
// same length and in the same order. A column added to one and not the other
// shifts every later column, and a model then reads RSI where it learned ADX —
// silently wrong, with nothing failing.
func TestVectorMatchesFeatureNames(t *testing.T) {
	t.Parallel()

	names := FeatureNames()

	f := Features{
		Ret1: 1, Ret5: 2, Ret20: 3,
		EMASpreadPct: 4, RSI: 5, MACDHistPct: 6, BBPos: 7, ATRPct: 8,
		ADX: 9, PlusDI: 15, MinusDI: 5, StochK: 11, StochD: 12, RVOL: 13, RangePos: 14,
		TrendPct: 15, TrendR2: 16,
	}

	v := f.Vector()
	if len(v) != len(names) {
		t.Fatalf("Vector has %d columns, FeatureNames %d", len(v), len(names))
	}

	// Each column carries its own position, so a swapped pair shows up here.
	for i, got := range v {
		if got != float64(i+1) {
			t.Errorf("column %d (%s) = %v, want %v", i, names[i], got, float64(i+1))
		}
	}

	// The two directional readings collapse into +DI − DI, the one thing the
	// ADX detector votes on.
	if i := slices.Index(names, "di_spread"); i < 0 || v[i] != 10 {
		t.Errorf("di_spread column = %v, want PlusDI-MinusDI = 10", v[i])
	}

	// Names are a schema key: no duplicates.
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Errorf("duplicate feature name %q", n)
		}

		seen[n] = true
	}
}

// TestDefinedNeedsTheCoreReadings guards the training filter: a warmup row of
// zero imputations teaches a model only what warmup looks like, so Defined
// must be false until the core indicators are computable — and true once a
// full history makes them so.
func TestDefinedNeedsTheCoreReadings(t *testing.T) {
	t.Parallel()

	if (Features{}).Defined() {
		t.Error("a zero snapshot reports Defined")
	}

	h, l, c, v := series(300)

	f, ok := BuildFeatures("AAPL", h, l, c, v, Params{}.WithDefaults())
	if !ok {
		t.Fatal("BuildFeatures: not ok on 300 bars")
	}

	if !f.Defined() {
		t.Error("a 300-bar snapshot is not Defined")
	}

	short, ok := BuildFeatures("AAPL", h[:5], l[:5], c[:5], v[:5], Params{}.WithDefaults())
	if ok && short.Defined() {
		t.Error("a 5-bar snapshot reports Defined")
	}
}
