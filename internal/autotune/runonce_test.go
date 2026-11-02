package autotune

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunOnceSharesTheSlotAndGivesItBack guards the operator's synchronous
// full pass: it must share the one process-wide run slot with the schedule
// and the per-symbol button — reporting false and running nothing when the
// slot is taken, so two searches never write strategy_overrides at once —
// and it must release the slot when done, or every later scheduled pass is
// skipped as "already running" for the life of the process.
//
// Not parallel: it holds the process-wide run slot.
func TestRunOnceSharesTheSlotAndGivesItBack(t *testing.T) {
	eng, rm, pf := newTestEngine(t, nil)
	cfg := eligibilityCfg(nil, nil)
	path := filepath.Join(t.TempDir(), "config.yaml")

	var logs strings.Builder

	if !TryAcquire() {
		t.Fatal("slot already held")
	}

	if RunOnce(context.Background(), eng, rm, pf, cfg, path, captureLog(&logs)) {
		t.Error("RunOnce ran while another search held the slot")
	}

	if !strings.Contains(logs.String(), "already running") {
		t.Errorf("a skipped pass was not logged: %q", logs.String())
	}

	Release()

	// A cancelled context keeps the pass itself trivial; what matters here is
	// that it ran and handed the slot back.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if !RunOnce(ctx, eng, rm, pf, cfg, path, captureLog(&strings.Builder{})) {
		t.Fatal("RunOnce refused a free slot")
	}

	if Busy() {
		t.Error("RunOnce returned still holding the run slot")
	}
}
