package errlog

import (
	"fmt"
	"sync"
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

// TestARareSourceSurvivesThreeFloodingOnes is the regression for EL2-2. The
// per-source cap is a third of the ring, so three sources at their cap fill
// it, and the global trim then dropped the oldest entry whatever its source:
// one saxo error, followed by 100 each from yahoo, trading212 and notify — a
// network outage, the case the per-source cap was written for — and the saxo
// error was gone.
//
// Not parallel: the ring is process-wide.
func TestARareSourceSurvivesThreeFloodingOnes(t *testing.T) {
	Clear()
	t.Cleanup(Clear)

	Record("saxo", "the one error that mattered")

	for range 101 {
		Record("yahoo", "429")
		Record("trading212", "503")
		Record("notify", "timeout")
	}

	for _, e := range Recent() {
		if e.Source == "saxo" {
			return
		}
	}

	t.Error("three flooding sources evicted the only saxo entry")
}

// TestRecordOnceKeepsOneCopyOfAStandingCondition is the regression for EL2-3:
// a typo'd detector name was recorded on every strategy.New — once per
// optimizer trial, autotune candidate and settings apply — filling the
// strategy source's share with copies of one line.
func TestRecordOnceKeepsOneCopyOfAStandingCondition(t *testing.T) {
	Clear()
	t.Cleanup(Clear)

	for range 50 {
		RecordOnce("strategy", `unknown detector "emacross" ignored`)
	}

	RecordOnce("strategy", `unknown detector "rsii" ignored`)

	if n := len(Recent()); n != 2 {
		t.Errorf("ring holds %d entries, want 2 (one per distinct message)", n)
	}
}

// EL3-6: RecordOnce scanned and appended under two separate locks, so
// concurrent callers could both find the line absent and both append it.
func TestRecordOnceRecordsOnceUnderConcurrency(t *testing.T) {
	for round := range 50 {
		Clear()

		var wg sync.WaitGroup

		start := make(chan struct{})

		for range 16 {
			wg.Go(func() {
				<-start
				RecordOnce("strategy", "a standing typo")
			})
		}

		close(start)
		wg.Wait()

		n := 0

		for _, e := range Recent() {
			if e.Message == "a standing typo" {
				n++
			}
		}

		if n != 1 {
			t.Fatalf("round %d: %d copies of a RecordOnce line, want 1", round, n)
		}
	}
}

// TestRecordOncefFormatsBeforeDeduplicating guards the formatted form of the
// standing-condition logger: the dedupe must compare the RENDERED message, so
// the same typo reported on every evaluation stays one entry while a second,
// different one still gets its own.
func TestRecordOncefFormatsBeforeDeduplicating(t *testing.T) {
	Clear()

	for range 5 {
		RecordOncef("config", "unknown detector %q", "emma_cross")
	}

	RecordOncef("config", "unknown detector %q", "rsii")

	var got []string

	for _, e := range Recent() {
		if e.Source == "config" {
			got = append(got, e.Message)
		}
	}

	if len(got) != 2 {
		t.Fatalf("config entries = %q, want one per distinct message", got)
	}

	if got[1] != `unknown detector "emma_cross"` {
		t.Errorf("oldest entry = %q, want the formatted message", got[1])
	}
}
