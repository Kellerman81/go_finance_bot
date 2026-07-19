package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	return NewAlpaca(srv.URL, "k", "s"), &posts
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
