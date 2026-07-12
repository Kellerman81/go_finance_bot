package engine

import "sync"

// PriceCache is a concurrency-safe latest-price store shared between the engine
// (which writes ticks) and the simulated broker (which reads them as its fill
// oracle).
type PriceCache struct {
	mu sync.RWMutex
	m  map[string]float64
}

// NewPriceCache creates an empty price cache.
func NewPriceCache() *PriceCache {
	return &PriceCache{m: make(map[string]float64)}
}

// Set records the latest price for a symbol.
func (c *PriceCache) Set(symbol string, price float64) {
	c.mu.Lock()
	c.m[symbol] = price
	c.mu.Unlock()
}

// Get returns the latest price for a symbol and whether one exists.
func (c *PriceCache) Get(symbol string) (float64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p, ok := c.m[symbol]
	return p, ok
}

// Snapshot returns a copy of all known prices.
func (c *PriceCache) Snapshot() map[string]float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]float64, len(c.m))
	for k, v := range c.m {
		out[k] = v
	}
	return out
}
