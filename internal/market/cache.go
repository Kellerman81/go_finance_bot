package market

import (
	"context"
	"strings"
	"sync"
	"time"
)

// HistoryFetcher is the Candles half of a DataProvider — the piece the cache and
// fallback wrappers compose.
type HistoryFetcher interface {
	// Candles fetches historical bars for symbol in [from, to] at res.
	Candles(
		ctx context.Context,
		symbol string,
		res Resolution,
		from, to time.Time,
	) ([]Candle, error)
}

// CachingHistory wraps a HistoryFetcher with a short-lived per-(symbol,resolution)
// cache. Callers request rolling windows ending at ~now, so within the TTL a
// cached fetch is reused and sliced to the requested [from,to] instead of hitting
// the network again. This collapses the repeated warmup and per-order price
// lookups (venuePrice) that otherwise re-fetch the same history.
type CachingHistory struct {
	src HistoryFetcher
	ttl time.Duration

	mu      sync.Mutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	candles   []Candle
	from, to  time.Time
	fetchedAt time.Time
}

// NewCachingHistory wraps src, caching each (symbol,resolution) result for ttl.
// A ttl <= 0 defaults to 15s.
func NewCachingHistory(src HistoryFetcher, ttl time.Duration) *CachingHistory {
	if ttl <= 0 {
		ttl = 15 * time.Second
	}

	return &CachingHistory{src: src, ttl: ttl, entries: make(map[string]cacheEntry)}
}

// key builds the cache key for a symbol at a resolution.
func (*CachingHistory) key(symbol string, res Resolution) string {
	return string(res) + "\x00" + symbol
}

// Candles serves from cache when a fresh entry covers the requested window,
// otherwise fetches from the underlying source and stores the result.
func (c *CachingHistory) Candles(
	ctx context.Context,
	symbol string,
	res Resolution,
	from, to time.Time,
) ([]Candle, error) {
	key := c.key(symbol, res)

	c.mu.Lock()

	e, ok := c.entries[key]
	fresh := ok && time.Since(e.fetchedAt) < c.ttl && !from.Before(e.from)
	c.mu.Unlock()

	if fresh {
		return sliceByTime(e.candles, from, to), nil
	}

	candles, err := c.src.Candles(ctx, symbol, res, from, to)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()

	c.entries[key] = cacheEntry{candles: candles, from: from, to: to, fetchedAt: time.Now()}
	c.mu.Unlock()

	return candles, nil
}

// Invalidate drops any cached entry for the symbol/resolution, forcing the next
// Candles call to refetch. A zero Resolution clears every resolution.
func (c *CachingHistory) Invalidate(symbol string, res Resolution) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if res == "" {
		for k := range c.entries {
			if strings.HasSuffix(k, "\x00"+symbol) {
				delete(c.entries, k)
			}
		}

		return
	}

	delete(c.entries, c.key(symbol, res))
}

// sliceByTime returns the candles whose Time is within [from, to]. The input is
// assumed chronologically ordered; the result shares the backing array.
func sliceByTime(candles []Candle, from, to time.Time) []Candle {
	lo := 0
	for lo < len(candles) && candles[lo].Time.Before(from) {
		lo++
	}

	hi := len(candles)
	for hi > lo && candles[hi-1].Time.After(to) {
		hi--
	}

	return candles[lo:hi]
}
