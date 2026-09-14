package broker

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/errlog"
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
			{
				"ticker":    "SAP_CA_EQ",
				"shortName": "SAP",
				"isin":      "CA8029121057",
				"type":      "STOCK",
			}, // Saputo (ambiguous)
			{"ticker": "SAP_US_EQ", "shortName": "SAP", "isin": "US8030542042", "type": "STOCK"},
			{"ticker": "SLABOND_EQ", "shortName": "SLA", "isin": "DE000SLA7M77", "type": "STOCK"},
			// Same ISIN, three currencies — EUR should win.
			{
				"ticker":       "PRJUl_EQ",
				"shortName":    "PRJU",
				"isin":         "LU1931974775",
				"currencyCode": "USD",
				"type":         "ETF",
			},
			{
				"ticker":       "PRIJl_EQ",
				"shortName":    "PRIJ",
				"isin":         "LU1931974775",
				"currencyCode": "GBX",
				"type":         "ETF",
			},
			{
				"ticker":       "PR1Jd_EQ",
				"shortName":    "PR1J",
				"isin":         "LU1931974775",
				"currencyCode": "EUR",
				"type":         "ETF",
			},
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
		_ = json.NewEncoder(w).
			Encode(map[string]any{"free": 900.0, "total": 1000.0, "invested": 100.0})
	})
	mux.HandleFunc("/equity/account/info", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"currencyCode": "EUR"})
	})
	mux.HandleFunc("/equity/portfolio", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"ticker":       "AAPL_US_EQ",
				"quantity":     2.0,
				"averagePrice": 150.0,
				"currentPrice": 160.0,
				"ppl":          20.0,
			},
		})
	})
	mux.HandleFunc("GET /equity/orders", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"id":         777,
				"ticker":     "AAPL_US_EQ",
				"quantity":   -5.0,
				"type":       "LIMIT",
				"status":     "WORKING",
				"limitPrice": 200.0,
			},
		})
	})
	// Fill polling: report the last submitted order as fully filled at 100/share.
	mux.HandleFunc("GET /equity/orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.PathValue("id"))
		q, _ := m.lastBody["quantity"].(float64)
		abs := q
		if abs < 0 {
			abs = -abs
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "ticker": m.lastBody["ticker"], "quantity": q,
			"filledQuantity": q, "filledValue": abs * 100.0, "status": "FILLED",
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
	c := NewTrading212(
		m.srv.URL,
		"key",
		"secret",
		map[string]string{"PRAJL.XC": "LU1931974775"},
		"EUR",
	)
	c.limiters = map[string]*t212RateLimiter{}
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

	if _, err := m.fresh().
		SubmitOrder(ctx, Order{Symbol: "AAPL", Side: Sell, Qty: 3, Type: Market}); err != nil {
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
	if _, err := m.fresh().
		SubmitOrder(ctx, Order{Symbol: "AAPL", Side: Sell, Qty: owned, Type: Market}); err != nil {
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
	if _, err := m.fresh().
		SubmitOrder(ctx, Order{Symbol: "AAPL", Side: Sell, Qty: 1e-12, Type: Market}); err == nil {
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
			"id":             1,
			"ticker":         body["ticker"],
			"quantity":       q,
			"filledQuantity": 0,
			"status":         "SUBMITTED",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewTrading212(srv.URL, "k", "s", nil, "EUR")
	c.limiters = map[string]*t212RateLimiter{}
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
	if _, err := c.SubmitOrder(
		ctx,
		Order{Symbol: "AAPL", Side: Sell, Qty: owned, Type: Market},
	); err != nil {
		t.Fatalf("second sell: %v", err)
	}
	if attempts != 1 {
		t.Errorf("cached precision should need exactly 1 attempt, got %d", attempts)
	}
}

// fillTestServer serves the minimum for one AAPL market order, with a custom
// handler for the fill-polling endpoint (nil = let the mux 404 it).
func fillTestServer(t *testing.T, orderStatus http.HandlerFunc) *Trading212 {
	mux := http.NewServeMux()
	mux.HandleFunc("/equity/metadata/instruments", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"ticker": "AAPL_US_EQ", "shortName": "AAPL", "isin": "US0378331005", "type": "STOCK"},
		})
	})
	mux.HandleFunc("POST /equity/orders/market", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 9, "ticker": body["ticker"], "quantity": body["quantity"],
			"filledQuantity": 0, "status": "NEW",
		})
	})
	if orderStatus != nil {
		mux.HandleFunc("GET /equity/orders/{id}", orderStatus)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := NewTrading212(srv.URL, "k", "s", nil, "EUR")
	c.limiters = map[string]*t212RateLimiter{}
	return c
}

func TestT212ConfirmFillPolling(t *testing.T) {
	// The POST answers NEW/0 (async fill). The order endpoint reports NEW on the
	// first poll and FILLED with real quantity + filledValue on the second — the
	// result must carry the true fill, not the submission snapshot.
	var polls int
	c := fillTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		polls++
		if polls == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 9, "ticker": "AAPL_US_EQ", "quantity": 2.0,
				"filledQuantity": 0, "status": "NEW",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 9, "ticker": "AAPL_US_EQ", "quantity": 2.0,
			"filledQuantity": 2.0, "filledValue": 391.0, "status": "FILLED",
		})
	})

	res, err := c.SubmitOrder(
		context.Background(),
		Order{Symbol: "AAPL", Side: Buy, Qty: 2, Type: Market},
	)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if polls < 2 {
		t.Errorf("expected at least 2 polls (NEW then FILLED), got %d", polls)
	}
	if res.FilledQty != 2 {
		t.Errorf("FilledQty = %v, want 2 (the confirmed fill)", res.FilledQty)
	}
	if res.FilledPx != 195.5 {
		t.Errorf("FilledPx = %v, want 195.5 (filledValue/filledQuantity)", res.FilledPx)
	}
	if res.Status != "filled" {
		t.Errorf("Status = %q, want filled", res.Status)
	}
}

// TestT212SubmitOrderConvertsFilledPx verifies that a market order's fill
// price, reported by Trading 212 in the instrument's native currency, is
// converted to preferCcy before being handed back — otherwise the caller
// would combine it with account-currency money values (spend limits,
// realized PnL) that already went through this same conversion.
func TestT212SubmitOrderConvertsFilledPx(t *testing.T) {
	fx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"base": "USD", "rates": map[string]float64{"EUR": 0.86},
		})
	}))
	t.Cleanup(fx.Close)

	mux := http.NewServeMux()
	mux.HandleFunc("/equity/metadata/instruments", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"ticker": "NVDA_US_EQ", "shortName": "NVDA", "isin": "US67066G1040",
				"currencyCode": "USD", "type": "STOCK",
			},
		})
	})
	mux.HandleFunc("POST /equity/orders/market", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 9, "ticker": body["ticker"], "quantity": body["quantity"],
			"filledQuantity": 0, "status": "NEW",
		})
	})
	mux.HandleFunc("GET /equity/orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 9, "ticker": "NVDA_US_EQ", "quantity": 1.0,
			"filledQuantity": 1.0, "filledValue": 224.72, "status": "FILLED",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewTrading212(srv.URL, "key", "secret", nil, "EUR")
	c.fx.baseURL = fx.URL
	c.limiters = map[string]*t212RateLimiter{}

	res, err := c.SubmitOrder(
		context.Background(),
		Order{Symbol: "NVDA", Side: Buy, Qty: 1, Type: Market},
	)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	if want := 224.72 * 0.86; res.FilledPx != want {
		t.Errorf("FilledPx = %v, want %v (converted from the native fill price)", res.FilledPx, want)
	}
}

func TestT212ConfirmFill404IsUnconfirmed(t *testing.T) {
	// No order-status handler and no history handler: both the poll and the
	// history fallback 404. Neither can distinguish filled, cancelled, or a
	// purged rejection, so the order must be reported as unconfirmed rather
	// than guessed at.
	c := fillTestServer(t, nil)
	res, err := c.SubmitOrder(
		context.Background(),
		Order{Symbol: "AAPL", Side: Sell, Qty: 3, Type: Market},
	)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.Terminal {
		t.Errorf("Terminal = true, want false (a 404 is not a confirmed outcome)")
	}
	if res.Status != "unknown" {
		t.Errorf("Status = %q, want %q", res.Status, "unknown")
	}
}

// t212HistoryTestServer serves order placement (always answering NEW/0, then
// 404ing the working-order status endpoint — the fast-fill case reported in
// the wild: a liquid market order is often gone from the working set before
// the very first status poll runs) plus a GET /equity/history/orders handler
// under the caller's control.
func t212HistoryTestServer(t *testing.T, history http.HandlerFunc) *Trading212 {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/equity/metadata/instruments", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"ticker": "AAPL_US_EQ", "shortName": "AAPL", "isin": "US0378331005", "type": "STOCK"},
		})
	})
	mux.HandleFunc("POST /equity/orders/market", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 9, "ticker": body["ticker"], "quantity": body["quantity"],
			"filledQuantity": 0, "status": "NEW",
		})
	})
	// No handler registered for GET /equity/orders/{id}: every status poll
	// 404s, exactly like an order that left the working set before the first
	// poll ran.
	mux.HandleFunc("/equity/history/orders", history)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := NewTrading212(srv.URL, "k", "s", nil, "EUR")
	c.limiters = map[string]*t212RateLimiter{}

	return c
}

// TestT212ConfirmFillResolvesA404FromHistory reproduces the reported bug:
// Trading 212 market orders on a liquid instrument routinely fill and leave
// the working set before confirmFill's very first status poll, so the poll
// 404s on every single order — and before this fix, a 404 was reported
// straight to the caller as unconfirmed with no further check, so every
// buy/sell logged "broker did not confirm this order's outcome" even though
// the order plainly went through. The fix reads the order's real outcome
// from the account's order history before giving up.
func TestT212ConfirmFillResolvesA404FromHistory(t *testing.T) {
	var gotTicker string

	c := t212HistoryTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotTicker = r.URL.Query().Get("ticker")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{"order": map[string]any{
					"id": 9, "ticker": "AAPL_US_EQ", "quantity": 2.0,
					"filledQuantity": 2.0, "filledValue": 391.0, "status": "FILLED",
				}},
			},
		})
	})

	res, err := c.SubmitOrder(
		context.Background(),
		Order{Symbol: "AAPL", Side: Buy, Qty: 2, Type: Market},
	)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !res.Terminal {
		t.Error("Terminal = false, want true — history has the order's real, final outcome")
	}
	if res.Status != "filled" {
		t.Errorf("Status = %q, want %q", res.Status, "filled")
	}
	if res.FilledQty != 2 {
		t.Errorf("FilledQty = %v, want 2", res.FilledQty)
	}
	if res.FilledPx != 195.5 {
		t.Errorf("FilledPx = %v, want 195.5 (filledValue/filledQuantity)", res.FilledPx)
	}
	if gotTicker != "AAPL_US_EQ" {
		t.Errorf("history lookup ticker = %q, want AAPL_US_EQ", gotTicker)
	}
}

// TestT212ConfirmFillResolvesARejectionFromHistory checks the other terminal
// outcome history can report: a rejected/cancelled order must still come
// back with an honest zero fill, not be reported as unconfirmed just because
// it wasn't a fill.
func TestT212ConfirmFillResolvesARejectionFromHistory(t *testing.T) {
	c := t212HistoryTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{"order": map[string]any{
					"id": 9, "ticker": "AAPL_US_EQ", "quantity": 2.0,
					"filledQuantity": 0, "status": "REJECTED",
				}},
			},
		})
	})

	res, err := c.SubmitOrder(
		context.Background(),
		Order{Symbol: "AAPL", Side: Buy, Qty: 2, Type: Market},
	)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if !res.Terminal {
		t.Error("Terminal = false, want true — a rejection is a confirmed, final outcome")
	}
	if res.FilledQty != 0 {
		t.Errorf("FilledQty = %v, want 0 for a rejected order", res.FilledQty)
	}
	if res.Status == "filled" {
		t.Errorf("Status = %q — a rejection must not be reported as filled", res.Status)
	}
}

// TestT212ConfirmFillHistoryMissFallsBackFast checks that a history lookup
// that can't find the order (empty page — not yet propagated, or a purged
// id) falls back to the pre-existing unconfirmed result promptly, rather
// than burning the whole 25s fill-wait deadline on a lookup that already
// came back empty once. This is what keeps a genuine failure (or a miss)
// from making every affected order block for the full timeout.
func TestT212ConfirmFillHistoryMissFallsBackFast(t *testing.T) {
	c := t212HistoryTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}})
	})

	start := time.Now()

	res, err := c.SubmitOrder(
		context.Background(),
		Order{Symbol: "AAPL", Side: Buy, Qty: 2, Type: Market},
	)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %s — a history miss should fall back immediately, "+
			"not poll out the fill-wait deadline", elapsed)
	}

	if res.Terminal {
		t.Error("Terminal = true, want false — history had no record of this order")
	}
	if res.Status != "unknown" {
		t.Errorf("Status = %q, want %q", res.Status, "unknown")
	}
}

// TestT212GetPositionsFXFailureDoesNotClaimFXRate reproduces the FX-fallback
// finding: a failed rate fetch must not report FXRate as if it were a real
// value. The engine treats any nonzero FXRate as authoritative and caches it
// (see Engine.syncPortfolio), so a fallback "1" there would silently
// overwrite a previously-correct cached rate with a wrong one.
func TestT212GetPositionsFXFailureDoesNotClaimFXRate(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/equity/portfolio", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"ticker": "AAPL_US_EQ", "quantity": 2.0, "averagePrice": 150.0, "currentPrice": 160.0, "ppl": 20.0},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewTrading212(srv.URL, "k", "s", nil, "EUR")
	c.tickerCcy = map[string]string{"AAPL_US_EQ": "USD"}
	// Nothing listens here, so the rate fetch fails deterministically without
	// depending on network access to the real FX API.
	c.fx.baseURL = "http://127.0.0.1:1"

	positions, err := c.GetPositions(context.Background())
	if err != nil {
		t.Fatalf("GetPositions: %v", err)
	}

	if len(positions) != 1 {
		t.Fatalf("got %d positions, want 1", len(positions))
	}

	if got := positions[0].FXRate; got != 0 {
		t.Errorf("FXRate = %v, want 0 (unresolved) after a failed FX fetch — "+
			"a fallback 1 would be silently cached by the engine as if it were a real rate", got)
	}
}

// TestT212InstrumentCurrencyPropagatesFXFailure checks the CurrencyResolver
// contract the engine's FX rate cache depends on: a failed rate fetch must
// come back as a real error, not a silent 1:1, so the engine skips caching
// it and keeps whatever rate it last resolved successfully.
func TestT212InstrumentCurrencyPropagatesFXFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/equity/metadata/instruments", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"ticker": "AAPL_US_EQ", "shortName": "AAPL", "isin": "US0378331005", "type": "STOCK", "currencyCode": "USD"},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewTrading212(srv.URL, "k", "s", nil, "EUR")
	c.fx.baseURL = "http://127.0.0.1:1"

	_, _, err := c.InstrumentCurrency(context.Background(), "AAPL")
	if err == nil {
		t.Fatal("want a non-nil error when the FX rate fetch fails")
	}
}

// TestT212RateLimitGroupsPaceIndependently reproduces the pace() finding: a
// single mutex held across the pacing sleep serialized every endpoint group
// behind whichever one happened to be waiting — including order submission
// behind a slow "instruments" refresh. Groups must pace independently.
func TestT212RateLimitGroupsPaceIndependently(t *testing.T) {
	c := NewTrading212("http://unused.invalid", "k", "s", nil, "EUR")
	c.limiters = map[string]*t212RateLimiter{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // release the still-sleeping goroutine below promptly

	// "portfolio" (5s) is one of the wider short-lived groups. Consume its
	// budget so the NEXT call for that group has to sleep a while.
	if err := c.pace(ctx, "portfolio"); err != nil {
		t.Fatalf("pace(portfolio) #1: %v", err)
	}

	done := make(chan error, 1)

	go func() { done <- c.pace(ctx, "portfolio") }() // this one sleeps ~5s, or until cancel

	// Give the goroutine a moment to actually enter its sleep before racing
	// a different group against it.
	time.Sleep(20 * time.Millisecond)

	start := time.Now()
	if err := c.pace(ctx, "orders"); err != nil {
		t.Fatalf("pace(orders): %v", err)
	}

	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("pace(orders) took %s while \"portfolio\" was mid-sleep — "+
			"groups are still serialized behind one shared lock", elapsed)
	}
}

func TestT212ConfirmFillRejectedStaysUnfilled(t *testing.T) {
	// A rejected order must keep its honest zero fill — no realized P&L.
	c := fillTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 9, "ticker": "AAPL_US_EQ", "quantity": 2.0,
			"filledQuantity": 0, "status": "REJECTED",
		})
	})
	res, err := c.SubmitOrder(
		context.Background(),
		Order{Symbol: "AAPL", Side: Buy, Qty: 2, Type: Market},
	)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.FilledQty != 0 {
		t.Errorf("FilledQty = %v, want 0 for a rejected order", res.FilledQty)
	}
	if res.Status == "filled" {
		t.Errorf("Status = %q — a rejection must not be reported as filled", res.Status)
	}
	if !res.Terminal {
		t.Errorf("Terminal = false, want true — a rejection is a confirmed, final outcome")
	}
}

func TestT212WholeShareInstrumentFailsFast(t *testing.T) {
	// An instrument that accepts only whole shares (rejects any fractional
	// quantity with a precision error). Verifies the probe converges to whole
	// shares within the attempt budget, caches it, and refuses a sub-1-share
	// order without POSTing a zero-share order or flooding attempts.
	var orderPosts int

	var lastQty float64

	mux := http.NewServeMux()
	mux.HandleFunc("/equity/metadata/instruments", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"ticker": "AAPL_US_EQ", "shortName": "AAPL", "isin": "US0378331005", "type": "STOCK"},
		})
	})
	mux.HandleFunc("POST /equity/orders/market", func(w http.ResponseWriter, r *http.Request) {
		orderPosts++

		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		q, _ := body["quantity"].(float64)

		if q != float64(int64(q)) { // whole shares only
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type": "/api-errors/quantity-precision-mismatch", "status": 400,
				"detail": "invalid quantity precision 0",
			})

			return
		}

		lastQty = q
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":             1,
			"ticker":         body["ticker"],
			"quantity":       q,
			"filledQuantity": 0,
			"status":         "SUBMITTED",
		})
	})
	mux.HandleFunc("GET /equity/orders/{id}", func(w http.ResponseWriter, _ *http.Request) {
		abs := lastQty
		if abs < 0 {
			abs = -abs
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 1, "ticker": "AAPL_US_EQ", "quantity": lastQty,
			"filledQuantity": lastQty, "filledValue": abs * 100.0, "status": "FILLED",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewTrading212(srv.URL, "k", "s", nil, "EUR")
	c.limiters = map[string]*t212RateLimiter{}
	ctx := context.Background()

	// A >=1 share buy converges to whole shares within the attempt budget.
	res, err := c.SubmitOrder(ctx, Order{Symbol: "AAPL", Side: Buy, Qty: 3.5, Type: Market})
	if err != nil {
		t.Fatalf("whole-share buy should succeed: %v", err)
	}

	if res.Qty != 3 {
		t.Errorf("filled qty = %v, want 3 whole shares", res.Qty)
	}

	if orderPosts > maxT212QtyAttempts {
		t.Errorf("made %d order POSTs, want <= %d (no barrage)", orderPosts, maxT212QtyAttempts)
	}

	// The accepted precision (0) is cached: the next order posts exactly once.
	orderPosts = 0
	if _, err := c.SubmitOrder(
		ctx,
		Order{Symbol: "AAPL", Side: Buy, Qty: 5.9, Type: Market},
	); err != nil {
		t.Fatalf("second whole-share buy: %v", err)
	}

	if orderPosts != 1 {
		t.Errorf("cached whole-share precision should need 1 POST, got %d", orderPosts)
	}

	// A sub-1-share order can't exist on a whole-share instrument: fail fast
	// with a clear error and never POST a zero-share order.
	orderPosts = 0

	_, err = c.SubmitOrder(ctx, Order{Symbol: "AAPL", Side: Buy, Qty: 0.4, Type: Market})
	if err == nil {
		t.Error("expected a clear failure for a sub-1-share whole-share order")
	}

	if orderPosts != 0 {
		t.Errorf("a zero-share order must not be POSTed, got %d POSTs", orderPosts)
	}
}

func TestSessionOpenAt(t *testing.T) {
	base := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC) // reference day
	at := func(h, m int) time.Time {
		return base.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
	}
	ev := func(h, m int, typ string) t212TimeEvent { return t212TimeEvent{At: at(h, m), Type: typ} }

	// Regular session 13:30–20:00 UTC with a break 16:00–16:30, plus the next
	// day's open so "now" stays inside the covered window.
	events := []t212TimeEvent{
		ev(13, 30, "OPEN"),
		ev(16, 0, "BREAK_START"),
		ev(16, 30, "BREAK_END"),
		ev(20, 0, "CLOSE"),
		ev(37, 30, "OPEN"), // next day 13:30
	}
	pre := 5 * time.Minute

	cases := []struct {
		name             string
		at               time.Time
		wantOpen, wantOK bool
	}{
		{"pre-open quiet hours", at(13, 0), false, true},
		{"within pre-open window", at(13, 26), true, true},
		{"mid session", at(14, 0), true, true},
		{"in break", at(16, 15), false, true},
		{"after break", at(16, 45), true, true},
		{"after close", at(20, 30), false, true},
		{"before covered window", base.Add(-48 * time.Hour), false, false},
		{"after covered window", base.Add(72 * time.Hour), false, false},
	}
	for _, c := range cases {
		open, ok := sessionOpenAt(events, c.at, pre)
		if open != c.wantOpen || ok != c.wantOK {
			t.Errorf(
				"%s: open=%v known=%v, want open=%v known=%v",
				c.name,
				open,
				ok,
				c.wantOpen,
				c.wantOK,
			)
		}
	}
}

func TestT212TradingOpenUsesSchedule(t *testing.T) {
	// Capability is advertised.
	var _ MarketScheduler = (*Trading212)(nil)

	openAt := time.Now().UTC().Add(-time.Hour) // session opened an hour ago
	closeAt := time.Now().UTC().Add(time.Hour) // and closes in an hour

	mux := http.NewServeMux()
	mux.HandleFunc("/equity/metadata/instruments", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"ticker":            "AAPL_US_EQ",
				"shortName":         "AAPL",
				"isin":              "US0378331005",
				"type":              "STOCK",
				"workingScheduleId": 7,
			},
		})
	})
	mux.HandleFunc("/equity/metadata/exchanges", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": 1, "name": "NASDAQ", "workingSchedules": []map[string]any{
				{"id": 7, "timeEvents": []map[string]any{
					{"date": openAt.Format(time.RFC3339), "type": "OPEN"},
					{"date": closeAt.Format(time.RFC3339), "type": "CLOSE"},
				}},
			}},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewTrading212(srv.URL, "k", "s", nil, "EUR")
	c.limiters = map[string]*t212RateLimiter{}

	// Resolving loads instruments + exchanges into the cache.
	if _, err := c.ResolveSymbol(context.Background(), "AAPL"); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if open, known := c.TradingOpen("AAPL", time.Now(), time.Minute); !known || !open {
		t.Errorf("during session: open=%v known=%v, want open=true known=true", open, known)
	}

	// An unknown symbol (never resolved, no schedule) defers to the caller.
	if _, known := c.TradingOpen("NOPE", time.Now(), time.Minute); known {
		t.Error("unmapped symbol should report known=false so the caller falls back")
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
	if len(pos) != 1 || pos[0].Symbol != "AAPL" || pos[0].MarketValue != 320 ||
		pos[0].UnrealizedPL != 20 {
		t.Errorf("position mapping wrong: %+v", pos)
	}
}

// TestT212GetAccountConvertsToPreferCcy reproduces the cash/position
// currency mismatch finding: GetAccount used to return cash.Free/cash.Total
// verbatim in the account's own currency while GetPositions already
// converted position values to preferCcy — Engine.syncPortfolio sums the
// two into one equity figure with no idea they were in different
// currencies. preferCcy defaults to "EUR", so a non-EUR account (e.g. a GBP
// Trading 212 account) hit this on ordinary defaults.
func TestT212GetAccountConvertsToPreferCcy(t *testing.T) {
	fx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("base"); got != "GBP" {
			t.Errorf("fx request base = %q, want GBP", got)
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"base": "GBP", "rates": map[string]float64{"EUR": 1.17},
		})
	}))
	t.Cleanup(fx.Close)

	mux := http.NewServeMux()
	mux.HandleFunc("/equity/account/info", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"currencyCode": "GBP"})
	})
	mux.HandleFunc("/equity/account/cash", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"free": 900.0, "total": 1000.0})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewTrading212(srv.URL, "key", "secret", nil, "EUR")
	c.fx.baseURL = fx.URL
	c.limiters = map[string]*t212RateLimiter{}

	acct, err := c.GetAccount(context.Background())
	if err != nil {
		t.Fatalf("account: %v", err)
	}

	if acct.Currency != "EUR" {
		t.Errorf("Currency = %q, want EUR (converted to preferCcy)", acct.Currency)
	}

	wantCash := 900.0 * 1.17
	if math.Abs(acct.Cash-wantCash) > 1e-9 {
		t.Errorf("Cash = %v, want %v — unconverted GBP cash summed against EUR position "+
			"values elsewhere produces a nonsense equity figure", acct.Cash, wantCash)
	}

	wantEquity := 1000.0 * 1.17
	if math.Abs(acct.Equity-wantEquity) > 1e-9 {
		t.Errorf("Equity = %v, want %v", acct.Equity, wantEquity)
	}
}

// TestT212PositionsConvertForeignCurrency verifies that a position in an
// instrument listed in a currency other than preferCcy gets its price/value
// converted using the FX rate API, while ppl (already broker-reported in
// account currency) passes through unchanged.
func TestT212PositionsConvertForeignCurrency(t *testing.T) {
	fx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("base"); got != "USD" {
			t.Errorf("fx request base = %q, want USD", got)
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"base": "USD", "rates": map[string]float64{"EUR": 0.86},
		})
	}))
	t.Cleanup(fx.Close)

	mux := http.NewServeMux()
	mux.HandleFunc("/equity/metadata/instruments", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"ticker": "NVDA_US_EQ", "shortName": "NVDA", "isin": "US67066G1040",
				"currencyCode": "USD", "type": "STOCK",
			},
		})
	})
	mux.HandleFunc("/equity/portfolio", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"ticker": "NVDA_US_EQ", "quantity": 0.4513,
				"averagePrice": 221.49, "currentPrice": 224.72, "ppl": 1.26,
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewTrading212(srv.URL, "key", "secret", nil, "EUR")
	c.fx.baseURL = fx.URL
	c.limiters = map[string]*t212RateLimiter{}

	pos, err := c.GetPositions(context.Background())
	if err != nil {
		t.Fatalf("positions: %v", err)
	}

	if len(pos) != 1 {
		t.Fatalf("got %d positions, want 1", len(pos))
	}

	p := pos[0]
	if p.Currency != "USD" || p.FXRate != 0.86 {
		t.Errorf("currency/rate = %s/%v, want USD/0.86", p.Currency, p.FXRate)
	}

	wantCurrent := 224.72 * 0.86
	wantAvg := 221.49 * 0.86
	wantMV := 224.72 * 0.4513 * 0.86

	if math.Abs(p.Current-wantCurrent) > 1e-9 {
		t.Errorf("Current = %v, want %v (unconverted USD price leaking through)", p.Current, wantCurrent)
	}

	if math.Abs(p.AvgPrice-wantAvg) > 1e-9 {
		t.Errorf("AvgPrice = %v, want %v", p.AvgPrice, wantAvg)
	}

	if math.Abs(p.MarketValue-wantMV) > 1e-9 {
		t.Errorf("MarketValue = %v, want %v", p.MarketValue, wantMV)
	}

	// ppl is already account currency per Trading 212's own docs — must not
	// be converted a second time.
	if p.UnrealizedPL != 1.26 {
		t.Errorf("UnrealizedPL = %v, want 1.26 (unconverted)", p.UnrealizedPL)
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
	if o.OrderID != "777" || o.Symbol != "AAPL" || o.Side != Sell || o.Qty != 5 || o.Price != 200 ||
		o.Type != "LIMIT" {
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
		"LXS.DE":          "LXSd_EQ",    // German listing via .DE suffix
		"SAP.DE":          "SAPd_EQ",    // disambiguate German SAP, not Saputo (CA)
		"SAP":             "SAP_US_EQ",  // no suffix => prefer US listing
		"AAPL":            "AAPL_US_EQ", // US preferred
		"DE000SLA7M77.SG": "SLABOND_EQ", // ISIN match
		"LXSd_EQ":         "LXSd_EQ",    // already a ticker, passes through
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

// TestT212LimitOrderDowngradeIsSurfaced pins a silent no-op: this adapter has
// only /equity/orders/market, so execution.type: "limit" — which makes the
// engine type every non-urgent clip broker.Limit and price it inside the
// touch — produced a plain market order that crossed the spread, with nothing
// anywhere saying so. The operator saw a "limit 99.9500" note on the trade
// record and a limit_offset_pct in their config, and got market fills.
//
// It cannot be rejected at config load (execution.type is broker-agnostic, and
// the same file is valid against Saxo, which does support limit orders), so
// the adapter has to surface it. Once per process: it is a fact about the
// configuration, not about any one order.
//
// Not parallel: errlog is process-wide state.
func TestT212LimitOrderDowngradeIsSurfaced(t *testing.T) {
	m := newT212Mock(t)
	ctx := context.Background()
	c := m.fresh()

	errlog.Clear()

	if _, err := c.SubmitOrder(ctx, Order{
		Symbol: "AAPL", Side: Buy, Qty: 3, Type: Limit, LimitPx: 99.95,
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// The downgrade itself: the limit price never reaches the wire.
	if _, ok := m.lastBody["limitPrice"]; ok {
		t.Errorf("body carries a limit price: %v — the adapter grew limit support "+
			"and this test is now pinning the wrong thing", m.lastBody)
	}

	var got []string

	for _, e := range errlog.Recent() {
		if e.Source == "trading212" && strings.Contains(e.Message, "limit") {
			got = append(got, e.Message)
		}
	}

	if len(got) != 1 {
		t.Fatalf("errlog has %d limit-downgrade notice(s), want exactly 1: %v", len(got), got)
	}

	if !strings.Contains(got[0], "execution.type") {
		t.Errorf("notice does not name the setting responsible: %q", got[0])
	}

	// A second order on the same client must not repeat it — an order-path
	// message that fires per submission is noise, not a signal.
	if _, err := c.SubmitOrder(ctx, Order{
		Symbol: "AAPL", Side: Buy, Qty: 2, Type: Limit, LimitPx: 99.95,
	}); err != nil {
		t.Fatalf("second submit: %v", err)
	}

	n := 0

	for _, e := range errlog.Recent() {
		if e.Source == "trading212" && strings.Contains(e.Message, "execution.type") {
			n++
		}
	}

	if n != 1 {
		t.Errorf("downgrade notice recorded %d times across two orders, want once per process", n)
	}

	// A market order must not trip it at all.
	errlog.Clear()

	if _, err := m.fresh().SubmitOrder(ctx, Order{
		Symbol: "AAPL", Side: Buy, Qty: 1, Type: Market,
	}); err != nil {
		t.Fatalf("market submit: %v", err)
	}

	for _, e := range errlog.Recent() {
		if strings.Contains(e.Message, "execution.type") {
			t.Errorf("a market order produced the limit-downgrade notice: %q", e.Message)
		}
	}
}
