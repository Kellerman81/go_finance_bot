package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// alpacaTestServer serves order submission plus a custom order-status handler
// for fill polling.
func alpacaTestServer(t *testing.T, orderStatus http.HandlerFunc) (*Alpaca, *int) {
	var posts int
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v2/orders", func(w http.ResponseWriter, r *http.Request) {
		posts++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "ord-1", "symbol": body["symbol"], "side": body["side"],
			"qty": body["qty"], "filled_qty": "0", "filled_avg_price": "",
			"status": "accepted", "submitted_at": "2026-01-01T00:00:00Z",
		})
	})
	if orderStatus != nil {
		mux.HandleFunc("GET /v2/orders/{id}", orderStatus)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewAlpaca(srv.URL, "k", "s", ""), &posts
}

func TestAlpacaQtyPrecisionRoundedDown(t *testing.T) {
	// A computed fractional quantity (order_size/price) commonly has 15+ decimal
	// places; Alpaca accepts at most 9. It must be sent rounded DOWN to <= 9 dp.
	var sentQty string

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v2/orders", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		sentQty, _ = body["qty"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "ord-1", "symbol": body["symbol"], "side": body["side"],
			"qty": sentQty, "filled_qty": sentQty, "filled_avg_price": "100",
			"status": "filled", "submitted_at": "2026-01-01T00:00:00Z",
		})
	})
	mux.HandleFunc("GET /v2/orders/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "ord-1", "symbol": "AAPL", "side": "buy", "qty": sentQty,
			"filled_qty": sentQty, "filled_avg_price": "100", "status": "filled",
			"submitted_at": "2026-01-01T00:00:00Z",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := NewAlpaca(srv.URL, "k", "s", "")

	raw := 100.0 / 393.0 // 0.2544529262086514… (16 dp)
	if _, err := a.SubmitOrder(
		context.Background(),
		Order{Symbol: "AAPL", Side: Buy, Qty: raw, Type: Market},
	); err != nil {
		t.Fatalf("submit: %v", err)
	}

	f, err := strconv.ParseFloat(sentQty, 64)
	if err != nil {
		t.Fatalf("sent qty %q not numeric: %v", sentQty, err)
	}

	if decimals(f) > alpacaMaxQtyDecimals {
		t.Errorf(
			"sent qty %q has %d decimals, want <= %d",
			sentQty,
			decimals(f),
			alpacaMaxQtyDecimals,
		)
	}

	if f > raw {
		t.Errorf("sent qty %v exceeds requested %v (must round down)", f, raw)
	}

	if want := 0.254452926; f != want {
		t.Errorf("sent qty %v, want %v (floored to 9 dp)", f, want)
	}
}

// TestAlpacaNotionalPrecisionKeepsFourDecimals reproduces the "2-decimal
// hardcode where the venue allows 4 for sub-$1 instruments" finding: a
// notional order for a sub-$1 instrument must not be truncated to 2
// decimals — 0.1234 was silently becoming 0.12 (a ~3% error) before this
// fix.
func TestAlpacaNotionalPrecisionKeepsFourDecimals(t *testing.T) {
	var sentNotional string

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v2/orders", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		sentNotional, _ = body["notional"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "ord-1", "symbol": body["symbol"], "side": body["side"],
			"notional": sentNotional, "filled_qty": "0", "filled_avg_price": "",
			"status": "accepted", "submitted_at": "2026-01-01T00:00:00Z",
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := NewAlpaca(srv.URL, "k", "s", "")

	if _, err := a.SubmitOrder(
		context.Background(),
		Order{Symbol: "PENNY", Side: Buy, Type: Market, Notional: 0.1234},
	); err != nil {
		t.Fatalf("submit: %v", err)
	}

	if sentNotional != "0.1234" {
		t.Errorf("sent notional = %q, want %q (4-decimal precision, not the old "+
			"flat 2dp truncation to 0.12)", sentNotional, "0.1234")
	}
}

// TestAlpacaNotionalPrecisionTrimsTrailingZeros confirms the fix doesn't
// overcorrect: a round dollar amount must still be sent as a plain number,
// not padded out to 4 decimal places.
func TestAlpacaNotionalPrecisionTrimsTrailingZeros(t *testing.T) {
	var sentNotional string

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v2/orders", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		sentNotional, _ = body["notional"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "ord-1", "symbol": body["symbol"], "side": body["side"],
			"notional": sentNotional, "filled_qty": "0", "filled_avg_price": "",
			"status": "accepted", "submitted_at": "2026-01-01T00:00:00Z",
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := NewAlpaca(srv.URL, "k", "s", "")

	if _, err := a.SubmitOrder(
		context.Background(),
		Order{Symbol: "AAPL", Side: Buy, Type: Market, Notional: 100},
	); err != nil {
		t.Fatalf("submit: %v", err)
	}

	if sentNotional != "100" {
		t.Errorf("sent notional = %q, want %q (no padded trailing zeros)", sentNotional, "100")
	}
}

// TestAlpacaPreferredCurrencyConversion verifies that GetAccount/GetPositions
// convert the account's native (USD) balances/positions into preferCcy when
// it's set to something else, and leave everything alone when it's empty
// (matching the account's own currency, the default before preferred_currency
// existed for Alpaca).
func TestAlpacaPreferredCurrencyConversion(t *testing.T) {
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
	mux.HandleFunc("/v2/account", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cash": "900", "equity": "1000", "buying_power": "900", "currency": "USD",
		})
	})
	mux.HandleFunc("/v2/positions", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"symbol": "AAPL", "qty": "2", "avg_entry_price": "150",
				"current_price": "160", "market_value": "320", "unrealized_pl": "20",
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := NewAlpaca(srv.URL, "k", "s", "EUR")
	a.fx.baseURL = fx.URL

	ctx := context.Background()

	acct, err := a.GetAccount(ctx)
	if err != nil {
		t.Fatalf("account: %v", err)
	}

	if acct.Currency != "EUR" || acct.Cash != 900*0.86 || acct.Equity != 1000*0.86 {
		t.Errorf("account not converted: %+v", acct)
	}

	pos, err := a.GetPositions(ctx)
	if err != nil {
		t.Fatalf("positions: %v", err)
	}

	if len(pos) != 1 {
		t.Fatalf("got %d positions, want 1", len(pos))
	}

	p := pos[0]
	if p.Currency != "USD" || p.FXRate != 0.86 || p.MarketValue != 320*0.86 || p.UnrealizedPL != 20*0.86 {
		t.Errorf("position not converted: %+v", p)
	}
}

// TestAlpacaGetPositionsFXFailureDoesNotClaimFXRate reproduces the
// FX-fallback finding: a failed rate fetch must not report FXRate as if it
// were a real value. The engine treats any nonzero FXRate as authoritative
// and caches it (see Engine.syncPortfolio), so a fallback "1" there would
// silently overwrite a previously-correct cached rate with a wrong one.
func TestAlpacaGetPositionsFXFailureDoesNotClaimFXRate(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/account", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cash": "900", "equity": "1000", "buying_power": "900", "currency": "USD",
		})
	})
	mux.HandleFunc("/v2/positions", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"symbol": "AAPL", "qty": "2", "avg_entry_price": "150",
				"current_price": "160", "market_value": "320", "unrealized_pl": "20",
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := NewAlpaca(srv.URL, "k", "s", "EUR")
	// Nothing listens here, so the rate fetch fails deterministically without
	// depending on network access to the real FX API.
	a.fx.baseURL = "http://127.0.0.1:1"

	pos, err := a.GetPositions(context.Background())
	if err != nil {
		t.Fatalf("positions: %v", err)
	}

	if len(pos) != 1 {
		t.Fatalf("got %d positions, want 1", len(pos))
	}

	if got := pos[0].FXRate; got != 0 {
		t.Errorf("FXRate = %v, want 0 (unresolved) after a failed FX fetch — "+
			"a fallback 1 would be silently cached by the engine as if it were a real rate", got)
	}
}

// TestAlpacaSubmitOrderConvertsLimitAndFillPrices verifies a limit order's
// LimitPx (arriving in preferCcy, like everything else this adapter reports)
// is converted to the account's own currency before being sent to Alpaca,
// and the returned FilledPx is converted back — otherwise the caller would
// submit at the wrong price entirely, off by roughly the FX rate.
func TestAlpacaSubmitOrderConvertsLimitAndFillPrices(t *testing.T) {
	fx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"base": "EUR", "rates": map[string]float64{"USD": 1.16},
		})
	}))
	t.Cleanup(fx.Close)

	var sentLimitPx string

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/account", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cash": "900", "equity": "1000", "buying_power": "900", "currency": "USD",
		})
	})
	mux.HandleFunc("POST /v2/orders", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		sentLimitPx, _ = body["limit_price"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "ord-1", "symbol": "NVDA", "side": "buy", "qty": "1",
			"filled_qty": "1", "filled_avg_price": "195.5",
			"status": "filled", "submitted_at": "2026-01-01T00:00:00Z",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := NewAlpaca(srv.URL, "k", "s", "EUR")
	a.fx.baseURL = fx.URL

	res, err := a.SubmitOrder(context.Background(), Order{
		Symbol: "NVDA", Side: Buy, Qty: 1, Type: Limit, LimitPx: 200, // EUR
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	if want := "232.00"; sentLimitPx != want { // 200 EUR * 1.16 USD/EUR
		t.Errorf("sent limit_price = %q, want %q (EUR converted to native USD)", sentLimitPx, want)
	}

	if want := 195.5 / 1.16; res.FilledPx != want { // native USD back to EUR
		t.Errorf("FilledPx = %v, want %v (converted back to preferCcy)", res.FilledPx, want)
	}
}

// TestAlpacaSubmitOrderRefusesALimitWhenFXFails reproduces the
// order-pricing FX finding: a failed rate fetch used to leave LimitPx
// unconverted and submit it as-is (silently ~9-16% off, not merely
// mislabeled). It must now refuse the order instead of sending a mispriced
// limit/stop.
func TestAlpacaSubmitOrderRefusesALimitWhenFXFails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/account", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cash": "900", "equity": "1000", "buying_power": "900", "currency": "USD",
		})
	})

	orderPosted := false

	mux.HandleFunc("POST /v2/orders", func(w http.ResponseWriter, r *http.Request) {
		orderPosted = true
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ord-1"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := NewAlpaca(srv.URL, "k", "s", "EUR")
	// Nothing listens here, so the rate fetch fails deterministically.
	a.fx.baseURL = "http://127.0.0.1:1"

	_, err := a.SubmitOrder(context.Background(), Order{
		Symbol: "NVDA", Side: Buy, Qty: 1, Type: Limit, LimitPx: 200,
	})
	if err == nil {
		t.Fatal("want an error: the fx rate needed to price this limit order is unavailable")
	}

	if orderPosted {
		t.Error("the order was posted to the broker despite the unpriced limit — " +
			"it must be refused before submission, not sent unconverted")
	}
}

func TestAlpacaConfirmFill(t *testing.T) {
	// The POST answers accepted/0 (async fill); the order endpoint reports the
	// real fill — the result must carry it, not the submission snapshot.
	var polls int
	a, _ := alpacaTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		polls++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": r.PathValue("id"), "symbol": "AAPL", "side": "buy", "qty": "2",
			"filled_qty": "2", "filled_avg_price": "195.5",
			"status": "filled", "submitted_at": "2026-01-01T00:00:00Z",
		})
	})

	res, err := a.SubmitOrder(
		context.Background(),
		Order{Symbol: "AAPL", Side: Buy, Qty: 2, Type: Market},
	)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if polls == 0 {
		t.Error("expected the fill to be polled")
	}
	if res.FilledQty != 2 {
		t.Errorf("FilledQty = %v, want 2 (the confirmed fill)", res.FilledQty)
	}
	if res.FilledPx != 195.5 {
		t.Errorf("FilledPx = %v, want 195.5", res.FilledPx)
	}
	if res.Status != "filled" {
		t.Errorf("Status = %q, want filled", res.Status)
	}
}

func TestAlpacaConfirmFillRejectedStaysUnfilled(t *testing.T) {
	// A rejected order must keep its honest zero fill — no realized P&L.
	a, _ := alpacaTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": r.PathValue("id"), "symbol": "AAPL", "side": "buy", "qty": "2",
			"filled_qty": "0", "status": "rejected", "submitted_at": "2026-01-01T00:00:00Z",
		})
	})

	res, err := a.SubmitOrder(
		context.Background(),
		Order{Symbol: "AAPL", Side: Buy, Qty: 2, Type: Market},
	)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.FilledQty != 0 {
		t.Errorf("FilledQty = %v, want 0 for a rejected order", res.FilledQty)
	}
	if res.Status != "rejected" {
		t.Errorf("Status = %q, want rejected", res.Status)
	}
}

func TestAlpacaLimitOrderNotPolled(t *testing.T) {
	// Limit orders may legitimately stay working — SubmitOrder must not block
	// polling them.
	var polls int
	a, _ := alpacaTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		polls++
		w.WriteHeader(http.StatusOK)
	})

	res, err := a.SubmitOrder(
		context.Background(),
		Order{Symbol: "AAPL", Side: Buy, Qty: 2, Type: Limit, LimitPx: 100},
	)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if polls != 0 {
		t.Errorf("limit order was polled %d times, want 0", polls)
	}
	if res.Status != "accepted" {
		t.Errorf("Status = %q, want the submission status", res.Status)
	}
}

// TestAlpacaMarketFillWithoutAnFXRateReportsNoPrice is the Alpaca half of
// BR2-3. A market order is still sent when the rate fetch fails (rate stays
// 1), and its fill price came back in the account's USD labelled as the
// preferred EUR — a $163 fill booked as €163, skewing entry price, daily
// spend, realized P&L and the stop anchors by the exchange rate. With no rate
// the price must be reported as unknown; the engine falls back to its own
// converted reference.
func TestAlpacaMarketFillWithoutAnFXRateReportsNoPrice(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/account", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cash": "900", "equity": "1000", "buying_power": "900", "currency": "USD",
		})
	})
	mux.HandleFunc("POST /v2/orders", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ord-1", "status": "accepted", "filled_qty": "0"})
	})
	mux.HandleFunc("GET /v2/orders/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "ord-1", "symbol": "NVDA", "side": "buy", "qty": "1",
			"filled_qty": "1", "filled_avg_price": "163", "status": "filled",
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := NewAlpaca(srv.URL, "k", "s", "EUR")
	a.fx.baseURL = "http://127.0.0.1:1" // the rate fetch fails deterministically

	res, err := a.SubmitOrder(context.Background(), Order{Symbol: "NVDA", Side: Buy, Qty: 1, Type: Market})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	if res.FilledPx != 0 {
		t.Errorf("FilledPx = %v with no USD->EUR rate, want 0 (unknown) rather than a USD price labelled EUR", res.FilledPx)
	}
}

// BR2-7, end to end: the earlier decimal fix reached only the notional path,
// which nothing calls; the LIMIT path still sent a fixed 2 dp. This checks
// what actually goes over the wire for a sub-$1 buy limit.
func TestAlpacaSendsASubDollarLimitAtFourDecimals(t *testing.T) {
	var sentLimit string

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v2/orders", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		sentLimit, _ = body["limit_price"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "ord-1", "symbol": body["symbol"], "side": body["side"],
			"qty": "100", "filled_qty": "0", "filled_avg_price": "",
			"status": "new", "submitted_at": "2026-01-01T00:00:00Z",
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := NewAlpaca(srv.URL, "k", "s", "")

	if _, err := a.SubmitOrder(context.Background(),
		Order{Symbol: "PENNY", Side: Buy, Type: Limit, Qty: 100, LimitPx: 0.4567},
	); err != nil {
		t.Fatalf("submit: %v", err)
	}

	if sentLimit != "0.4567" {
		t.Errorf("sent limit_price = %q, want %q (not rounded UP to 0.46 on a buy)", sentLimit, "0.4567")
	}
}
