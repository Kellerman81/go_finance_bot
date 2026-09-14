package protect

import (
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// lowProfitManager builds a manager with the low-profit guard armed and a clock
// the test drives, so the lookback window is not measured in wall-clock time.
func lowProfitManager(t *testing.T, minPnL float64) (*Manager, *time.Time) {
	t.Helper()

	clock := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	m := New(config.ProtectionsConfig{
		Enabled:           true,
		LowProfitTrades:   3,
		LowProfitLookback: 7 * 24 * time.Hour,
		LowProfitLock:     24 * time.Hour,
		LowProfitMinPnL:   minPnL,
	})
	m.SetClock(func() time.Time { return clock })

	return m, &clock
}

// Fewer closes than the threshold is not evidence. Locking on two losses would
// remove symbols for behaving exactly as a 60%-win strategy expects.
func TestLowProfitNeedsEnoughTrades(t *testing.T) {
	m, _ := lowProfitManager(t, 0)

	m.RecordClose("AAPL", -5, false)
	m.RecordClose("AAPL", -5, false)

	// Two losses inside the window, but the guard wants three. The cooldown is
	// off in this config, so nothing else should have locked it either.
	if ok, reason := m.AllowEntry("AAPL"); !ok {
		t.Fatalf("locked after only 2 closes: %s", reason)
	}
}

// Three closes that net out negative lock the symbol.
func TestLowProfitLocksABleedingSymbol(t *testing.T) {
	m, _ := lowProfitManager(t, 0)

	m.RecordClose("AAPL", -4, false)
	m.RecordClose("AAPL", 3, false)
	m.RecordClose("AAPL", -2, false) // net -3

	ok, reason := m.AllowEntry("AAPL")
	if ok {
		t.Fatal("a symbol with net -3 over three trades was not locked")
	}

	if reason == "" {
		t.Error("a refusal must say which protection fired")
	}
}

// A symbol that is net profitable over the window is left alone, even with
// losing trades among the winners.
func TestLowProfitLeavesAProfitableSymbolAlone(t *testing.T) {
	m, _ := lowProfitManager(t, 0)

	m.RecordClose("MSFT", -4, false)
	m.RecordClose("MSFT", 12, false)
	m.RecordClose("MSFT", -2, false) // net +6

	if ok, reason := m.AllowEntry("MSFT"); !ok {
		t.Fatalf("a net-profitable symbol was locked: %s", reason)
	}
}

// Closes that fall out of the lookback window stop counting.
func TestLowProfitWindowExpires(t *testing.T) {
	m, clock := lowProfitManager(t, 0)

	m.RecordClose("AAPL", -5, false)
	m.RecordClose("AAPL", -5, false)

	*clock = clock.Add(8 * 24 * time.Hour) // both fall outside the 7d window

	m.RecordClose("AAPL", -1, false)

	if ok, _ := m.AllowEntry("AAPL"); !ok {
		t.Fatal("stale closes outside the lookback still triggered the guard")
	}
}

// min_pnl above zero demands the symbol actually pays for itself, not merely
// that it avoids losing.
func TestLowProfitMinPnLDemandsRealProfit(t *testing.T) {
	m, _ := lowProfitManager(t, 10)

	m.RecordClose("AAPL", 2, false)
	m.RecordClose("AAPL", 2, false)
	m.RecordClose("AAPL", 1, false) // net +5, under the +10 required

	if ok, _ := m.AllowEntry("AAPL"); ok {
		t.Fatal("net +5 passed a +10 requirement")
	}
}

// The lock expires on its own; only halts are sticky.
func TestLowProfitLockExpires(t *testing.T) {
	m, clock := lowProfitManager(t, 0)

	for range 3 {
		m.RecordClose("AAPL", -2, false)
	}

	if ok, _ := m.AllowEntry("AAPL"); ok {
		t.Fatal("expected a lock")
	}

	*clock = clock.Add(25 * time.Hour)

	if ok, reason := m.AllowEntry("AAPL"); !ok {
		t.Fatalf("the lock outlived low_profit_lock: %s", reason)
	}
}

// The guard is per symbol: one bad instrument must not stop the others.
func TestLowProfitIsPerSymbol(t *testing.T) {
	m, _ := lowProfitManager(t, 0)

	for range 3 {
		m.RecordClose("AAPL", -2, false)
	}

	if ok, _ := m.AllowEntry("MSFT"); !ok {
		t.Fatal("locking AAPL also blocked MSFT")
	}
}

// Off by default: an unconfigured guard blocks nothing.
func TestLowProfitOffByDefault(t *testing.T) {
	m := New(config.ProtectionsConfig{Enabled: true})

	for range 10 {
		m.RecordClose("AAPL", -50, false)
	}

	if ok, reason := m.AllowEntry("AAPL"); !ok {
		t.Fatalf("an unconfigured low-profit guard locked a symbol: %s", reason)
	}
}
