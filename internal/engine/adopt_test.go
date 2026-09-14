package engine

import (
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
)

// adoptEngine builds an engine holding one broker position with no durable
// record — the "held, not tracked" state an unconfirmed buy leaves behind.
func adoptEngine(t *testing.T, limitTimeout time.Duration) *Engine {
	t.Helper()

	e := fxTestEngine(plainBroker{})
	e.store = nopStore{}
	e.openPos = make(map[string]*OpenPosition)
	e.pendingSell = make(map[string]float64)
	e.peaks = make(map[string]float64)
	e.untrackedSince = make(map[string]time.Time)
	e.cfg = config.EngineConfig{
		KeepInterval: 2 * time.Hour,
		Execution:    config.ExecutionConfig{LimitTimeout: limitTimeout},
	}

	e.pf.SyncPositions([]portfolio.Holding{
		{Symbol: "NVDA", Qty: 0.8486, AvgPrice: 189.0, LastPrice: 190.0, MarketValue: 161.0},
	})

	return e
}

func nvdaHoldings() []portfolio.Holding {
	return []portfolio.Holding{
		{Symbol: "NVDA", Qty: 0.8486, AvgPrice: 189.0, LastPrice: 190.0, MarketValue: 161.0},
	}
}

// The bug this closes: an unconfirmed buy is deliberately never booked into
// openPos, and heldLongEnough returns "don't block" when there is no record —
// so a 2h keep_interval failed to stop a sell twenty minutes after the buy.
// Adopting the holding gives it an EntryTime, and the hold applies.
func TestAdoptionMakesKeepIntervalApply(t *testing.T) {
	t.Parallel()

	e := adoptEngine(t, time.Minute)

	// Before adoption: no record, so the hold is not enforced at all.
	if !e.heldLongEnough("NVDA") {
		t.Fatal("precondition: an untracked holding should not be blocked yet")
	}

	// First sight only starts the clock.
	e.adoptUntrackedHoldings(nvdaHoldings())

	if _, tracked := e.openPos["NVDA"]; tracked {
		t.Fatal("adopted on first sight, before the grace elapsed")
	}

	// Age it past the grace, then sync again. adoptGrace takes the lock
	// itself, so it must be read before acquiring it.
	grace := e.adoptGrace()

	e.mu.Lock()
	e.untrackedSince["NVDA"] = time.Now().Add(-grace - time.Second)
	e.mu.Unlock()

	e.adoptUntrackedHoldings(nvdaHoldings())

	pos, tracked := e.openPos["NVDA"]
	if !tracked {
		t.Fatal("the holding was never adopted")
	}

	if pos.Qty != 0.8486 || pos.EntryPrice != 189.0 {
		t.Errorf("adopted %+v, want the broker's own qty and average price", pos)
	}

	if e.heldLongEnough("NVDA") {
		t.Error("keep_interval still did not apply after adoption")
	}
}

// A resting limit order is also "held with no record", and watchLimitOrder will
// call recordOpen for it — which ADDS to any existing record. Adopting before
// that would count the same fill twice and leave openPos.Qty at double the real
// position, corrupting realized P&L and the scale-out ladder's base quantity.
// The grace must always outlast the order's own working time.
func TestAdoptionGraceOutlastsALimitOrder(t *testing.T) {
	t.Parallel()

	for name, timeout := range map[string]time.Duration{
		"configured 2m": 2 * time.Minute,
		"long 10m":      10 * time.Minute,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			e := adoptEngine(t, timeout)

			if got := e.adoptGrace(); got <= timeout {
				t.Errorf("adoptGrace = %v, want > the %v limit timeout", got, timeout)
			}

			// Simulate the whole window a limit order may work for: no
			// adoption may happen inside it.
			e.adoptUntrackedHoldings(nvdaHoldings())

			e.mu.Lock()
			e.untrackedSince["NVDA"] = time.Now().Add(-timeout)
			e.mu.Unlock()

			e.adoptUntrackedHoldings(nvdaHoldings())

			if _, tracked := e.openPos["NVDA"]; tracked {
				t.Error("adopted while a limit order could still be working")
			}
		})
	}
}

// An unset limit timeout must still produce a grace longer than the default
// working time, or the same double-count race reopens on a default config.
func TestAdoptionGraceUsesTheDefaultLimitTimeout(t *testing.T) {
	t.Parallel()

	e := adoptEngine(t, 0)

	if got := e.adoptGrace(); got <= defaultLimitTimeout {
		t.Errorf("adoptGrace = %v, want > the %v default", got, defaultLimitTimeout)
	}
}

// The clock measures a CONTINUOUS untracked stretch. A symbol that stops being
// reported (or gets tracked) drops out, so an old episode cannot combine with a
// new one to adopt something that has only just appeared.
func TestAdoptionClockResetsWhenTheHoldingDisappears(t *testing.T) {
	t.Parallel()

	e := adoptEngine(t, time.Minute)

	e.adoptUntrackedHoldings(nvdaHoldings())

	e.mu.Lock()
	e.untrackedSince["NVDA"] = time.Now().Add(-time.Hour) // long overdue
	e.mu.Unlock()

	// The broker stops reporting it — position closed elsewhere.
	e.adoptUntrackedHoldings(nil)

	e.mu.RLock()
	_, stillTiming := e.untrackedSince["NVDA"]
	e.mu.RUnlock()

	if stillTiming {
		t.Fatal("the clock kept running for a holding that is no longer reported")
	}

	// It comes back: the clock starts over rather than adopting immediately.
	e.adoptUntrackedHoldings(nvdaHoldings())

	if _, tracked := e.openPos["NVDA"]; tracked {
		t.Error("a reappearing holding was adopted on the strength of a stale clock")
	}
}

// A sell in flight means the broker snapshot may simply not have caught up.
// Adopting there would resurrect a position on its way out.
func TestAdoptionSkipsASymbolWithASellInFlight(t *testing.T) {
	t.Parallel()

	e := adoptEngine(t, time.Minute)
	e.markPendingSell("NVDA", 0.8486)

	for range 2 {
		e.adoptUntrackedHoldings(nvdaHoldings())

		e.mu.Lock()
		e.untrackedSince["NVDA"] = time.Now().Add(-time.Hour)
		e.mu.Unlock()
	}

	e.adoptUntrackedHoldings(nvdaHoldings())

	if _, tracked := e.openPos["NVDA"]; tracked {
		t.Error("adopted a position with an unconfirmed sell outstanding")
	}
}

// Dust is a just-closed position or a rounding remainder, not something to
// start managing.
func TestAdoptionSkipsDust(t *testing.T) {
	t.Parallel()

	e := adoptEngine(t, time.Minute)

	dust := []portfolio.Holding{{Symbol: "NVDA", Qty: sellDustQty / 2, AvgPrice: 189}}

	for range 3 {
		e.adoptUntrackedHoldings(dust)
	}

	if _, tracked := e.openPos["NVDA"]; tracked {
		t.Error("a dust row was adopted")
	}
}

// A position the bot already tracks (restored at startup, or booked from a
// confirmed fill) must never be re-adopted — that would reset its EntryTime and
// restart both keep_interval and the max_hold clock on every restart.
func TestAdoptionLeavesTrackedPositionsAlone(t *testing.T) {
	t.Parallel()

	e := adoptEngine(t, time.Minute)

	entered := time.Now().Add(-12 * time.Hour)
	e.openPos["NVDA"] = &OpenPosition{
		Symbol: "NVDA", Qty: 0.8486, EntryPrice: 150, EntryTime: entered,
	}

	for range 3 {
		e.adoptUntrackedHoldings(nvdaHoldings())
	}

	if got := e.openPos["NVDA"].EntryTime; !got.Equal(entered) {
		t.Errorf("EntryTime = %v, want the original %v preserved", got, entered)
	}

	if got := e.openPos["NVDA"].EntryPrice; got != 150 {
		t.Errorf("EntryPrice = %v, want the tracked 150, not the broker's average", got)
	}
}
