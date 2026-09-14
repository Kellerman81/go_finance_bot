package market

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// binanceStub serves canned Binance responses and records the paths requested.
func binanceStub(t *testing.T, klines, tickers string) (*Binance, *[]string) {
	t.Helper()

	var paths []string

	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.String())

			w.Header().Set("Content-Type", "application/json")

			switch {
			case strings.HasPrefix(r.URL.Path, "/api/v3/klines"):
				_, _ = w.Write([]byte(klines))
			case strings.HasPrefix(r.URL.Path, "/api/v3/ticker/price"):
				_, _ = w.Write([]byte(tickers))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}),
	)

	b := NewBinance(srv.URL)

	t.Cleanup(func() {
		_ = b.Close()
		srv.Close()
	})

	return b, &paths
}

// TestBinanceCandlesParsesKlines checks the positional kline array is decoded
// into OHLCV correctly — the part most likely to break silently.
func TestBinanceCandlesParsesKlines(t *testing.T) {
	t.Parallel()

	klines := `[
	  [1767225600000,"42000.10","42500.00","41800.55","42350.25","123.45",1767225659999,"0",0,"0","0","0"],
	  [1767225660000,"42350.25","42600.00","42300.00","42580.00","98.10",1767225719999,"0",0,"0","0","0"]
	]`

	b, paths := binanceStub(t, klines, `[]`)

	candles, err := b.Candles(t.Context(), "BTCUSDT", Res1Min, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	if len(candles) != 2 {
		t.Fatalf("got %d candles, want 2", len(candles))
	}

	c := candles[0]
	if c.Open != 42000.10 || c.High != 42500 || c.Low != 41800.55 || c.Close != 42350.25 {
		t.Errorf("OHLC = %v/%v/%v/%v, want 42000.10/42500/41800.55/42350.25",
			c.Open, c.High, c.Low, c.Close)
	}

	if c.Volume != 123.45 {
		t.Errorf("volume = %v, want 123.45", c.Volume)
	}

	if c.Symbol != "BTCUSDT" {
		t.Errorf("symbol = %q, want BTCUSDT", c.Symbol)
	}

	if c.Time.IsZero() || !c.Time.Equal(time.UnixMilli(1767225600000).UTC()) {
		t.Errorf("time = %v, want the kline's open time", c.Time)
	}

	if len(*paths) == 0 || !strings.Contains((*paths)[0], "interval=1m") {
		t.Errorf("requested %v, want a 1m interval", *paths)
	}
}

// TestBinanceIntervalMapping checks each resolution maps to Binance's name.
func TestBinanceIntervalMapping(t *testing.T) {
	t.Parallel()

	for res, want := range map[Resolution]string{
		Res1Min: "1m", Res5Min: "5m", Res15Min: "15m", Res1Hour: "1h", Res1Day: "1d",
	} {
		if got := binanceInterval(res); got != want {
			t.Errorf("binanceInterval(%q) = %q, want %q", res, got, want)
		}
	}
}

// TestBinanceNormalizesPairs checks the separators people type are stripped, so
// "BTC/USDT" in a watchlist resolves rather than 400ing.
func TestBinanceNormalizesPairs(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"BTC/USDT", "btc-usdt", "btc_usdt", " btcusdt "} {
		if got := normalizePair(in); got != "BTCUSDT" {
			t.Errorf("normalizePair(%q) = %q, want BTCUSDT", in, got)
		}
	}
}

// TestBinancePollEmitsQuotes checks subscribed symbols produce ticks and update
// LastQuote.
func TestBinancePollEmitsQuotes(t *testing.T) {
	t.Parallel()

	tickers := `[{"symbol":"BTCUSDT","price":"42350.25"},{"symbol":"ETHUSDT","price":"2210.40"}]`

	b, _ := binanceStub(t, `[]`, tickers)

	if err := b.Subscribe("BTC/USDT", "ETHUSDT"); err != nil {
		t.Fatal(err)
	}

	b.pollOnce() // drive one cycle directly rather than waiting on the ticker

	q, ok := b.LastQuote("BTCUSDT")
	if !ok || q.Price != 42350.25 {
		t.Errorf("LastQuote = %+v (ok=%v), want a 42350.25 price", q, ok)
	}

	// The separator-normalised form must find the same quote.
	if _, ok := b.LastQuote("BTC/USDT"); !ok {
		t.Error("LastQuote does not normalise the requested symbol")
	}

	got := 0

	for range 2 {
		select {
		case <-b.Quotes():
			got++
		case <-time.After(time.Second):
			t.Fatalf("got %d quotes on the channel, want 2", got)
		}
	}
}

// TestBinanceUnsubscribeStopsPolling checks a removed symbol is no longer
// requested.
func TestBinanceUnsubscribeStopsPolling(t *testing.T) {
	t.Parallel()

	b, paths := binanceStub(t, `[]`, `[{"symbol":"BTCUSDT","price":"1"}]`)

	_ = b.Subscribe("BTCUSDT", "ETHUSDT")
	_ = b.Unsubscribe("ETHUSDT")

	b.pollOnce()

	if len(*paths) == 0 {
		t.Fatal("no request made")
	}

	if strings.Contains((*paths)[0], "ETHUSDT") {
		t.Errorf("request %q still includes the unsubscribed symbol", (*paths)[0])
	}
}

// TestBinanceErrorsSurface checks a non-2xx becomes an error rather than an
// empty candle slice that would look like "no history".
func TestBinanceErrorsSurface(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(`{"code":-1121,"msg":"Invalid symbol."}`))
	}))

	b := NewBinance(srv.URL)

	t.Cleanup(func() {
		_ = b.Close()
		srv.Close()
	})

	_, err := b.Candles(t.Context(), "NOPE", Res1Min, time.Time{}, time.Time{})
	if err == nil {
		t.Fatal("want an error for an invalid symbol")
	}

	if !strings.Contains(err.Error(), "Invalid symbol") {
		t.Errorf("error = %v, want it to carry the API message", err)
	}

	if _, err := b.Candles(t.Context(), "  ", Res1Min, time.Time{}, time.Time{}); err == nil {
		t.Error("want an error for an empty symbol")
	}
}

// TestBinanceSatisfiesDataProvider is a compile-time check that the crypto feed
// is a drop-in for the engine.
func TestBinanceSatisfiesDataProvider(t *testing.T) {
	t.Parallel()

	var _ DataProvider = (*Binance)(nil)
}

// TestBinanceCloseDuringInFlightPollDoesNotPanic reproduces the shutdown race:
// Close must not close the quotes channel while pollOnce might still be about
// to send on it. Before the fix, Close closed quotes directly from the
// caller's goroutine while poll could still be mid-request (the ticker
// endpoint can block for up to the client's 15s timeout) — a subsequent send
// in pollOnce panicked with "send on closed channel" even though it used a
// non-blocking select. This holds the request open server-side and calls
// Close while it's in flight; Close must cancel the request, wait for poll to
// actually exit, and only then let poll's own goroutine close the channel.
func TestBinanceCloseDuringInFlightPollDoesNotPanic(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})

	var startOnce sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v3/ticker/price") {
			startOnce.Do(func() { close(started) })
			<-release // hold the request open until the test lets it go
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"symbol":"BTCUSDT","price":"1"}]`))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	b := newBinance(srv.URL, 5*time.Millisecond) // force poll to reach the handler almost immediately

	if err := b.Subscribe("BTCUSDT"); err != nil {
		t.Fatal(err)
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("poll never reached the ticker endpoint")
	}

	// Close while the request is still held open server-side — this is
	// exactly the window that used to race the quotes channel close. ctx
	// cancellation should abort the client-side wait, so this must return
	// promptly rather than blocking for the request's full timeout.
	done := make(chan error, 1)

	go func() { done <- b.Close() }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Close() = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return — ctx cancellation did not abort the in-flight request")
	}

	if _, ok := <-b.quotes; ok {
		t.Error("quotes channel still open (or delivered a tick) after Close returned")
	}
}
