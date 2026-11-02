package exits

import (
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// EX3-4: an ATR so large that entry − mult·ATR fell below zero dropped the
// ATR stop. With an ATR-only config that left NO stop; it gets a 1% floor.
// With a percentage set, that one stays in force — exits read the CURRENT
// ATR, so a crash bar on a held position must not move its stop to 1% of
// entry (the entry side is refused in risk.SizeBuy).
func TestAHugeATRNeverLeavesAPositionWithoutAStop(t *testing.T) {
	t.Parallel()

	atrOnly := config.ExitsConfig{Enabled: true, ATRStopMult: 3}
	if got := Compute(atrOnly, 10, 10, 3.4, 0).Stop; got <= 0 {
		t.Errorf("an ATR-only config has stop %v at ATR 3.4: the position has no stop-loss", got)
	}

	held := config.ExitsConfig{Enabled: true, StopLossPct: 0.05, ATRStopMult: 3}
	if got := Compute(held, 10, 10, 3.4, 0).Stop; got != 9.5 {
		t.Errorf("a held position's stop under an ATR spike = %v, want the 9.50 percentage stop", got)
	}

	if !ATROutOfRange(held, 10, 3.4) || ATROutOfRange(held, 10, 3.2) {
		t.Error("ATROutOfRange does not mark exactly the entries whose ATR stop cannot sit above zero")
	}
}

// EX3-5: the trail floor an add leaves behind is the pre-add trail AT THE
// SETTINGS OF THE ADD. Kept after the operator widened the trail from 3% to
// 10%, it froze the old tight level for exactly the positions that were ever
// scaled into. A floor lapses once the trailing settings change; one
// recorded before keys existed (empty key) is kept.
func TestATrailFloorLapsesWhenTheTrailSettingsChange(t *testing.T) {
	t.Parallel()

	tight := config.ExitsConfig{Enabled: true, TrailingStopPct: 0.03}
	_, floor := RebaseOnAdd(tight, 100, 110, 0, 0, 105, 110)
	key := TrailKey(tight)

	if FloorFor(tight, floor, key) != floor {
		t.Fatal("the floor lapsed under the settings that set it")
	}

	wide := tight
	wide.TrailingStopPct = 0.10

	if got := Compute(wide, 105, 110, 0, 0).WithTrailFloor(wide, FloorFor(wide, floor, key)).Trail; got > 99.01 {
		t.Errorf("after widening the trail to 10%%, the scaled-into position trails at %.2f, want 99", got)
	}

	if FloorFor(wide, floor, "") != floor {
		t.Error("a floor recorded before keys existed was dropped")
	}
}
