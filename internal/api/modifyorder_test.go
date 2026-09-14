package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestModifyOrderRejectsAnUnvalidatedSide reproduces the "modifyOrder
// accepts an unvalidated side string" finding: PATCH /api/orders/:id is
// the order path that changes a working order's side, quantity and price
// directly from a raw JSON field. Every other order-placement path
// validates side against the buy/sell enum; this one must too, rather than
// passing an arbitrary string through to the broker adapter.
func TestModifyOrderRejectsAnUnvalidatedSide(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPatch, "/api/orders/ord-1",
		`{"symbol":"AAPL","side":"sideways","qty":1,"price":100,"type":"limit"}`)

	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", code, http.StatusBadRequest, body)
	}

	if !strings.Contains(body, "side") {
		t.Errorf("error body = %q, want it to mention the invalid side", body)
	}
}

// TestModifyOrderAcceptsAValidSide confirms the fix doesn't reject
// legitimate requests — the validation must accept buy/sell (case-
// insensitively), not merely reject everything.
func TestModifyOrderAcceptsAValidSide(t *testing.T) {
	s := newTestServer(t)

	// The broker adapter itself will likely fail this modification (no such
	// working order in the test harness) — the point here is only that
	// "side" passes the request-level validation and the request reaches
	// the engine, i.e. the response is NOT the "require symbol, side
	// (buy|sell), qty>0" validation error.
	code, body := do(t, s, http.MethodPatch, "/api/orders/ord-1",
		`{"symbol":"AAPL","side":"BUY","qty":1,"price":100,"type":"limit"}`)

	if strings.Contains(body, "require symbol, side") {
		t.Errorf("status=%d body=%q: a valid side was rejected by the request-level validation", code, body)
	}
}

// TestModifyOrderRejectsAnEmptySymbolOrNonPositiveQty covers the other two
// guards in the same validation check, so the whole condition is exercised
// rather than just the side clause.
func TestModifyOrderRejectsAnEmptySymbolOrNonPositiveQty(t *testing.T) {
	s := newTestServer(t)

	cases := []string{
		`{"symbol":"","side":"buy","qty":1,"price":100,"type":"limit"}`,
		`{"symbol":"AAPL","side":"buy","qty":0,"price":100,"type":"limit"}`,
	}

	for _, body := range cases {
		code, resp := do(t, s, http.MethodPatch, "/api/orders/ord-1", body)
		if code != http.StatusBadRequest {
			t.Errorf("body=%s: status = %d, want %d: %s", body, code, http.StatusBadRequest, resp)
		}
	}
}
