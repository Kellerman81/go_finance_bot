package exits

import (
	"math"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// close reports whether two prices agree to within a cent.
func close(a, b float64) bool { return math.Abs(a-b) < 0.01 }

// ----- item 1: the two-stage trailing stop -----

// Below the offset the loose trail applies; at or above it the tight one does.
// One distance cannot be both loose enough early and tight enough late.
func TestTrailTightensPastTheOffset(t *testing.T) {
	cfg := config.ExitsConfig{
		Enabled:           true,
		TrailingStopPct:   0.10, // loose
		TrailingTightPct:  0.02, // tight
		TrailingOffsetPct: 0.05, // +5% arms the tight trail
	}

	// Up 3%: below the offset, so the loose trail — 10% under the peak.
	lv := Compute(cfg, 100, 103, 0, 0)
	if !close(lv.Trail, 92.70) {
		t.Errorf("below offset: trail = %.2f, want 92.70 (10%% under a 103 peak)", lv.Trail)
	}

	// Up 6%: past the offset, so the tight trail — 2% under the peak.
	lv = Compute(cfg, 100, 106, 0, 0)
	if !close(lv.Trail, 103.88) {
		t.Errorf("past offset: trail = %.2f, want 103.88 (2%% under a 106 peak)", lv.Trail)
	}

	// And the tight trail now protects a gain rather than sitting under water.
	if lv.Trail <= 100 {
		t.Errorf("past offset the trail (%.2f) should be above the 100 entry", lv.Trail)
	}
}

// The offset exactly reached counts as reached — a boundary that silently used
// the loose trail would make the setting behave differently from what it says.
func TestTrailTightensExactlyAtTheOffset(t *testing.T) {
	cfg := config.ExitsConfig{
		Enabled: true, TrailingStopPct: 0.10, TrailingTightPct: 0.02,
		TrailingOffsetPct: 0.05,
	}

	if lv := Compute(cfg, 100, 105, 0, 0); !close(lv.Trail, 102.90) {
		t.Errorf("at the offset: trail = %.2f, want the tight 102.90", lv.Trail)
	}
}

// trailing_only_after_offset leaves the stop-loss as the only downside rule
// until the position is far enough ahead.
func TestTrailSuppressedBelowOffset(t *testing.T) {
	cfg := config.ExitsConfig{
		Enabled: true, StopLossPct: 0.08,
		TrailingStopPct: 0.10, TrailingTightPct: 0.02,
		TrailingOffsetPct: 0.05, TrailingOnlyAfterOffset: true,
	}

	lv := Compute(cfg, 100, 103, 0, 0)
	if lv.Trail != 0 {
		t.Errorf("below the offset the trail should be suppressed, got %.2f", lv.Trail)
	}

	if !close(lv.Stop, 92) {
		t.Errorf("the stop-loss must still apply: got %.2f, want 92", lv.Stop)
	}

	if lv := Compute(cfg, 100, 106, 0, 0); !close(lv.Trail, 103.88) {
		t.Errorf("past the offset the trail should arm: got %.2f", lv.Trail)
	}
}

// With no offset configured the trail behaves exactly as it did before the
// second stage existed.
func TestTrailUnchangedWithoutAnOffset(t *testing.T) {
	cfg := config.ExitsConfig{Enabled: true, TrailingStopPct: 0.05}

	if lv := Compute(cfg, 100, 120, 0, 0); !close(lv.Trail, 114) {
		t.Errorf("single-stage trail = %.2f, want 114", lv.Trail)
	}
}

// ----- item 2: the time barrier -----

func TestMaxHoldFiresOnlyOnceHeldLongEnough(t *testing.T) {
	cfg := config.ExitsConfig{Enabled: true, MaxHold: 4 * time.Hour}

	if lv := Compute(cfg, 100, 100, 0, 3*time.Hour); lv.TimeExit != "" {
		t.Errorf("max_hold fired early at 3h: %q", lv.TimeExit)
	}

	lv := Compute(cfg, 100, 100, 0, 4*time.Hour)
	if lv.TimeExit == "" {
		t.Fatal("max_hold did not fire at exactly 4h")
	}

	// It has no price level, so it must trigger at whatever the market offers.
	if got := lv.Triggered(99.5); got != lv.TimeExit {
		t.Errorf("Triggered = %q, want the time exit %q", got, lv.TimeExit)
	}
}

// TestMaxHoldFiresEvenWithNoValidEntryPrice reproduces the "entry <= 0
// disables every exit rule including max_hold" finding: Compute used to
// return an all-zero Levels the moment entry <= 0, before ever looking at
// MaxHold — the one rule that needs no price at all. A position whose entry
// price is momentarily unknown (0) must still be able to age out rather than
// being stuck forever.
func TestMaxHoldFiresEvenWithNoValidEntryPrice(t *testing.T) {
	cfg := config.ExitsConfig{Enabled: true, MaxHold: 4 * time.Hour}

	lv := Compute(cfg, 0, 0, 0, 4*time.Hour)
	if lv.TimeExit == "" {
		t.Fatal("max_hold did not fire with entry <= 0")
	}

	if got := lv.Triggered(1); got != lv.TimeExit {
		t.Errorf("Triggered = %q, want the time exit %q", got, lv.TimeExit)
	}

	// Every price-based rule must still stay off with no valid entry — this
	// is not a general relaxation of the entry <= 0 guard, only max_hold's
	// exemption from it.
	if lv.Stop != 0 || lv.Trail != 0 || lv.Target != 0 {
		t.Errorf("a price-based level was computed with entry <= 0: %+v", lv)
	}
}

// A bar that breaks a price barrier and the time barrier together must report
// the price one: in a backtest that is the difference between filling at the
// stop and filling at the close.
func TestPriceBarriersOutrankTheTimeBarrier(t *testing.T) {
	cfg := config.ExitsConfig{Enabled: true, StopLossPct: 0.05, MaxHold: time.Hour}

	lv := Compute(cfg, 100, 100, 0, 2*time.Hour)
	if lv.TimeExit == "" {
		t.Fatal("expected the time barrier to be set")
	}

	if got := lv.Triggered(94); got == lv.TimeExit {
		t.Error("a breached stop-loss must outrank the time barrier")
	}
}

// The ROI ladder can only ever exit at breakeven or better; that is exactly the
// gap max_hold exists to close.
func TestROILadderCannotExitAtALoss(t *testing.T) {
	cfg := config.ExitsConfig{Enabled: true, ROI: map[string]float64{"0s": 0}}

	lv := Compute(cfg, 100, 100, 0, time.Hour)
	if lv.Target < 100 {
		t.Fatalf("ROI target %.2f is below entry; the ladder must not sell at a loss", lv.Target)
	}

	if lv.Triggered(97) != "" {
		t.Error("the ROI ladder fired at a loss")
	}
}

// ----- item 3: the scale-out ladder -----

func TestPartialRungsFireOnceEachInOrder(t *testing.T) {
	cfg := config.ExitsConfig{PartialTakeProfit: []config.PartialExit{
		{ProfitPct: 0.05, SellPct: 0.3},
		{ProfitPct: 0.10, SellPct: 0.3},
	}}

	if _, ok := NextPartial(cfg, 0.04, 0); ok {
		t.Error("a rung fired below its threshold")
	}

	p, ok := NextPartial(cfg, 0.06, 0)
	if !ok || p.Rung != 0 || !close(p.Fraction, 0.3) {
		t.Fatalf("first rung: %+v ok=%v, want rung 0 selling 0.3", p, ok)
	}

	// Having taken rung 0, the same price must not take it again.
	if _, ok := NextPartial(cfg, 0.06, 1); ok {
		t.Error("rung 0 fired twice")
	}

	p, ok = NextPartial(cfg, 0.11, 1)
	if !ok || p.Rung != 1 || !close(p.Fraction, 0.3) {
		t.Fatalf("second rung: %+v ok=%v, want rung 1", p, ok)
	}

	if _, ok := NextPartial(cfg, 0.50, 2); ok {
		t.Error("the ladder kept firing after every rung was taken")
	}
}

// A gap that clears several rungs at once takes all of them, rather than
// dribbling one out per evaluation and leaving the ladder behind the price.
func TestGapTakesEveryClearedRungAtOnce(t *testing.T) {
	cfg := config.ExitsConfig{PartialTakeProfit: []config.PartialExit{
		{ProfitPct: 0.05, SellPct: 0.25},
		{ProfitPct: 0.10, SellPct: 0.25},
		{ProfitPct: 0.15, SellPct: 0.25},
	}}

	p, ok := NextPartial(cfg, 0.20, 0)
	if !ok || p.Rung != 2 || !close(p.Fraction, 0.75) {
		t.Fatalf("gap: %+v ok=%v, want rung 2 selling 0.75", p, ok)
	}
}

// Rungs written out of order still mean what they say.
func TestLadderIsSortedByProfit(t *testing.T) {
	cfg := config.ExitsConfig{PartialTakeProfit: []config.PartialExit{
		{ProfitPct: 0.10, SellPct: 0.5},
		{ProfitPct: 0.03, SellPct: 0.2},
	}}

	p, ok := NextPartial(cfg, 0.04, 0)
	if !ok || !close(p.Fraction, 0.2) {
		t.Fatalf("at +4%% only the 3%% rung should fire, got %+v ok=%v", p, ok)
	}
}

func TestNoLadderMeansNoPartials(t *testing.T) {
	if _, ok := NextPartial(config.ExitsConfig{}, 5, 0); ok {
		t.Error("an unconfigured ladder produced a scale-out")
	}
}

// A ladder selling more than the position is a typo, not a strategy.
func TestValidateRejectsAnOversizedLadder(t *testing.T) {
	cfg := config.ExitsConfig{PartialTakeProfit: []config.PartialExit{
		{ProfitPct: 0.05, SellPct: 0.6},
		{ProfitPct: 0.10, SellPct: 0.6},
	}}

	if err := cfg.Validate(); err == nil {
		t.Error("a ladder selling 120% of the position was accepted")
	}
}
