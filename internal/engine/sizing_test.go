package engine

import (
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
)

// TestSetSizingAlwaysOverwritesRiskFields locks in the asymmetry documented on
// SetSizing: minPositions only overwrites when meaningfully set (below 0 has
// no sensible meaning), but maxRiskSizeUSD and minOrderSizeUSD are always
// overwritten, because 0 is a real, valid value for both (no cap; no refusal
// threshold) — a caller that sends 0 must be able to actually turn them off
// rather than have it silently ignored.
func TestSetSizingAlwaysOverwritesRiskFields(t *testing.T) {
	e, _ := forgetEngine()

	e.SetSizing(4, 500, 50)

	minPos, maxRisk, minOrder := e.Sizing()
	if minPos != 4 || maxRisk != 500 || minOrder != 50 {
		t.Fatalf("Sizing() = %v/%v/%v, want 4/500/50", minPos, maxRisk, minOrder)
	}

	// Sending 0 for the two ceiling/threshold fields must actually clear
	// them, unlike min_positions which ignores an out-of-range value and
	// keeps what was already set.
	e.SetSizing(-1, 0, 0)

	minPos, maxRisk, minOrder = e.Sizing()
	if minPos != 4 {
		t.Errorf("min_positions changed on a -1 update: got %v, want the unchanged 4", minPos)
	}

	if maxRisk != 0 || minOrder != 0 {
		t.Errorf("max_risk_size_usd/min_order_size_usd = %v/%v, want 0/0 (a 0 update must clear them)",
			maxRisk, minOrder)
	}
}

// min_positions is the coarse tier of the position-slot rule, which lives in
// risk.Manager — so SetSizing has to push it there as well as storing it, or
// the setting would change in the UI and bind nothing.
func TestSetSizingPushesMinPositionsToTheRiskManager(t *testing.T) {
	e, _ := forgetEngine()
	e.rm = risk.New(config.Limits{}, config.CostsConfig{})

	e.SetSizing(5, 0, 0)

	if got := e.rm.MinPositions(); got != 5 {
		t.Errorf("risk manager min_positions = %d, want 5 — the setting would enforce nothing", got)
	}
}
