package engine

import (
	"context"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
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

// stalledFeedBroker reports a position whose broker-side price has fallen to
// 80, and records the sells it is sent.
type stalledFeedBroker struct {
	asyncBroker

	sells int
}

func (b *stalledFeedBroker) SubmitOrder(ctx context.Context, o broker.Order) (broker.OrderResult, error) {
	if o.Side == broker.Sell {
		b.sells++
	}

	return b.asyncBroker.SubmitOrder(ctx, o)
}

func (*stalledFeedBroker) GetPositions(context.Context) ([]broker.Position, error) {
	return []broker.Position{{Symbol: "X", Qty: 2, AvgPrice: 100, Current: 80}}, nil
}

// TestStaleTickDoesNotOverrideTheBrokersPriceAtSync is the regression for
// PF2-1. syncPortfolio replaced the broker's fresh p.Current with ANY cached
// tick, however old. checkExits' stale-tick fallback reads h.LastPrice —
// which is that same frozen tick — so the fallback above could never reach
// the broker's price: with the feed stalled at 120, the broker at 80 and a
// stop at 90, no sell was ever sent. The test above cannot catch this, because
// its broker price equals the stale tick.
func TestStaleTickDoesNotOverrideTheBrokersPriceAtSync(t *testing.T) {
	e := ledgerEngine(t)
	b := &stalledFeedBroker{}
	e.brk = b

	e.prices.mu.Lock()
	e.prices.m["X"] = priceEntry{price: 120, at: time.Now().Add(-30 * time.Minute)}
	e.prices.mu.Unlock()

	e.SetExitsConfig(config.ExitsConfig{Enabled: true, StopLossPct: 0.10}) // stop at 90

	e.syncPortfolio(context.Background())

	if h, _ := e.pf.Snapshot().Holding("X"); h.LastPrice != 80 {
		t.Errorf("synced LastPrice = %v, want the broker's 80 over a 30-minute-old tick", h.LastPrice)
	}

	e.checkExits(context.Background())

	if b.sells != 1 {
		t.Errorf("stop-loss at 90 with the broker at 80 sent %d sells, want 1", b.sells)
	}
}

// lowerCaseBroker reports its one position under a lower-case symbol.
type lowerCaseBroker struct{ asyncBroker }

func (lowerCaseBroker) GetPositions(context.Context) ([]broker.Position, error) {
	return []broker.Position{{Symbol: "x", Qty: 2, AvgPrice: 100, Current: 120}}, nil
}

// TestSyncKeepsThePeakOfACaseDifferentSymbol is the regression for EN2-11.
// syncPortfolio dropped trailing-stop peaks for symbols no longer held, but
// built "held" from the raw broker symbols while the peaks are stored under
// normalised keys (EN-6). A broker reporting a symbol in another case never
// matched, so its peak was deleted on every sync and the trail re-seeded from
// scratch — a trailing stop that stops trailing.
func TestSyncKeepsThePeakOfACaseDifferentSymbol(t *testing.T) {
	e := ledgerEngine(t)
	e.brk = lowerCaseBroker{}
	e.peaks["X"] = 150

	e.syncPortfolio(context.Background())

	if got := e.peaks["X"]; got != 150 {
		t.Errorf("peak for X after a sync reporting \"x\" = %v, want the 150 high-water mark kept", got)
	}
}
