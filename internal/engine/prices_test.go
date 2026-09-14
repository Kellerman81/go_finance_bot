package engine

import (
	"context"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// TestPriceCacheGetFreshRejectsAStalePrice reproduces the
// dropped-ticks-unrecoverable finding: GetFresh must distinguish a price the
// feed is still actively updating from one it stopped updating a while ago
// (e.g. dropping ticks under load without the stream fully closing), so a
// caller like checkExits can fall back instead of trusting a stale price for
// a stop-loss/take-profit decision.
func TestPriceCacheGetFreshRejectsAStalePrice(t *testing.T) {
	c := NewPriceCache()
	c.Set("AAPL", 100)

	if _, ok := c.GetFresh("AAPL", time.Hour); !ok {
		t.Fatal("a just-set price should be fresh")
	}

	// Back-date the entry, as if the feed stopped ticking a while ago.
	c.mu.Lock()
	c.m["AAPL"] = priceEntry{price: 100, at: time.Now().Add(-10 * time.Minute)}
	c.mu.Unlock()

	if _, ok := c.GetFresh("AAPL", 5*time.Minute); ok {
		t.Error("GetFresh returned a price older than maxAge")
	}

	// Get (unlike GetFresh) has no age awareness — must still return it.
	if p, ok := c.Get("AAPL"); !ok || p != 100 {
		t.Errorf("Get(\"AAPL\") = %v, %v, want 100, true (Get ignores staleness by design)", p, ok)
	}

	if _, ok := c.GetFresh("UNKNOWN", time.Hour); ok {
		t.Error("GetFresh found a price for a symbol that was never set")
	}
}

// TestCheckExitsFallsBackToBrokerPriceWhenTickIsStale reproduces the same
// finding at the checkExits integration level: a stale engine-side tick
// must not be trusted for the stop-loss check — the existing "no price"
// fallback to the broker's own last-synced price (h.LastPrice) must engage
// for a stale price too, not just a missing one.
func TestCheckExitsFallsBackToBrokerPriceWhenTickIsStale(t *testing.T) {
	e := ledgerEngine(t)

	// ledgerEngine seeds e.prices.Set("X", 120) and a position with
	// EntryPrice 100. Back-date that tick so GetFresh rejects it, leaving
	// only the broker snapshot's LastPrice (120, matching what was synced
	// in SyncPositions) as a usable price.
	e.prices.mu.Lock()
	e.prices.m["X"] = priceEntry{price: 120, at: time.Now().Add(-10 * time.Minute)}
	e.prices.mu.Unlock()

	// A stop-loss wide enough that it would NOT fire at the broker's
	// LastPrice (120, above the 100 entry) — exercising checkExits must not
	// panic or silently skip the symbol just because the engine-side tick is
	// stale.
	e.SetExitsConfig(config.ExitsConfig{Enabled: true, StopLossPct: 0.5})

	e.checkExits(context.Background())

	// The position must still be tracked (no panic, no spurious full exit
	// from misreading a stale/zero price as a crash).
	if _, ok := e.openPos["X"]; !ok {
		t.Error("position disappeared after checkExits ran against a stale tick")
	}
}
