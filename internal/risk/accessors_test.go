package risk

import (
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// TestSettersReachTheLiveManager guards the API's limits and costs saves: the
// settings form calls SetLimits/SetCosts on the running manager, and a setter
// that wrote a copy would show the new value in the UI while Authorize kept
// enforcing the old one. Commission must follow the new cost model at once.
func TestSettersReachTheLiveManager(t *testing.T) {
	t.Parallel()

	m := New(config.Limits{MaxOrderValue: 100}, config.CostsConfig{})

	m.SetLimits(config.Limits{MaxOrderValue: 250, MaxOpenPositions: 3})

	if got := m.Limits(); got.MaxOrderValue != 250 || got.MaxOpenPositions != 3 {
		t.Errorf("Limits after SetLimits = %+v", got)
	}

	if got := m.Commission(1000); got != 0 {
		t.Errorf("Commission with no cost model = %v, want 0", got)
	}

	m.SetCosts(config.CostsConfig{CommissionFlat: 1, CommissionPct: 0.001, CommissionMin: 2.5})

	if got := m.Costs(); got.CommissionMin != 2.5 {
		t.Errorf("Costs after SetCosts = %+v", got)
	}

	// 1 + 0.1% of 1000 = 2, under the 2.5 minimum.
	if got := m.Commission(1000); got != 2.5 {
		t.Errorf("Commission(1000) = %v, want the 2.5 minimum", got)
	}

	// 1 + 0.1% of 10000 = 11; a sell's negative notional costs the same.
	if got := m.Commission(-10000); got != 11 {
		t.Errorf("Commission(-10000) = %v, want 11", got)
	}
}

// TestMinPositionsIsNilSafe guards the slot tier's getter: the engine reads
// it from a manager that may not exist (persistence/test wiring), and a nil
// manager must answer "no tier", not panic on the evaluation path.
func TestMinPositionsIsNilSafe(t *testing.T) {
	t.Parallel()

	var nilM *Manager
	if got := nilM.MinPositions(); got != 0 {
		t.Errorf("nil MinPositions = %d, want 0", got)
	}

	m := New(config.Limits{}, config.CostsConfig{})
	m.SetMinPositions(4)

	if got := m.MinPositions(); got != 4 {
		t.Errorf("MinPositions = %d, want 4", got)
	}
}

// TestDayRollsToTheCurrentUTCDay guards the test-mode snapshot (TM-3): Day is
// read to save the real daily spend before simulated buys begin. If it
// returned the day the accumulator last ROLLED rather than today, a snapshot
// taken just after midnight would restore yesterday's spend into today and
// refuse live buys against a budget already used up.
func TestDayRollsToTheCurrentUTCDay(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 1, 23, 59, 0, 0, time.UTC)

	m := New(config.Limits{MaxDailySpend: 1000}, config.CostsConfig{})
	m.SetClock(func() time.Time { return now })
	m.Restore("2026-11-01", 400)

	if got := m.Day(); got != "2026-11-01" {
		t.Fatalf("Day = %q, want 2026-11-01", got)
	}

	now = now.Add(2 * time.Minute)

	if got := m.Day(); got != "2026-11-02" {
		t.Errorf("Day after midnight = %q, want 2026-11-02", got)
	}

	if got := m.SpentToday(); got != 0 {
		t.Errorf("SpentToday after rolling = %v, want 0", got)
	}
}
