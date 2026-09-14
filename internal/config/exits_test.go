package config

import "testing"

// TestExitsConfigRejectsAStopLossPctOfOneOrMore reproduces the
// stop_loss_pct finding: exits/exits.go computes stop = entry*(1-pct), so a
// value >= 1 makes the stop price zero or negative and the position runs
// with no stop loss at all — a units typo like stop_loss_pct: 2 (meant as
// "2%") must be rejected at load, not silently accepted.
func TestExitsConfigRejectsAStopLossPctOfOneOrMore(t *testing.T) {
	for _, pct := range []float64{1, 1.5, 2, -0.1} {
		e := ExitsConfig{Enabled: true, StopLossPct: pct}
		if err := e.Validate(); err == nil {
			t.Errorf("stop_loss_pct=%v: want a validation error, got nil", pct)
		}
	}
}

// TestExitsConfigAllowsZeroDistances checks the documented "0 disables it"
// convention still loads clean — Validate must not reject the values the
// package's own doc comments call out as intentional.
func TestExitsConfigAllowsZeroDistances(t *testing.T) {
	e := ExitsConfig{
		Enabled: true,
		// All zero: rely entirely on ATR-based stops/trail, no percentage
		// floor, no take-profit, no trailing offset stage.
		StopLossPct: 0, TrailingStopPct: 0, TrailingTightPct: 0,
		TakeProfitPct: 0, TrailingOffsetPct: 0,
		ATRStopMult: 2.5, ATRTrailMult: 2,
	}
	if err := e.Validate(); err != nil {
		t.Errorf("all-zero percentage distances should be valid (ATR-only mode), got: %v", err)
	}
}

// TestExitsConfigRejectsNegativeTakeProfitAndOffset checks the two other new
// bounds independently of the stop/trail check above.
func TestExitsConfigRejectsNegativeTakeProfitAndOffset(t *testing.T) {
	if err := (ExitsConfig{Enabled: true, TakeProfitPct: -0.1}).Validate(); err == nil {
		t.Error("negative take_profit_pct: want a validation error, got nil")
	}

	if err := (ExitsConfig{Enabled: true, TrailingOffsetPct: -0.1}).Validate(); err == nil {
		t.Error("negative trailing_offset_pct: want a validation error, got nil")
	}
}

// TestExitsConfigRejectsANegativeROIValue reproduces the "negative ROI-ladder
// values produce a take-profit that exits at a loss" finding: ROI's own doc
// comment states the ladder "can only ever exit at breakeven or better", but
// nothing enforced it — a negative entry produces a target price below
// entry, which fires under a take-profit label for what is actually a loss.
func TestExitsConfigRejectsANegativeROIValue(t *testing.T) {
	e := ExitsConfig{Enabled: true, ROI: map[string]float64{"0s": 0.10, "2h": -0.02}}
	if err := e.Validate(); err == nil {
		t.Error("a negative ROI rung: want a validation error, got nil")
	}
}

// A zero ROI value is documented as intentional ("exit at any profit") and
// must still load clean.
func TestExitsConfigAllowsAZeroROIValue(t *testing.T) {
	e := ExitsConfig{Enabled: true, ROI: map[string]float64{"0s": 0.10, "6h": 0}}
	if err := e.Validate(); err != nil {
		t.Errorf("a zero ROI rung ('exit at any profit') should be valid, got: %v", err)
	}
}
