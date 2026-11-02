package market

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeFinnhub is a minimal Finnhub websocket: it records every control frame
// the client sends and lets the test push trade messages back.
type fakeFinnhub struct {
	mu     sync.Mutex
	frames []map[string]string
	conns  chan *websocket.Conn
}

func (f *fakeFinnhub) handler(t *testing.T) http.HandlerFunc {
	up := websocket.Upgrader{}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "KEY" {
			t.Errorf("dial carried token %q, want KEY", r.URL.Query().Get("token"))
		}

		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		f.conns <- c

		for {
			var m map[string]string
			if err := c.ReadJSON(&m); err != nil {
				return
			}

			f.mu.Lock()
			f.frames = append(f.frames, m)
			f.mu.Unlock()
		}
	}
}

func (f *fakeFinnhub) saw(typ, symbol string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, m := range f.frames {
		if m["type"] == typ && m["symbol"] == symbol {
			return true
		}
	}

	return false
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", what)
}

// TestFinnhubStreamLifecycle guards the live tick feed end to end, which no
// other test reaches: symbols subscribed BEFORE the socket is up must be
// sent on connect (a reconnect otherwise streams nothing), later
// subscriptions and unsubscriptions must reach the server, a trade message
// must arrive on Quotes and in LastQuote, and Close must close the channel —
// the engine's quote loop ranges over it — and refuse new subscriptions.
// Run under -race it also guards TC-1: runConn counted f.symbols after
// releasing f.mu while Subscribe writes it.
func TestFinnhubStreamLifecycle(t *testing.T) {
	t.Parallel()

	fake := &fakeFinnhub{conns: make(chan *websocket.Conn, 1)}
	srv := httptest.NewServer(fake.handler(t))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	f := NewFinnhub("KEY", "", wsURL, nil)
	if err := f.Subscribe("AAPL", ""); err != nil {
		t.Fatalf("Subscribe before connect: %v", err)
	}

	var conn *websocket.Conn
	select {
	case conn = <-fake.conns:
	case <-time.After(5 * time.Second):
		t.Fatal("client never dialled")
	}

	eventually(t, "resubscribe of AAPL on connect", func() bool { return fake.saw("subscribe", "AAPL") })

	// A connected client sends new subscriptions straight away.
	eventually(t, "connection registered", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()

		return f.conn != nil
	})

	if err := f.Subscribe("MSFT"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	eventually(t, "subscribe MSFT", func() bool { return fake.saw("subscribe", "MSFT") })

	if err := f.Unsubscribe("AAPL"); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}

	eventually(t, "unsubscribe AAPL", func() bool { return fake.saw("unsubscribe", "AAPL") })

	// A ping is ignored; a trade becomes a quote.
	_ = conn.WriteJSON(map[string]any{"type": "ping"})
	_ = conn.WriteJSON(map[string]any{
		"type": "trade",
		"data": []map[string]any{{"s": "MSFT", "p": 412.5, "v": 3, "t": int64(1_700_000_000_000)}},
	})

	select {
	case q := <-f.Quotes():
		if q.Symbol != "MSFT" || q.Price != 412.5 || !q.Time.Equal(time.UnixMilli(1_700_000_000_000)) {
			t.Errorf("quote = %+v", q)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no quote delivered")
	}

	if q, ok := f.LastQuote("MSFT"); !ok || q.Price != 412.5 {
		t.Errorf("LastQuote(MSFT) = %+v, %v", q, ok)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := f.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	eventually(t, "Quotes channel closed", func() bool {
		select {
		case _, open := <-f.Quotes():
			return !open
		default:
			return false
		}
	})

	if err := f.Subscribe("NVDA"); err == nil {
		t.Error("Subscribe after Close succeeded")
	}
}

// TestUnsubscribeStopsTracking guards the screener's drop path on the two
// offline feeds: a symbol removed from the watchlist must leave the
// provider's set, or the mock keeps ticking it and the replay keeps
// replaying it into an engine that no longer tracks it.
func TestUnsubscribeStopsTracking(t *testing.T) {
	t.Parallel()

	m := NewMock(time.Hour)
	t.Cleanup(func() { _ = m.Close() })

	_ = m.Subscribe("AAA", "BBB")
	_ = m.Unsubscribe("AAA", "NOTHELD")

	m.mu.Lock()
	_, aaa := m.prices["AAA"]
	_, bbb := m.prices["BBB"]
	m.mu.Unlock()

	if aaa || !bbb {
		t.Errorf("mock after Unsubscribe(AAA): AAA tracked=%v BBB tracked=%v", aaa, bbb)
	}

	if m.Quotes() == nil {
		t.Error("mock Quotes is nil")
	}

	r := NewReplay(map[string][]Candle{}, time.Hour)
	t.Cleanup(func() { _ = r.Close() })

	_ = r.Subscribe("AAA", "BBB")
	_ = r.Unsubscribe("BBB")

	r.mu.Lock()
	_, aaa = r.subscribed["AAA"]
	_, bbb = r.subscribed["BBB"]
	r.mu.Unlock()

	if !aaa || bbb {
		t.Errorf("replay after Unsubscribe(BBB): AAA=%v BBB=%v", aaa, bbb)
	}
}

// TestResampleEveryAnchorsOnTheNewestBar guards the higher-timeframe series:
// buckets are counted back from the NEWEST bar so the last aggregate is
// always complete (a half-filled bucket made the HTF reading flicker), the
// partial remainder is dropped from the OLD end, OHLCV aggregates correctly,
// and each bar is stamped with its last input's time — the moment it became
// knowable, never earlier (lookahead).
func TestResampleEveryAnchorsOnTheNewestBar(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)

	var in []Candle

	for i := range 7 {
		p := float64(10 + i)
		in = append(in, Candle{
			Time: base.Add(time.Duration(i) * time.Minute),
			Open: p, High: p + 0.5, Low: p - 0.5, Close: p + 0.2, Volume: 100,
		})
	}

	in[5].Low = 1 // the low of bucket 2 sits in its middle bar

	out := ResampleEvery(in, 3)
	if len(out) != 2 {
		t.Fatalf("7 bars by 3 -> %d bars, want 2 (the oldest one dropped)", len(out))
	}

	// Bucket 1 is bars 1..3, bucket 2 bars 4..6.
	b1, b2 := out[0], out[1]

	if b1.Open != 11 || b1.High != 13.5 || b1.Low != 10.5 || b1.Close != 13.2 || b1.Volume != 300 {
		t.Errorf("bucket 1 = %+v", b1)
	}

	if !b1.Time.Equal(in[3].Time) {
		t.Errorf("bucket 1 stamped %v, want its last bar's %v", b1.Time, in[3].Time)
	}

	if b2.Low != 1 || b2.Close != in[6].Close || !b2.Time.Equal(in[6].Time) {
		t.Errorf("bucket 2 = %+v", b2)
	}

	// The input is not modified.
	if in[1].High != 11.5 {
		t.Error("ResampleEvery mutated its input")
	}

	if got := ResampleEvery(in, 1); len(got) != len(in) {
		t.Error("n=1 is not the identity")
	}

	if got := ResampleEvery(in[:2], 3); got != nil {
		t.Errorf("fewer bars than n -> %v, want nil (no complete bar yet)", got)
	}

	if got := ResampleEvery(nil, 3); got != nil {
		t.Errorf("nil input -> %v", got)
	}
}

// TestResolutionStepsAgree guards the bar-length tables: the exported
// ResolutionDuration (used by the screener and the probe) and Binance's own
// kline step must agree on every resolution, and an unknown one must fall
// back to a minute rather than to zero — a zero step pages forever.
func TestResolutionStepsAgree(t *testing.T) {
	t.Parallel()

	want := map[Resolution]time.Duration{
		Res1Min: time.Minute, Res5Min: 5 * time.Minute, Res15Min: 15 * time.Minute,
		Res1Hour: time.Hour, Res1Day: 24 * time.Hour, "bogus": time.Minute,
	}

	for res, d := range want {
		if got := ResolutionDuration(res); got != d {
			t.Errorf("ResolutionDuration(%q) = %v, want %v", res, got, d)
		}

		if got := binanceStep(res); got != d {
			t.Errorf("binanceStep(%q) = %v, want %v", res, got, d)
		}
	}
}

// TestYahooEndpointsAndCacheUnwrap guards two small seams: Yahoo tries its
// mirror hosts in order unless a base URL is pinned (tests and proxies pin
// one and must not fall through to the real hosts), and CachingHistory
// exposes its source so a capability probe — the symbol picker's Searcher
// assertion — can see through the cache (the picker vanished in production
// without it).
func TestYahooEndpointsAndCacheUnwrap(t *testing.T) {
	t.Parallel()

	y := NewYahooHistory()

	got := y.endpoints()
	if len(got) != len(yahooHosts) || got[0] != "https://"+yahooHosts[0] {
		t.Errorf("endpoints = %v, want every mirror host in order", got)
	}

	y.base = "http://127.0.0.1:9"
	if got := y.endpoints(); len(got) != 1 || got[0] != y.base {
		t.Errorf("pinned endpoints = %v, want only the base", got)
	}

	c := NewCachingHistory(y, time.Minute)
	if c.Unwrap() != HistoryFetcher(y) {
		t.Error("Unwrap did not return the wrapped source")
	}

	if _, ok := c.Unwrap().(Searcher); !ok {
		t.Error("the unwrapped Yahoo source does not satisfy Searcher")
	}
}
