package market

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestFetchCoolsDownAfterExhaustingRetries reproduces the "Yahoo retry/
// fallback can generate up to 12 requests per Candles call under failure, no
// negative caching — self-amplifying during a 429 storm" finding: once one
// fetch call exhausts every retry against a 429, a separate fetch call — as
// every other symbol's Candles call would trigger during a screener rescan —
// must not immediately repeat the same request storm.
func TestFetchCoolsDownAfterExhaustingRetries(t *testing.T) {
	var requests int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	y := NewYahooHistory()
	y.base = srv.URL
	y.minInterval = 0
	y.maxRetries = 1 // keep the exponential backoff short enough for a test

	if _, err := y.fetch(context.Background(), "AAPL", "1m", "5d"); err == nil {
		t.Fatal("fetch succeeded against a server that always 429s")
	}

	firstBatch := atomic.LoadInt32(&requests)
	if firstBatch != int32(y.maxRetries+1) {
		t.Fatalf("first fetch made %d requests, want %d (maxRetries+1)", firstBatch, y.maxRetries+1)
	}

	if !y.throttled() {
		t.Fatal("fetch exhausted every retry against a 429 but did not trip the cooldown")
	}

	// A second fetch — modeling a different symbol's Candles call arriving
	// during the same throttling episode — must not make any request at all.
	if _, err := y.fetch(context.Background(), "MSFT", "1m", "5d"); err == nil {
		t.Fatal("fetch succeeded while cooling down")
	}

	if got := atomic.LoadInt32(&requests); got != firstBatch {
		t.Fatalf("second fetch made %d more request(s) while cooling down, want 0",
			got-firstBatch)
	}
}

// TestFetchClearsCooldownOnSuccess confirms a cooldown does not outlive its
// purpose: once a request actually succeeds, a later one must not still be
// blocked by an earlier cooldown.
func TestFetchClearsCooldownOnSuccess(t *testing.T) {
	y := NewYahooHistory()
	y.coolDown() // simulate an earlier exhausted-retries cooldown

	if !y.throttled() {
		t.Fatal("coolDown did not trip the cooldown")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chartBars(5)))
	}))
	t.Cleanup(srv.Close)

	y.base = srv.URL
	y.minInterval = 0

	// The cooldown from coolDown() above is still active, so this must fail
	// immediately with no request — otherwise the assertion below (that
	// clearCooldown is what let the next one through) would be meaningless.
	if _, err := y.fetch(context.Background(), "AAPL", "1m", "5d"); err == nil {
		t.Fatal("fetch succeeded while a cooldown set by coolDown was still active")
	}

	y.clearCooldown()

	candles, err := y.fetch(context.Background(), "AAPL", "1m", "5d")
	if err != nil {
		t.Fatalf("fetch after clearCooldown: %v", err)
	}

	if len(candles) != 5 {
		t.Fatalf("got %d candles, want 5", len(candles))
	}

	if y.throttled() {
		t.Fatal("a successful fetch must clear any cooldown, not just skip it once")
	}
}
