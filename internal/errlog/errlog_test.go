package errlog

import (
	"fmt"
	"testing"
)

// TestRecordEnforcesPerSourceCap reproduces the "no per-source cap" finding
// directly: a single flaky source must never occupy more than maxPerSource
// of the ring, regardless of how many errors it reports.
func TestRecordEnforcesPerSourceCap(t *testing.T) {
	Clear()

	for i := range maxPerSource + 50 {
		Recordf("yahoo", "failure %d", i)
	}

	count := 0

	for _, e := range Recent() {
		if e.Source == "yahoo" {
			count++
		}
	}

	if count > maxPerSource {
		t.Errorf("yahoo has %d entries in the ring, want <= %d (maxPerSource)", count, maxPerSource)
	}
}

// TestRecordPerSourceCapProtectsOtherSources reproduces the finding's own
// scenario: a flaky dependency (yahoo) recording far more than the global
// ring capacity must not evict a different, rarely-failing source's entry
// — the exact "genuinely important errors evicted" harm the finding names.
func TestRecordPerSourceCapProtectsOtherSources(t *testing.T) {
	Clear()

	Record("saxo", "a genuinely important one-off failure")

	// Flood past the GLOBAL cap with a different source — without a
	// per-source cap, this alone would push the saxo entry out once the
	// ring hit maxEntries.
	for i := range maxEntries + 100 {
		Recordf("yahoo", "failure %d", i)
	}

	found := false

	for _, e := range Recent() {
		if e.Source == "saxo" {
			found = true

			break
		}
	}

	if !found {
		t.Error("the saxo entry was evicted by yahoo flooding the ring — " +
			"a single flaky source must not be able to crowd out another source's errors")
	}
}

// TestRecordEnforcesGlobalCap confirms the pre-existing global-size
// contract still holds: the ring never exceeds maxEntries even across many
// distinct sources (so the per-source fix can't just let the ring grow
// unbounded by spreading errors across enough source names).
func TestRecordEnforcesGlobalCap(t *testing.T) {
	Clear()

	for i := range maxEntries * 3 {
		Recordf(fmt.Sprintf("source-%d", i%20), "failure %d", i)
	}

	if got := len(Recent()); got > maxEntries {
		t.Errorf("ring has %d entries, want <= %d (maxEntries)", got, maxEntries)
	}
}
