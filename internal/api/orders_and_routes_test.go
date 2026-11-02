package api

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// bracketBroker is a Sim that also places brackets, lists and cancels orders
// and resolves symbols, recording what reached it.
type bracketBroker struct {
	*broker.Sim

	mu        sync.Mutex
	brackets  []broker.BracketOrder
	cancelled []string
}

func (b *bracketBroker) PlaceBracket(_ context.Context, o broker.BracketOrder) (broker.BracketResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.brackets = append(b.brackets, o)

	return broker.BracketResult{EntryID: "E1"}, nil
}

func (b *bracketBroker) OpenOrders(context.Context) ([]broker.OpenOrder, error) {
	return []broker.OpenOrder{{OrderID: "O1", Symbol: "AAA"}}, nil
}

func (b *bracketBroker) CancelOrders(_ context.Context, ids ...string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.cancelled = append(b.cancelled, ids...)

	return nil
}

func (b *bracketBroker) ResolveSymbol(_ context.Context, symbol string) (string, error) {
	if symbol == "AAA" {
		return "AAA_US_EQ", nil
	}

	return "", errors.New("not found")
}

func orderLimits() config.Config {
	cfg := config.Config{}
	cfg.Limits = config.Limits{
		MaxOrderValue: 1e6, MaxPerPosition: 1e6, MaxTotalInvested: 1e6,
		MaxDailySpend: 1e6, MaxOpenPositions: 10,
	}

	return cfg
}

// TestPostOrderValidatesBeforeTheEngine guards the manual order endpoint, a
// path that places real orders: malformed JSON, an unknown side, a blank
// symbol and a non-positive quantity are refused with 400 before the engine
// sees them; the symbol and side are normalised; a valid order comes back
// as its trade record; and an engine refusal (no price yet) is a 400 the UI
// can show, not a 500 or a silent 200.
func TestPostOrderValidatesBeforeTheEngine(t *testing.T) {
	s := newTestServerFrom(t, nil, orderLimits())

	for _, body := range []string{
		`not json`,
		`{"symbol":"AAA","side":"short","qty":1}`,
		`{"symbol":"  ","side":"buy","qty":1}`,
		`{"symbol":"AAA","side":"buy","qty":0}`,
		`{"symbol":"AAA","side":"buy","qty":-3}`,
	} {
		if code, out := do(t, s, http.MethodPost, "/api/orders", body); code != http.StatusBadRequest {
			t.Errorf("POST /api/orders %s = %d %s, want 400", body, code, out)
		}
	}

	if code, out := do(t, s, http.MethodPost, "/api/orders", `{"symbol":"aaa","side":"BUY","qty":1}`); code != http.StatusBadRequest ||
		!strings.Contains(out, "no current price for AAA") {
		t.Errorf("order with no price = %d %s, want 400 naming the normalised symbol", code, out)
	}

	s.prices.Set("AAA", 10)
	s.pf.SetCash(10000)

	code, out := do(t, s, http.MethodPost, "/api/orders", `{"symbol":" aaa ","side":" Buy ","qty":2}`)
	if code != http.StatusOK {
		t.Fatalf("valid order = %d %s", code, out)
	}

	var rec struct {
		Symbol string  `json:"symbol"`
		Side   string  `json:"side"`
		Qty    float64 `json:"qty"`
	}
	if err := json.Unmarshal([]byte(out), &rec); err != nil || rec.Symbol != "AAA" || rec.Side != "buy" || rec.Qty != 2 {
		t.Errorf("trade record = %s (%v)", out, err)
	}
}

// TestPostBracketMapsTheRequest guards the manual bracket endpoint: it
// validates like /api/orders, maps entry_type "limit" (any case) to a limit
// entry and everything else to market, upper-cases the symbol, and passes
// stop and take-profit through untouched. A broker without brackets is a
// 400 naming it.
func TestPostBracketMapsTheRequest(t *testing.T) {
	plain := newTestServerFrom(t, nil, orderLimits())
	plain.prices.Set("AAA", 10)

	if code, out := do(t, plain, http.MethodPost, "/api/orders/bracket",
		`{"symbol":"AAA","side":"buy","qty":1}`); code != http.StatusBadRequest || !strings.Contains(out, "bracket") {
		t.Errorf("Sim bracket = %d %s, want 400", code, out)
	}

	brk := &bracketBroker{Sim: broker.NewSim(10000, func(string) (float64, bool) { return 10, true }, nil)}
	s := newTestServerFrom(t, brk, orderLimits())
	s.prices.Set("AAA", 10)
	s.pf.SetCash(10000)

	for _, body := range []string{
		`{`, `{"symbol":"AAA","side":"hold","qty":1}`, `{"symbol":"","side":"buy","qty":1}`,
		`{"symbol":"AAA","side":"buy","qty":0}`,
	} {
		if code, _ := do(t, s, http.MethodPost, "/api/orders/bracket", body); code != http.StatusBadRequest {
			t.Errorf("bracket %s = %d, want 400", body, code)
		}
	}

	if len(brk.brackets) != 0 {
		t.Fatalf("an invalid bracket reached the broker: %+v", brk.brackets)
	}

	code, out := do(t, s, http.MethodPost, "/api/orders/bracket", `{"symbol":" aaa ","side":"Buy","qty":2,
		"entry_type":"LIMIT","entry_price":9.5,"stop_loss":9,"take_profit":12,"duration":"DayOrder"}`)
	if code != http.StatusOK || !strings.Contains(out, `"entry_id":"E1"`) {
		t.Fatalf("bracket = %d %s", code, out)
	}

	got := brk.brackets[0]
	if got.Symbol != "AAA" || got.Side != broker.Buy || got.EntryType != broker.Limit || got.EntryPrice != 9.5 ||
		got.StopLoss != 9 || got.TakeProfit != 12 || got.Duration != "DayOrder" {
		t.Errorf("bracket at the broker = %+v", got)
	}

	_, _ = do(t, s, http.MethodPost, "/api/orders/bracket", `{"symbol":"AAA","side":"buy","qty":1,"entry_type":"stop"}`)

	if n := len(brk.brackets); n != 2 || brk.brackets[1].EntryType != broker.Market {
		t.Errorf("an unknown entry_type did not fall back to market: %+v", brk.brackets)
	}
}

// TestOrderListingEndpoints guards the order views and the cancel action:
// a broker that cannot list, resolve or cancel answers 400 with the reason
// (the UI hides the panel on it) rather than a 500, and a capable one
// returns its data — a comma-separated id path cancels every id.
func TestOrderListingEndpoints(t *testing.T) {
	plain := newTestServer(t)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/orderable"},
		{http.MethodGet, "/api/orders/open"},
		{http.MethodDelete, "/api/orders/O1"},
	} {
		if code, _ := do(t, plain, tc.method, tc.path, ""); code != http.StatusBadRequest {
			t.Errorf("%s %s on a Sim = %d, want 400", tc.method, tc.path, code)
		}
	}

	brk := &bracketBroker{Sim: broker.NewSim(10000, nil, nil)}
	s := newTestServerWith(t, brk)

	if code, out := do(t, s, http.MethodGet, "/api/orders/open", ""); code != http.StatusOK ||
		!strings.Contains(out, `"order_id":"O1"`) {
		t.Errorf("open orders = %d %s", code, out)
	}

	if code, out := do(t, s, http.MethodDelete, "/api/orders/O1,O2", ""); code != http.StatusOK ||
		!strings.Contains(out, `["O1","O2"]`) {
		t.Errorf("cancel = %d %s", code, out)
	}

	if strings.Join(brk.cancelled, ",") != "O1,O2" {
		t.Errorf("cancelled at the broker = %v", brk.cancelled)
	}

	if code, out := do(t, s, http.MethodGet, "/api/orderable", ""); code != http.StatusOK || out != "[]" {
		t.Errorf("orderable with an empty watchlist = %d %s", code, out)
	}
}

// TestWatchlistEndpoints guards adding and removing symbols through the
// API: symbols are trimmed and upper-cased (a lower-case "aapl" would be a
// second, never-priced symbol), blanks are dropped, an empty request is a
// 400, and DELETE normalises the same way so it removes what POST added.
func TestWatchlistEndpoints(t *testing.T) {
	s := newTestServer(t)

	for _, body := range []string{`{}`, `{"symbols":[]}`, `nope`} {
		if code, _ := do(t, s, http.MethodPost, "/api/watchlist", body); code != http.StatusBadRequest {
			t.Errorf("POST /api/watchlist %s = %d, want 400", body, code)
		}
	}

	code, out := do(t, s, http.MethodPost, "/api/watchlist", `{"symbols":[" aaa ","", "bbb"]}`)
	if code != http.StatusOK || !strings.Contains(out, `["AAA","BBB"]`) {
		t.Fatalf("add = %d %s", code, out)
	}

	if code, out := do(t, s, http.MethodGet, "/api/watchlist", ""); code != http.StatusOK ||
		!strings.Contains(out, `["AAA","BBB"]`) {
		t.Errorf("GET /api/watchlist = %d %s", code, out)
	}

	if code, out := do(t, s, http.MethodDelete, "/api/watchlist/%20aaa", ""); code != http.StatusOK ||
		!strings.Contains(out, `["BBB"]`) {
		t.Errorf("remove = %d %s", code, out)
	}
}

// TestEveryReadRouteAnswersJSON guards the read-only API surface the UI and
// any script polls: every GET route must answer 200 with valid JSON on a
// fresh bot — a nil map or an unconfigured feature (no ML, no screener, no
// protections) must render as empty data, never a panic or a 500.
func TestEveryReadRouteAnswersJSON(t *testing.T) {
	s := newTestServer(t)

	for _, path := range []string{
		"/api/dashboard", "/api/status", "/api/quotes", "/api/positions", "/api/open",
		"/api/signals", "/api/trades", "/api/stats", "/api/stats?period=month", "/api/metrics",
		"/api/watchlist", "/api/limits", "/api/strategy", "/api/exits", "/api/sizing",
		"/api/behavior", "/api/detectors", "/api/ai", "/api/ml", "/api/errors", "/api/unconfirmed",
		"/api/chart/AAA", "/api/screener", "/api/notify", "/api/protections",
	} {
		code, body := do(t, s, http.MethodGet, path, "")
		if code != http.StatusOK {
			t.Errorf("GET %s = %d %s", path, code, body)

			continue
		}

		if !json.Valid([]byte(body)) {
			t.Errorf("GET %s returned invalid JSON: %.80s", path, body)
		}
	}

	if _, body := do(t, s, http.MethodGet, "/api/stats?period=month", ""); !strings.Contains(body, `"period":"month"`) {
		t.Errorf("stats ignored ?period: %s", body)
	}
}

// TestUnconfirmedAndUnlockActions guards two operator actions: clearing an
// unconfirmed order that does not exist is a 404 (a typo must not read as
// "reconciled"), and unlocking a symbol answers with the normalised symbol
// even with protections disabled (the nil-safe manager).
func TestUnconfirmedAndUnlockActions(t *testing.T) {
	s := newTestServer(t)

	if code, _ := do(t, s, http.MethodDelete, "/api/unconfirmed/nope", ""); code != http.StatusNotFound {
		t.Errorf("clearing an unknown unconfirmed order = %d, want 404", code)
	}

	if code, out := do(t, s, http.MethodDelete, "/api/protections/lock/aaa", ""); code != http.StatusOK ||
		!strings.Contains(out, `"unlocked":"AAA"`) {
		t.Errorf("unlock = %d %s", code, out)
	}
}

// TestGetChartWindowsTheHistory guards the chart endpoint: ?bars bounds the
// history (default 240, capped at 1000, junk ignored), and trade markers are
// limited to the window on show and to the symbol asked for.
func TestGetChartWindowsTheHistory(t *testing.T) {
	s := newTestServer(t)

	if err := s.eng.AddSymbols("AAA"); err != nil {
		t.Fatal(err)
	}

	type chart struct {
		Symbol  string          `json:"symbol"`
		Candles []market.Candle `json:"candles"`
		Trades  []chartTrade    `json:"trades"`
	}

	get := func(q string) chart {
		t.Helper()

		code, body := do(t, s, http.MethodGet, "/api/chart/aaa"+q, "")
		if code != http.StatusOK {
			t.Fatalf("GET chart%s = %d %s", q, code, body)
		}

		var c chart
		if err := json.Unmarshal([]byte(body), &c); err != nil {
			t.Fatal(err)
		}

		return c
	}

	all := len(s.eng.Series("AAA"))
	if all < 300 {
		t.Fatalf("setup: only %d warmup bars", all)
	}

	if c := get(""); c.Symbol != "AAA" || len(c.Candles) != 240 {
		t.Errorf("default window: %s, %d candles, want AAA and 240", c.Symbol, len(c.Candles))
	}

	if c := get("?bars=50"); len(c.Candles) != 50 {
		t.Errorf("?bars=50 -> %d candles", len(c.Candles))
	}

	if c := get("?bars=junk"); len(c.Candles) != 240 {
		t.Errorf("?bars=junk -> %d candles, want the 240 default", len(c.Candles))
	}

	if c := get("?bars=999999"); len(c.Candles) != min(all, 1000) {
		t.Errorf("?bars=999999 -> %d candles, want min(%d, 1000)", len(c.Candles), all)
	}
}

// TestGzipWriterKeepsTheBodyIntact guards the JSON compression middleware's
// writer: a client asking for gzip must get a gzip body that decompresses
// to the same JSON, and no stale Content-Length (a length set for the
// uncompressed body truncates or hangs the client).
func TestGzipWriterKeepsTheBodyIntact(t *testing.T) {
	s := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/watchlist", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q", rec.Header().Get("Content-Encoding"))
	}

	if rec.Header().Get("Content-Length") != "" {
		t.Errorf("a compressed response kept Content-Length %q", rec.Header().Get("Content-Length"))
	}

	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}

	plain, _ := io.ReadAll(zr)
	if !json.Valid(plain) || !strings.Contains(string(plain), `"symbols"`) {
		t.Errorf("decompressed body = %s", plain)
	}

	// WriteString takes the same path.
	rec = httptest.NewRecorder()
	gz := gzip.NewWriter(rec)
	c, _ := gin.CreateTestContext(rec)
	w := &gzipWriter{ResponseWriter: c.Writer, gz: gz}

	if _, err := w.WriteString(`{"a":1}`); err != nil {
		t.Fatal(err)
	}

	_ = gz.Close()

	zr, _ = gzip.NewReader(rec.Body)
	if b, _ := io.ReadAll(zr); string(b) != `{"a":1}` {
		t.Errorf("WriteString round trip = %q", b)
	}
}

// TestChartFragment guards the v2 chart screen: with nothing watched it
// says so instead of rendering an empty plot; with a watched symbol and no
// ?symbol it falls back to the first one; and a window too dense for candle
// bodies draws a single close line (drawCloseLine) rather than sub-pixel
// candles nobody can read.
func TestChartFragment(t *testing.T) {
	s := newTestServer(t)

	if body := fetch(t, s, "/v2/f/chart").Body.String(); !strings.Contains(body, "No symbols on the watchlist yet") {
		t.Errorf("empty watchlist chart: %.200s", body)
	}

	if err := s.eng.AddSymbols("AAA"); err != nil {
		t.Fatal(err)
	}

	body := fetch(t, s, "/v2/f/chart?symbol=nope&bars=60").Body.String()
	if !strings.Contains(body, "<svg") || !strings.Contains(body, "AAA: 60 bars") {
		t.Errorf("fallback to the first symbol: %.300s", body)
	}

	if strings.Contains(body, `class="line `) {
		t.Error("60 bars were drawn as a line, want candles")
	}

	if body := fetch(t, s, "/v2/f/chart?symbol=AAA&bars=1000").Body.String(); !strings.Contains(body, `class="line `) {
		t.Error("a dense window did not fall back to a close line")
	}
}

// TestDrawCloseLine guards the dense-chart path directly: one path through
// every close, starting with a move-to, coloured by the window's direction.
func TestDrawCloseLine(t *testing.T) {
	t.Parallel()

	var b strings.Builder

	candles := []market.Candle{{Close: 1}, {Close: 3}, {Close: 2}}
	drawCloseLine(&b, candles, func(i int) float64 { return float64(i) }, func(p float64) float64 { return p })

	got := b.String()
	if !strings.Contains(got, `d="M0.00 1.00L1.00 3.00L2.00 2.00"`) {
		t.Errorf("path = %s", got)
	}

	if !strings.Contains(got, `class="line `+upDown(true)) {
		t.Errorf("a rising window is not classed as rising: %s", got)
	}

	b.Reset()
	drawCloseLine(&b, []market.Candle{{Close: 3}, {Close: 1}},
		func(i int) float64 { return float64(i) }, func(p float64) float64 { return p })

	if !strings.Contains(b.String(), `class="line `+upDown(false)) {
		t.Errorf("a falling window is not classed as falling: %s", b.String())
	}
}

// TestChartTradesOnlyInTheWindow guards the chart's markers: only this
// symbol's priced trades inside the visible window are drawn — a trade
// before the first candle would be pinned to the left edge as if it
// happened there.
func TestChartTradesOnlyInTheWindow(t *testing.T) {
	s := newTestServer(t)

	if got := s.chartTrades("AAA", nil); got != nil {
		t.Errorf("no candles -> %v, want nil", got)
	}

	s.prices.Set("AAA", 10)
	_, _ = do(t, s, http.MethodPost, "/api/orders", `{"symbol":"AAA","side":"buy","qty":1}`)

	now := time.Now()
	inWindow := []market.Candle{{Time: now.Add(-time.Hour)}, {Time: now.Add(time.Hour)}}
	afterIt := []market.Candle{{Time: now.Add(time.Hour)}, {Time: now.Add(2 * time.Hour)}}

	if got := s.chartTrades("AAA", inWindow); len(got) != 1 || got[0].Price != 10 {
		t.Errorf("trade inside the window: %+v", got)
	}

	if got := s.chartTrades("AAA", afterIt); len(got) != 0 {
		t.Errorf("trade before the window was drawn: %+v", got)
	}

	if got := s.chartTrades("BBB", inWindow); len(got) != 0 {
		t.Errorf("another symbol's trade was drawn: %+v", got)
	}
}

// TestFormHelpers guards the round trip between the settings forms and the
// config: maps render as sorted "k:v" pairs (so an unchanged form posts
// back the same text), floats lose only trailing zeros, scale-out rungs
// parse as profit:fraction pairs and refuse anything else with the field's
// name, and parseIntDefault ignores junk and non-positive values.
func TestFormHelpers(t *testing.T) {
	t.Parallel()

	if got := formatKV(map[string]string{"b": "2", "a": "1"}); got != "a:1, b:2" {
		t.Errorf("formatKV = %q", got)
	}

	if formatKV(nil) != "" || formatKVFloat(nil) != "" || joinPairs(nil) != "" {
		t.Error("an empty map did not render empty")
	}

	if got := formatKVFloat(map[string]float64{"rsi": 0.5, "ema_cross": 2, "x": 0.1234567}); got != "ema_cross:2, rsi:0.5, x:0.123457" {
		t.Errorf("formatKVFloat = %q", got)
	}

	if got := joinPairs(map[string]any{"z": 0.33, "a": "x"}); got != "a:x, z:0.33" {
		t.Errorf("joinPairs = %q", got)
	}

	s := []string{"c", "a", "b", "a"}
	sortStrings(s)

	if strings.Join(s, "") != "aabc" {
		t.Errorf("sortStrings = %v", s)
	}

	fl := field{Name: "scale_out"}

	rungs, err := decodeRungs(fl, "0.05:0.33, 0.10 : 0.5")
	if err != nil || len(rungs) != 2 {
		t.Fatalf("decodeRungs = %v, %v", rungs, err)
	}

	if r := rungs[1].(map[string]any); r["profit_pct"] != 0.10 || r["sell_pct"] != 0.5 {
		t.Errorf("second rung = %v", r)
	}

	for _, bad := range []string{"0.05", "a:b", "0.05:x"} {
		if _, err := decodeRungs(fl, bad); err == nil || !strings.Contains(err.Error(), "scale_out") {
			t.Errorf("decodeRungs(%q) err = %v, want a refusal naming the field", bad, err)
		}
	}

	if r, err := decodeRungs(fl, ""); err != nil || r != nil {
		t.Errorf("empty rungs = %v, %v", r, err)
	}

	for raw, want := range map[string]int{"12": 12, "": 7, "x": 7, "0": 7, "-3": 7} {
		if got := parseIntDefault(raw, 7); got != want {
			t.Errorf("parseIntDefault(%q) = %d, want %d", raw, got, want)
		}
	}
}

// TestHumanAgo guards the "how long ago" labels: the zero time is a dash,
// not "56 years ago", each unit switches at its boundary, and a time in the
// future (clock skew) reads as a distance rather than a negative number.
func TestHumanAgo(t *testing.T) {
	t.Parallel()

	now := time.Now()

	for in, want := range map[time.Time]string{
		{}:                         "—",
		now.Add(-30 * time.Second): "30s ago",
		now.Add(-5 * time.Minute):  "5m ago",
		now.Add(-3 * time.Hour):    "3h ago",
		now.Add(-50 * time.Hour):   "2d ago",
		now.Add(90 * time.Second):  "1m ago",
	} {
		if got := humanAgo(in); got != want {
			t.Errorf("humanAgo(%v) = %q, want %q", now.Sub(in), got, want)
		}
	}
}

// TestDetectorMatrixFillsEveryCell guards the detector table: every row
// has a cell in every column even when a symbol's detectors differ, a
// missing reading renders as a dash, and rows and columns are sorted so
// the table does not reshuffle between refreshes.
func TestDetectorMatrixFillsEveryCell(t *testing.T) {
	t.Parallel()

	names, rows := detectorMatrix(map[string][]strategy.DetectorResult{
		"ZZZ": {{Name: "rsi", Action: strategy.Buy, Enabled: true}},
		"AAA": {{Name: "macd", Action: strategy.Sell, Reason: "cross"}, {Name: "rsi", Action: strategy.Hold}},
	})

	if strings.Join(names, ",") != "macd,rsi" {
		t.Fatalf("columns = %v", names)
	}

	if len(rows) != 2 || rows[0].Symbol != "AAA" || rows[1].Symbol != "ZZZ" {
		t.Fatalf("rows = %+v", rows)
	}

	if c := rows[0].Cells[0]; c.Action != string(strategy.Sell) || c.Reason != "cross" {
		t.Errorf("AAA/macd = %+v", c)
	}

	if c := rows[1].Cells[0]; c.Action != "—" {
		t.Errorf("ZZZ/macd = %+v, want a dash", c)
	}

	if c := rows[1].Cells[1]; !c.Enabled || c.Action != string(strategy.Buy) {
		t.Errorf("ZZZ/rsi = %+v", c)
	}
}

// TestRunServesAndShutsDown guards the HTTP lifecycle cmd/bot depends on:
// Run must return nil once its context is cancelled (a clean shutdown, so
// run's defers save state), and a listener that cannot start must come back
// as an error rather than leave the bot running with no API.
func TestRunServesAndShutsDown(t *testing.T) {
	s := newTestServer(t)

	if err := s.Run(context.Background(), "127.0.0.1:-1"); err == nil {
		t.Error("Run on an invalid address returned nil")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() { done <- s.Run(ctx, "127.0.0.1:0") }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run after cancel = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}
