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

	mu        sync.Mutex
	entries   map[string]cacheEntry
	lastSweep time.Time
}

// cacheSweepMultiple bounds how long an entry survives after it was last
// queried, as a multiple of ttl. Without a sweep, a symbol the screener drops
// (a churning universe adds and drops symbols over a long-running process)
// left its candle data in entries forever — nothing ever removes a key,
// only Candles overwriting it on a later query for the same symbol, which
// never happens once the symbol stops being tracked. A steady memory leak
// over the bot's lifetime. See CODE_REVIEW_FINDINGS.md's CachingHistory
// finding.
const cacheSweepMultiple = 20

// Unwrap returns the wrapped source, so a caller probing for an optional
// capability can see through the decorator.
//
// Without it the symbol picker vanished in production while passing every test:
// main.go wraps the Yahoo provider in this cache, and a plain
// `historySource().(Searcher)` assertion then finds a *CachingHistory and gives
// up. Delegating Search here instead would be worse — the cache would satisfy
// Searcher even when wrapping a source that cannot search, so the UI would offer
// a picker that always finds nothing.
func (c *CachingHistory) Unwrap() HistoryFetcher { return c.src }

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

// sweepLocked removes entries not queried in a while, bounding the cache to
// roughly the symbols in active use rather than every symbol ever seen —
// see cacheSweepMultiple. Throttled to once per ttl so it doesn't add an
// O(len(entries)) scan to every single call. Caller holds c.mu.
func (c *CachingHistory) sweepLocked() {
	now := time.Now()
	if now.Sub(c.lastSweep) < c.ttl {
		return
	}

	c.lastSweep = now
	cutoff := now.Add(-cacheSweepMultiple * c.ttl)

	for k, e := range c.entries {
		if e.fetchedAt.Before(cutoff) {
			delete(c.entries, k)
		}
	}
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
	c.sweepLocked()

	e, ok := c.entries[key]
	// Both bounds must be covered, not just from: a cached entry fetched for
	// an earlier, narrower [from,to] would otherwise be served to a caller
	// asking for data reaching further forward than what's actually
	// cached — silently truncated results instead of a fresh fetch. See
	// CODE_REVIEW_FINDINGS.md's CachingHistory finding.
	//
	// The upper bound allows the TTL itself as slack. Every real caller asks
	// for a window ending at time.Now(), so a later call's `to` is always past
	// the cached one and "!to.After(e.to)" never held: the cache never hit in
	// production, and every warmup, venue-price and screener read went to
	// Yahoo — 429s, then the cooldown, then "venue price unavailable" blocking
	// entries (MK2-1). An entry younger than the TTL cannot be missing more
	// than the TTL's worth of bars past its own `to`, which is what the TTL
	// already accepts as fresh enough.
	fresh := ok && time.Since(e.fetchedAt) < c.ttl && !from.Before(e.from) &&
		!to.After(e.to.Add(c.ttl))
	c.mu.Unlock()

	if fresh {
		// A copy, not the cached backing array: sliceByTime's result shares
		// memory with the cached entry, and a caller that mutates its
		// candles in place (the engine's FX-conversion loop in warmup does
		// exactly this) would otherwise silently corrupt what every other
		// caller reads from this cache within the TTL — see
		// CODE_REVIEW_FINDINGS.md's CachingHistory finding.
		//
		// A substituted COARSER series is returned whole, as the miss path
		// returns it (MK2-6): sliced on a hit, the same warmup request got
		// three months of a fallback's hourly bars on a miss and nine days
		// of them on a hit — 400 bars or 216, decided by whether someone
		// fetched the symbol in the last 15 s (MK3-3).
		if gap, ok := FinestBarGap(e.candles); ok && gap > resolutionDuration(res)*3/2 {
			return cloneCandles(e.candles), nil
		}

		return cloneCandles(sliceByTime(e.candles, from, to)), nil
	}

	candles, err := c.src.Candles(ctx, symbol, res, from, to)
	if err != nil {
		return nil, err
	}

	// The miss path returns the source's answer whole, deliberately (MK2-6,
	// decided after the verify-change pass). Yahoo now windows the interval
	// it was asked for itself, so what can lie outside [from, to] here is a
	// coarser FALLBACK's history — exactly what warmup wants, and what
	// trimming here cut to ~60 hourly bars for an illiquid symbol. A hit
	// within the TTL still slices, as it always has: a narrower sub-window
	// served from a wider fetch.

	c.mu.Lock()

	c.entries[key] = cacheEntry{candles: candles, from: from, to: to, fetchedAt: time.Now()}
	c.mu.Unlock()

	// Same reasoning as the cache-hit path: candles is now also the cache's
	// own copy (stored above), so the caller must get its own.
	return cloneCandles(candles), nil
}

// cloneCandles returns an independent copy of candles, so a caller that
// mutates the result in place cannot corrupt a cache entry (or another
// caller's slice of the same backing array).
func cloneCandles(candles []Candle) []Candle {
	if candles == nil {
		return nil
	}

	out := make([]Candle, len(candles))
	copy(out, candles)

	return out
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
// assumed chronologically ordered; the result shares the backing array. A zero
// bound is open: "no window" must not read as "nothing after year 1".
func sliceByTime(candles []Candle, from, to time.Time) []Candle {
	lo := 0
	for lo < len(candles) && candles[lo].Time.Before(from) {
		lo++
	}

	hi := len(candles)
	for !to.IsZero() && hi > lo && candles[hi-1].Time.After(to) {
		hi--
	}

	return candles[lo:hi]
}
