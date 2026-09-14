package main

import "testing"

// TestValidateFlagsRejectsNonPositiveCash reproduces the "-cash 0 and -bars
// too low don't crash but produce a misleading report" finding: a
// non-positive -cash used to flow straight into the run and surface only as
// a "strategy made no trades" report, indistinguishable from a strategy
// that legitimately never signalled.
func TestValidateFlagsRejectsNonPositiveCash(t *testing.T) {
	for _, cash := range []float64{0, -100} {
		if err := validateFlags(cash, 500, "AAPL"); err == nil {
			t.Errorf("cash=%v: want a validation error, got nil", cash)
		}
	}
}

// -bars only matters with -generate; -csv reads however many rows the file
// has, so a low/zero -bars value must not be rejected there.
func TestValidateFlagsRejectsNonPositiveBarsOnlyWithGenerate(t *testing.T) {
	for _, bars := range []int{0, -10} {
		if err := validateFlags(100000, bars, "AAPL"); err == nil {
			t.Errorf("bars=%d with -generate: want a validation error, got nil", bars)
		}

		if err := validateFlags(100000, bars, ""); err != nil {
			t.Errorf("bars=%d without -generate (i.e. -csv): want no error, got %v", bars, err)
		}
	}
}

func TestValidateFlagsAllowsSensibleValues(t *testing.T) {
	if err := validateFlags(100000, 500, "AAPL"); err != nil {
		t.Errorf("sensible cash/bars: want no error, got %v", err)
	}
}
