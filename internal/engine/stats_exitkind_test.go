package engine

import "testing"

// BT3-5's live twin: the time barrier ("max hold <d> reached") classified as a
// strategy exit, so the per-symbol exit breakdown could not show whether
// max_hold paid for itself. Mirrors backtest.classifyExit.
func TestTheTimeBarrierHasItsOwnExitKind(t *testing.T) {
	t.Parallel()

	if got := ClassifyExitKind("EXIT max hold 72h0m0s reached"); got != ExitKindMaxHold {
		t.Errorf("a time-barrier exit classified as %q, want %q", got, ExitKindMaxHold)
	}
}
