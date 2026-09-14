package engine

import (
	"testing"
	"time"
)

// TestRecordAutotuneApplyPersistsAndSurvivesARestart mirrors
// TestSectorsSurviveARestartAndConfigStillWins: a symbol's last-applied
// timestamp is written to the store immediately, and a fresh Engine
// sharing only that store recovers it via restoreAutotuneApplies — the
// same "as if the process restarted" pattern every other persisted cache
// in this package is tested with.
func TestRecordAutotuneApplyPersistsAndSurvivesARestart(t *testing.T) {
	t.Parallel()

	st := &nameStore{}
	first := nameEngine(st, nil)

	before := time.Now()
	first.RecordAutotuneApply("AMZN")

	if st.applyWrites == 0 {
		t.Fatal("RecordAutotuneApply did not persist anything")
	}

	if _, ok := first.LastAutotuneApply("MSFT"); ok {
		t.Error("LastAutotuneApply(MSFT) = true, want false — never recorded")
	}

	got, ok := first.LastAutotuneApply("AMZN")
	if !ok {
		t.Fatal("LastAutotuneApply(AMZN) = false, want true immediately after recording")
	}

	if got.Before(before) || got.After(time.Now()) {
		t.Errorf("LastAutotuneApply(AMZN) = %v, want a timestamp taken during this test", got)
	}

	// A fresh engine, as if the process restarted, sharing only the store.
	second := nameEngine(st, nil)

	if _, ok := second.LastAutotuneApply("AMZN"); ok {
		t.Fatal("a freshly constructed engine already knows about AMZN before restoring — test setup is wrong")
	}

	second.restoreAutotuneApplies()

	restored, ok := second.LastAutotuneApply("AMZN")
	if !ok {
		t.Fatal("LastAutotuneApply(AMZN) = false after restore, want the persisted timestamp")
	}

	if !restored.Equal(got) {
		t.Errorf("restored timestamp = %v, want the originally recorded %v", restored, got)
	}
}

// TestRecordAutotuneApplyOverwritesAnOlderTimestamp checks a second apply
// to the same symbol replaces the first, rather than somehow accumulating
// — RunSymbolOptimize's own "how has our LAST change performed" question
// only makes sense against the MOST RECENT apply.
func TestRecordAutotuneApplyOverwritesAnOlderTimestamp(t *testing.T) {
	t.Parallel()

	st := &nameStore{}
	e := nameEngine(st, nil)

	e.RecordAutotuneApply("AMZN")
	first, _ := e.LastAutotuneApply("AMZN")

	time.Sleep(2 * time.Millisecond) // guarantee a strictly later timestamp

	e.RecordAutotuneApply("AMZN")
	second, _ := e.LastAutotuneApply("AMZN")

	if !second.After(first) {
		t.Errorf("second apply's timestamp %v is not after the first's %v", second, first)
	}
}
