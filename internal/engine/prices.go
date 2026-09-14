package engine

import (
	"sync"
	"time"
)

// priceEntry is one symbol's last known price plus when it was recorded, so
// a reader that cares (see GetFresh) can tell a genuinely current price from
// one that stopped updating a while ago — e.g. because the feed is dropping
// ticks under load without fully closing (see CachingHistory's mutation fix
// and the closed-quote-stream fix for the related, more severe case of the
// feed stopping entirely).
type priceEntry struct {
	price float64
	at    time.Time
}

// PriceCache is a concurrency-safe latest-price store shared between the engine
// (which writes ticks) and the simulated broker (which reads them as its fill
// oracle).
type PriceCache struct {
	mu sync.RWMutex
	m  map[string]priceEntry
}

// NewPriceCache creates an empty price cache.
func NewPriceCache() *PriceCache {
	return &PriceCache{m: make(map[string]priceEntry)}
}

// Set records the latest price for a symbol, timestamped now.
func (c *PriceCache) Set(symbol string, price float64) {
	c.mu.Lock()

	c.m[symbol] = priceEntry{price: price, at: time.Now()}
	c.mu.Unlock()
}

// Get returns the latest price for a symbol and whether one exists, with no
// regard for how old it is. Most callers size or price an order they are
// about to submit synchronously, where a slightly-stale reference price is
// an ordinary and accepted risk (confirmVenuePrice/the deviation gate exist
// precisely to catch a reference price that has drifted too far from
// reality by the time the order goes out). Callers that instead decide
// whether to act at all from this price alone — currently just checkExits —
// should prefer GetFresh, which can tell "no price" from "an old one".
func (c *PriceCache) Get(symbol string) (float64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	e, ok := c.m[symbol]

	return e.price, ok
}

// GetFresh is Get, but reports ok=false for a price older than maxAge —
// treating a stale entry the same as no entry at all, so a caller with an
// existing "no price" fallback (checkExits falls back to the broker's own
// last-synced price) gets it instead of silently trusting a price the feed
// stopped updating a while ago.
func (c *PriceCache) GetFresh(symbol string, maxAge time.Duration) (float64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	e, ok := c.m[symbol]
	if !ok || time.Since(e.at) > maxAge {
		return 0, false
	}

	return e.price, true
}

// ReferencePrice is the best price the engine can name for a symbol: the live
// tick when there is one, otherwise the most recent bar's close.
//
// It exists because the engine and the simulated broker used to disagree about
// whether a symbol was priceable at all. The engine prices an order from the
// tick cache and falls back to the strategy signal's own price — which is the
// last bar's close, and the bar series is seeded from provider HISTORY during
// warmup. The Sim's oracle was PriceCache.Get alone. So for an instrument that
// has history but whose live feed never ticks — a thin Frankfurt or OTC listing
// is the reported case — the engine happily authorised an order that the Sim
// then rejected with "sim: no price for X", every evaluation, forever: an error
// row and an operator notification per cycle, for both entries and exits, with
// nothing ever completing. See CODE_REVIEW_FINDINGS.md's TM-1 finding.
//
// Deliberately NOT written back into the PriceCache: a warmup close is hours
// old, and seeding it there would make it look fresh to PriceCache.GetFresh,
// which checkExits uses precisely to refuse a stale price when deciding a
// stop-loss.
func (e *Engine) ReferencePrice(symbol string) (float64, bool) {
	if px, ok := e.prices.Get(symbol); ok && px > 0 {
		return px, true
	}

	if e.barsStore == nil {
		return 0, false
	}

	if bars := e.barsStore.tail(symbol, 1, nil); len(bars) > 0 && bars[len(bars)-1].Close > 0 {
		return bars[len(bars)-1].Close, true
	}

	return 0, false
}

// Snapshot returns a copy of all known prices.
func (c *PriceCache) Snapshot() map[string]float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make(map[string]float64, len(c.m))
	for k, v := range c.m {
		out[k] = v.price
	}

	return out
}
