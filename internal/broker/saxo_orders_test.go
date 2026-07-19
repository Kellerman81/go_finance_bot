package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// saxoTestToken is a TokenSource stub for httptest-backed Saxo clients.
type saxoTestToken string

func (t saxoTestToken) Token(context.Context) (string, error) { return string(t), nil }

// saxoFillClient builds a Saxo client (bypassing account resolution) whose
// working-order list is driven by the given order-ID lists, returned in call
// order (the last list repeats).
func saxoFillClient(t *testing.T, workingSets ...[]string) (*Saxo, *int) {
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /port/v1/orders/me", func(w http.ResponseWriter, r *http.Request) {
		set := workingSets[min(calls, len(workingSets)-1)]
		calls++
		data := make([]map[string]any, 0, len(set))
		for _, id := range set {
			data = append(data, map[string]any{"OrderId": id})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Data": data})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Saxo{
		baseURL:    srv.URL,
		tokens:     saxoTestToken("tok"),
		http:       srv.Client(),
		clientKey:  "CK",
		accountKey: "ACC",
	}, &calls
}

func TestSaxoConfirmFillOrderGoneMeansFilled(t *testing.T) {
	// The order is in the working set on the first poll and gone on the second:
	// a day market order we did not cancel has filled — report the submitted
	// amount as filled (price stays 0; callers use their reference price).
	s, calls := saxoFillClient(t, []string{"42", "7"}, []string{"7"})
	res := s.confirmFill(
		context.Background(),
		OrderResult{ID: "42", Qty: 5, Status: "submitted", SubmittedAt: time.Now()},
	)
	if *calls < 2 {
		t.Errorf("expected at least 2 polls, got %d", *calls)
	}
	if res.FilledQty != 5 {
		t.Errorf("FilledQty = %v, want 5 (submitted amount assumed filled)", res.FilledQty)
	}
	if res.Status != "filled" {
		t.Errorf("Status = %q, want filled", res.Status)
	}
}

func TestSaxoConfirmFillImmediatelyGone(t *testing.T) {
	// Already absent on the first poll (filled before we could look).
	s, _ := saxoFillClient(t, []string{})
	res := s.confirmFill(context.Background(), OrderResult{ID: "42", Qty: 3, Status: "submitted"})
	if res.FilledQty != 3 || res.Status != "filled" {
		t.Errorf("want filled/3, got %q/%v", res.Status, res.FilledQty)
	}
}

func TestSaxoConfirmFillErrorKeepsSubmittedState(t *testing.T) {
	// A failing status endpoint must not fabricate a fill.
	mux := http.NewServeMux() // no handler → 404 on the list endpoint = error
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s := &Saxo{
		baseURL:    srv.URL,
		tokens:     saxoTestToken("tok"),
		http:       srv.Client(),
		clientKey:  "CK",
		accountKey: "ACC",
	}
	res := s.confirmFill(context.Background(), OrderResult{ID: "42", Qty: 3, Status: "submitted"})
	if res.FilledQty != 0 || res.Status != "submitted" {
		t.Errorf("want submitted/0 on error, got %q/%v", res.Status, res.FilledQty)
	}
}

func TestBuildBracketBody_LongThreeWay(t *testing.T) {
	inst := instrument{Uic: 211, AssetType: "Stock"}
	b := BracketOrder{
		Symbol: "AAPL", Side: Buy, Qty: 10,
		EntryType: Limit, EntryPrice: 150,
		StopLoss: 140, TakeProfit: 170,
	}
	body := buildBracketBody("ACC", inst, b, 10)

	if body["BuySell"] != "Buy" || body["OrderType"] != "Limit" || body["OrderPrice"] != 150.0 {
		t.Fatalf("entry leg wrong: %#v", body)
	}
	if body["Uic"].(int) != 211 || body["AssetType"] != "Stock" || body["Amount"].(float64) != 10 {
		t.Fatalf("entry instrument/amount wrong: %#v", body)
	}
	if dur := body["OrderDuration"].(map[string]any)["DurationType"]; dur != "GoodTillCancel" {
		t.Fatalf("entry duration = %v, want GoodTillCancel", dur)
	}

	legs, ok := body["Orders"].([]map[string]any)
	if !ok || len(legs) != 2 {
		t.Fatalf("want 2 related legs, got %#v", body["Orders"])
	}
	// Stop-loss leg: opposite side (Sell), StopIfTraded at 140.
	if legs[0]["BuySell"] != "Sell" || legs[0]["OrderType"] != "StopIfTraded" ||
		legs[0]["OrderPrice"] != 140.0 {
		t.Errorf("stop leg wrong: %#v", legs[0])
	}
	// Take-profit leg: opposite side (Sell), Limit at 170.
	if legs[1]["BuySell"] != "Sell" || legs[1]["OrderType"] != "Limit" ||
		legs[1]["OrderPrice"] != 170.0 {
		t.Errorf("take-profit leg wrong: %#v", legs[1])
	}
}

func TestBuildBracketBody_MarketEntryStopOnly(t *testing.T) {
	inst := instrument{Uic: 5, AssetType: "Etf"}
	b := BracketOrder{Symbol: "SPY", Side: Buy, Qty: 3, EntryType: Market, StopLoss: 400}
	body := buildBracketBody("ACC", inst, b, 3)

	if _, has := body["OrderPrice"]; has {
		t.Error("market entry must not carry OrderPrice")
	}
	legs := body["Orders"].([]map[string]any)
	if len(legs) != 1 || legs[0]["OrderType"] != "StopIfTraded" {
		t.Fatalf("want a single stop leg, got %#v", legs)
	}
}

func TestBuildBracketBody_ShortBracketSidesFlip(t *testing.T) {
	inst := instrument{Uic: 7, AssetType: "Stock"}
	b := BracketOrder{
		Symbol:     "X",
		Side:       Sell,
		Qty:        2,
		EntryType:  Market,
		StopLoss:   110,
		TakeProfit: 90,
	}
	body := buildBracketBody("ACC", inst, b, 2)
	legs := body["Orders"].([]map[string]any)
	for _, l := range legs {
		if l["BuySell"] != "Buy" {
			t.Errorf("short bracket legs must Buy to cover, got %v", l["BuySell"])
		}
	}
}

func TestPlaceBracket_Validation(t *testing.T) {
	s := &Saxo{accountKey: "ACC"}
	// Long bracket with stop above take-profit is invalid.
	if _, err := s.PlaceBracket(
		nil,
		BracketOrder{Symbol: "X", Side: Buy, Qty: 1, StopLoss: 170, TakeProfit: 150},
	); err == nil {
		t.Error("expected error for long bracket with stop >= take-profit")
	}
	// Negative quantity is invalid.
	if _, err := s.PlaceBracket(nil, BracketOrder{Symbol: "X", Side: Buy, Qty: -1}); err == nil {
		t.Error("expected error for negative quantity")
	}
	// Bad side is invalid.
	if _, err := s.PlaceBracket(
		nil,
		BracketOrder{Symbol: "X", Side: "sideways", Qty: 1},
	); err == nil {
		t.Error("expected error for invalid side")
	}
}

func TestBuildModifyBody(t *testing.T) {
	inst := instrument{Uic: 211, AssetType: "Stock"}
	m := OrderModification{
		OrderID: "ord-1",
		Symbol:  "AAPL",
		Side:    Sell,
		Type:    Limit,
		Qty:     5,
		Price:   160,
	}
	body := buildModifyBody("ACC", inst, m, 5)
	if body["OrderId"] != "ord-1" || body["OrderPrice"] != 160.0 || body["Amount"].(float64) != 5 ||
		body["BuySell"] != "Sell" || body["OrderType"] != "Limit" {
		t.Fatalf("modify body wrong: %#v", body)
	}
}
