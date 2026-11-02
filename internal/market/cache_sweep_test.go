package market

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestCachingHistoryRefetchesWhenToExtendsPastTheCachedWindow reproduces
// the "freshness check ignores the requested to bound" finding: a narrower
// first fetch must not be reused for a later call within TTL asking for
// data reaching further forward than what's cached — sliceByTime would
// otherwise silently return a truncated result instead of triggering a
// fresh fetch.
func TestCachingHistoryRefetchesWhenToExtendsPastTheCachedWindow(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	src := &countingFetcher{candles: minuteCandles(60, start)}
	c := NewCachingHistory(src, time.Minute)

	// First call: a narrow window, start..start+30min.
	if _, err := c.Candles(
		context.Background(), "AAPL", Res1Min, start, start.Add(30*time.Minute),
	); err != nil {
		t.Fatal(err)
	}

	// Second call within TTL: same from, but to extends 30 minutes further
	// forward than what's cached — must trigger a fresh fetch, not be
	// silently served (and truncated) from the narrower cached entry.
	if _, err := c.Candles(
		context.Background(), "AAPL", Res1Min, start, start.Add(60*time.Minute),
	); err != nil {
		t.Fatal(err)
	}

	if got := atomic.LoadInt64(&src.calls); got != 2 {
		t.Errorf("cache made %d upstream calls, want 2 — a wider `to` than what's "+
			"cached must trigger a fresh fetch, not be served (truncated) from cache", got)
	}
}

// TestCachingHistoryStillServesANarrowerToFromCache confirms the fix does
// not overcorrect: a LATER call asking for a `to` already covered by the
// cached window must still be served from cache, not force a refetch.
func TestCachingHistoryStillServesANarrowerToFromCache(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	src := &countingFetcher{candles: minuteCandles(60, start)}
	c := NewCachingHistory(src, time.Minute)

	if _, err := c.Candles(
		context.Background(), "AAPL", Res1Min, start, start.Add(60*time.Minute),
	); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Candles(
		context.Background(), "AAPL", Res1Min, start, start.Add(30*time.Minute),
	); err != nil {
		t.Fatal(err)
	}

	if got := atomic.LoadInt64(&src.calls); got != 1 {
		t.Errorf("cache made %d upstream calls, want 1 — a narrower `to` than "+
			"what's cached should still be served from cache", got)
	}
}

// TestCachingHistorySweepsStaleEntries reproduces the "entries are never
// evicted" finding: a symbol that stops being queried (dropped by a
// churning screener universe) must eventually be removed from the cache
// rather than sitting there forever — a steady memory leak over a
// long-running process otherwise.
func TestCachingHistorySweepsStaleEntries(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	src := &countingFetcher{candles: minuteCandles(60, start)}
	ttl := 5 * time.Millisecond // tiny so cacheSweepMultiple*ttl elapses fast in a test
	c := NewCachingHistory(src, ttl)

	if _, err := c.Candles(context.Background(), "AAPL", Res1Min, start, start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()

	if n != 1 {
		t.Fatalf("entries after first fetch = %d, want 1", n)
	}

	// Wait well past cacheSweepMultiple*ttl so AAPL's entry qualifies for
	// eviction, then make an unrelated call — the sweep is opportunistic,
	// piggybacked on any Candles call (throttled to once per ttl, itself
	// long past by this point too).
	time.Sleep(3 * cacheSweepMultiple * ttl)

	if _, err := c.Candles(context.Background(), "MSFT", Res1Min, start, start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	c.mu.Lock()
	_, aaplStillThere := c.entries[c.key("AAPL", Res1Min)]
	after := len(c.entries)
	c.mu.Unlock()

	if aaplStillThere {
		t.Error("AAPL's entry survived well past cacheSweepMultiple*ttl of disuse — it should have been swept")
	}

	if after != 1 {
		t.Errorf("entries after the sweep = %d, want 1 (only MSFT, just fetched)", after)
	}
}

// TestCachingHistoryHitsForARollingNowWindow is the regression for MK2-1. The
// "to must be covered" check meant a window ending at time.Now() — what every
// real caller asks for — could never hit: a later call's `to` is always past
// the cached one. Five rolling calls inside the 15 s TTL made five upstream
// fetches. An entry younger than the TTL may serve a `to` up to TTL past its
// own, which is the staleness the TTL already accepts.
func TestCachingHistoryHitsForARollingNowWindow(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	src := &countingFetcher{candles: minuteCandles(60, start)}
	c := NewCachingHistory(src, 15*time.Second)

	for range 5 {
		now := time.Now()
		if _, err := c.Candles(context.Background(), "AAPL", Res1Min, now.Add(-30*time.Minute), now); err != nil {
			t.Fatal(err)
		}

		time.Sleep(2 * time.Millisecond)
	}

	if got := atomic.LoadInt64(&src.calls); got != 1 {
		t.Errorf("5 rolling now-windows inside the TTL made %d upstream calls, want 1", got)
	}
}
