package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// t212Mock serves the handful of endpoints the adapter uses and records the
// last order body it received.
type t212Mock struct {
	srv        *httptest.Server
	lastBody   map[string]any
	lastAuth   string
	lastCancel string
}

func newT212Mock(t *testing.T) *t212Mock {
	m := &t212Mock{}
	mux := http.NewServeMux()
	record := func(r *http.Request) { m.lastAuth = r.Header.Get("Authorization") }
	mux.HandleFunc("/equity/metadata/instruments", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"ticker": "AAPL_US_EQ", "shortName": "AAPL", "isin": "US0378331005", "type": "STOCK"},
			{"ticker": "AAPL_GB_EQ", "shortName": "AAPL", "isin": "US0378331005", "type": "STOCK"},
			{"ticker": "VUSA_EQ", "shortName": "VUSA", "isin": "IE00B3XXRP09", "type": "ETF"},
			{"ticker": "LXSd_EQ", "shortName": "LXS", "isin": "DE0005470405", "type": "STOCK"},
			{"ticker": "SAPd_EQ", "shortName": "SAP", "isin": "DE0007164600", "type": "STOCK"},
			{"ticker": "SAP_CA_EQ", "shortName": "SAP", "isin": "CA8029121057", "type": "STOCK"}, // Saputo (ambiguous)
			{"ticker": "SAP_US_EQ", "shortName": "SAP", "isin": "US8030542042", "type": "STOCK"},
			{"ticker": "SLABOND_EQ", "shortName": "SLA", "isin": "DE000SLA7M77", "type": "STOCK"},
			// Same ISIN, three currencies — EUR should win.
			{"ticker": "PRJUl_EQ", "shortName": "PRJU", "isin": "LU1931974775", "currencyCode": "USD", "type": "ETF"},
			{"ticker": "PRIJl_EQ", "shortName": "PRIJ", "isin": "LU1931974775", "currencyCode": "GBX", "type": "ETF"},
			{"ticker": "PR1Jd_EQ", "shortName": "PR1J", "isin": "LU1931974775", "currencyCode": "EUR", "type": "ETF"},
		})
	})
	mux.HandleFunc("POST /equity/orders/market", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		_ = json.NewDecoder(r.Body).Decode(&m.lastBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 12345, "ticker": m.lastBody["ticker"], "quantity": m.lastBody["quantity"],
			"filledQuantity": 0, "status": "SUBMITTED",
		})
	})
	mux.HandleFunc("/equity/account/cash", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"free": 900.0, "total": 1000.0, "invested": 100.0})
	})
	mux.HandleFunc("/equity/account/info", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"currencyCode": "EUR"})
	})
	mux.HandleFunc("/equity/portfolio", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"ticker": "AAPL_US_EQ", "quantity": 2.0, "averagePrice": 150.0, "currentPrice": 160.0, "ppl": 20.0},
		})
	})
	mux.HandleFunc("GET /equity/orders", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 777, "ticker": "AAPL_US_EQ", "quantity": -5.0, "type": "LIMIT", "status": "WORKING", "limitPrice": 200.0},
		})
	})
	mux.HandleFunc("DELETE /equity/orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		m.lastCancel = r.PathValue("id")
		w.WriteHeader(http.StatusOK)
	})
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

// fresh returns a client with pacing reset so back-to-back calls don't sleep.
func (m *t212Mock) fresh() *Trading212 {
	c := NewTrading212(m.srv.URL, "key", "secret", map[string]string{"PRAJL.XC": "LU1931974775"}, "EUR")
	c.lastCall = map[string]time.Time{}
	return c
}

func TestT212OrderSignedQuantity(t *testing.T) {
	m := newT212Mock(t)
	ctx := context.Background()

	res, err := m.fresh().SubmitOrder(ctx, Order{Symbol: "AAPL", Side: Buy, Qty: 3, Type: Market})
	if err != nil {
		t.Fatalf("buy: %v", err)
	}
	if m.lastBody["ticker"] != "AAPL_US_EQ" {
		t.Errorf("ticker not resolved/US-preferred: %v", m.lastBody["ticker"])
	}
	if q, _ := m.lastBody["quantity"].(float64); q != 3 {
		t.Errorf("buy quantity = %v, want +3", m.lastBody["quantity"])
	}
	if res.ID != "12345" || res.Side != Buy || res.Qty != 3 {
		t.Errorf("unexpected result: %+v", res)
	}
	// HTTP Basic auth: base64("key:secret") == "a2V5OnNlY3JldA==".
	if m.lastAuth != "Basic a2V5OnNlY3JldA==" {
		t.Errorf("auth header = %q, want Basic base64(key:secret)", m.lastAuth)
	}

	if _, err := m.fresh().SubmitOrder(ctx, Order{Symbol: "AAPL", Side: Sell, Qty: 3, Type: Market}); err != nil {
		t.Fatalf("sell: %v", err)
	}
	if q, _ := m.lastBody["quantity"].(float64); q != -3 {
		t.Errorf("sell quantity = %v, want -3 (signed)", m.lastBody["quantity"])
	}
}

func TestT212SellQuantityRoundedDown(t *testing.T) {
	m := newT212Mock(t)
	ctx := context.Background()

	// A fractional position quantity (100/27) must be sent rounded DOWN to 8 dp,
	// never above the owned amount, so Trading 212 doesn't reject it as "selling
	// more than owned".
	owned := 100.0 / 27.0 // 3.7037037037037037
	if _, err := m.fresh().SubmitOrder(ctx, Order{Symbol: "AAPL", Side: Sell, Qty: owned, Type: Market}); err != nil {
		t.Fatalf("sell: %v", err)
	}
	q, _ := m.lastBody["quantity"].(float64)
	if q >= 0 {
		t.Fatalf("sell quantity %v should be negative (signed)", q)
	}
	sent := -q
	if sent > owned {
		t.Errorf("sent %v exceeds owned %v — would be rejected as over-sell", sent, owned)
	}
	if want := 3.7037037; sent != want {
		t.Errorf("sent %v, want %v (floored to 8 dp)", sent, want)
	}

	// A quantity that rounds to zero is rejected rather than sent as a 0-order.
	if _, err := m.fresh().SubmitOrder(ctx, Order{Symbol: "AAPL", Side: Sell, Qty: 1e-12, Type: Market}); err == nil {
		t.Error("a sub-dust quantity should be rejected, not sent")
	}
}

func decimals(v float64) int {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return len(s) - i - 1
	}
	return 0
}

func TestT212QuantityPrecisionRetry(t *testing.T) {
	// An instrument whose quantity precision is 2: the server rejects any order
	// carrying more than 2 decimals with the API's precision-mismatch error.
	const maxDP = 2
	var attempts int
	var accepted float64
	mux := http.NewServeMux()
	mux.HandleFunc("/equity/metadata/instruments", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"ticker": "AAPL_US_EQ", "shortName": "AAPL", "isin": "US0378331005", "type": "STOCK"},
		})
	})
	mux.HandleFunc("POST /equity/orders/market", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		attempts++
		q, _ := body["quantity"].(float64)
		if decimals(q) > maxDP {
			w.WriteHeader(http.StatusBadRequest)
			// The real API names the instrument's allowed precision in the error.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type": "/api-errors/quantity-precision-mismatch", "status": 400,
				"detail": "invalid quantity precision " + strconv.Itoa(maxDP),
			})
			return
		}
		accepted = q
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 1, "ticker": body["ticker"], "quantity": q, "filledQuantity": 0, "status": "SUBMITTED"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewTrading212(srv.URL, "k", "s", nil, "EUR")
	c.lastCall = map[string]time.Time{}
	ctx := context.Background()

	owned := 100.0 / 27.0 // 3.7037037037037037
	res, err := c.SubmitOrder(ctx, Order{Symbol: "AAPL", Side: Sell, Qty: owned, Type: Market})
	if err != nil {
		t.Fatalf("sell should eventually succeed after lowering precision: %v", err)
	}
	if attempts != 2 {
		t.Errorf("expected 2 attempts (reject, then jump to reported precision), got %d", attempts)
	}
	if decimals(accepted) > maxDP {
		t.Errorf("accepted quantity %v has more than %d decimals", accepted, maxDP)
	}
	if -accepted > owned { // accepted is negative (sell); magnitude must not exceed owned
		t.Errorf("sent magnitude %v exceeds owned %v", -accepted, owned)
	}
	if res.Qty != 3.70 {
		t.Errorf("result qty = %v, want 3.70 (floored to 2 dp)", res.Qty)
	}

	// The learned precision is cached: a second order resolves in one attempt.
	attempts = 0
	if _, err := c.SubmitOrder(ctx, Order{Symbol: "AAPL", Side: Sell, Qty: owned, Type: Market}); err != nil {
		t.Fatalf("second sell: %v", err)
	}
	if attempts != 1 {
		t.Errorf("cached precision should need exactly 1 attempt, got %d", attempts)
	}
}

func TestT212AccountAndPositions(t *testing.T) {
	m := newT212Mock(t)
	ctx := context.Background()

	acct, err := m.fresh().GetAccount(ctx)
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	if acct.Cash != 900 || acct.Equity != 1000 || acct.Currency != "EUR" {
		t.Errorf("account mapping wrong: %+v", acct)
	}

	pos, err := m.fresh().GetPositions(ctx)
	if err != nil {
		t.Fatalf("positions: %v", err)
	}
	if len(pos) != 1 || pos[0].Symbol != "AAPL" || pos[0].MarketValue != 320 || pos[0].UnrealizedPL != 20 {
		t.Errorf("position mapping wrong: %+v", pos)
	}
}

func TestT212OpenOrdersAndCancel(t *testing.T) {
	m := newT212Mock(t)
	ctx := context.Background()

	orders, err := m.fresh().OpenOrders(ctx)
	if err != nil {
		t.Fatalf("open orders: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("want 1 order, got %d", len(orders))
	}
	o := orders[0]
	if o.OrderID != "777" || o.Symbol != "AAPL" || o.Side != Sell || o.Qty != 5 || o.Price != 200 || o.Type != "LIMIT" {
		t.Errorf("order mapping wrong: %+v", o)
	}

	if err := m.fresh().CancelOrders(ctx, "777"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if m.lastCancel != "777" {
		t.Errorf("cancel hit id %q, want 777", m.lastCancel)
	}

	// Trading 212 must satisfy the list/cancel capabilities but not bracket.
	var b Broker = m.fresh()
	if _, ok := b.(OrderLister); !ok {
		t.Error("Trading212 should implement OrderLister")
	}
	if _, ok := b.(OrderCanceller); !ok {
		t.Error("Trading212 should implement OrderCanceller")
	}
	if _, ok := b.(BracketBroker); ok {
		t.Error("Trading212 should NOT implement BracketBroker")
	}
}

func TestT212ResolveExchanges(t *testing.T) {
	m := newT212Mock(t)
	ctx := context.Background()
	c := m.fresh()

	cases := map[string]string{
		"LXS.DE":           "LXSd_EQ",     // German listing via .DE suffix
		"SAP.DE":           "SAPd_EQ",     // disambiguate German SAP, not Saputo (CA)
		"SAP":              "SAP_US_EQ",   // no suffix => prefer US listing
		"AAPL":             "AAPL_US_EQ",  // US preferred
		"DE000SLA7M77.SG":  "SLABOND_EQ",  // ISIN match
		"LXSd_EQ":          "LXSd_EQ",     // already a ticker, passes through
	}
	for in, want := range cases {
		got, err := c.ResolveSymbol(ctx, in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}

	// Reverse mapping: a position ticker maps back to the input symbol used.
	c2 := m.fresh()
	if _, err := c2.ResolveSymbol(ctx, "LXS.DE"); err != nil {
		t.Fatal(err)
	}
	if s := c2.symbolFor("LXSd_EQ"); s != "LXS.DE" {
		t.Errorf("symbolFor(LXSd_EQ) = %q, want LXS.DE", s)
	}

	// A symbol with no listing is reported as unresolvable.
	if _, err := c.ResolveSymbol(ctx, "NOPE.DE"); err == nil {
		t.Error("expected error for unknown symbol")
	}
}

func TestT212ISINOverrideAndCurrency(t *testing.T) {
	m := newT212Mock(t)
	c := m.fresh() // configured with isins{"PRAJL.XC":"LU1931974775"}, preferCcy EUR

	// The .XC symbol isn't on T212 by ticker, but the ISIN override resolves it,
	// and among the three currency listings the EUR one is chosen.
	got, err := c.ResolveSymbol(context.Background(), "PRAJL.XC")
	if err != nil {
		t.Fatalf("resolve PRAJL.XC: %v", err)
	}
	if got != "PR1Jd_EQ" {
		t.Errorf("PRAJL.XC -> %s, want PR1Jd_EQ (EUR listing)", got)
	}
}

func TestT212PassThroughTicker(t *testing.T) {
	c := NewTrading212("http://x", "k", "s", nil, "EUR")
	tk, err := c.resolveTicker(context.Background(), "VUSA_EQ")
	if err != nil || tk != "VUSA_EQ" {
		t.Errorf("ticker with '_' should pass through, got %q err %v", tk, err)
	}
}
