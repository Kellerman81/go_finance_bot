package config

import (
	"strings"
	"testing"
)

// CF3-4: the base strategy was checked for fast_ma < slow_ma only, so
// `min_strength: 25` — a percent where a fraction is read — loaded cleanly and
// the bot never bought, while the same value in an override was refused.
func TestBaseMinStrengthIsRangeChecked(t *testing.T) {
	t.Parallel()

	c := Default()
	c.Strategy.MinStrength = 25

	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "min_strength") {
		t.Errorf("Validate = %v, want a min_strength range error", err)
	}
}

// ST3-5: an ai or ml detector has no symbol on the higher timeframe, so it
// abstains — in htf_detectors it voted HOLD on every bar, silently. Refused
// while the higher timeframe is on; harmless (and allowed) while it is off.
func TestAIOrMLInTheHigherTimeframeIsRefused(t *testing.T) {
	t.Parallel()

	sc := Default().Strategy
	sc.HTFMultiple = 4
	sc.HTFDetectors = []string{"trend", "ML"}

	if err := ValidateStrategyRules(sc, "strategy"); err == nil {
		t.Error("htf_detectors with ml was accepted")
	}

	sc.HTFMultiple = 0
	if err := ValidateStrategyRules(sc, "strategy"); err != nil {
		t.Errorf("an inactive higher timeframe was refused: %v", err)
	}
}
