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
	if !res.Terminal {
		t.Errorf("Terminal = false, want true — order-gone is Saxo's confirmed fill signal")
	}
}

func TestSaxoConfirmFillImmediatelyGone(t *testing.T) {
	// Already absent on the first poll (filled before we could look).
	s, _ := saxoFillClient(t, []string{})
	res := s.confirmFill(context.Background(), OrderResult{ID: "42", Qty: 3, Status: "submitted"})
	if res.FilledQty != 3 || res.Status != "filled" {
		t.Errorf("want filled/3, got %q/%v", res.Status, res.FilledQty)
	}
	if !res.Terminal {
		t.Errorf("Terminal = false, want true")
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
	if res.Terminal {
		t.Errorf("Terminal = true, want false — a failed status check confirms nothing")
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

// TestSaxoSubmitOrderRefusesALimitWhenFXFails and
// TestSaxoPlaceBracketRefusesWhenFXFails reproduce the order-pricing FX
// finding: a failed rate fetch used to leave entry/stop/take-profit prices
// unconverted and send them as-is (silently ~9% off, not merely
// mislabeled) — buildBracketBody's own long/short sanity checks run before
// the conversion, so they can't catch it. Both must now refuse the order
// instead.
func TestSaxoSubmitOrderRefusesALimitWhenFXFails(t *testing.T) {
	orderPosted := false
	mux := http.NewServeMux()
	mux.HandleFunc("POST /trade/v2/orders", func(w http.ResponseWriter, r *http.Request) {
		orderPosted = true
		_ = json.NewEncoder(w).Encode(map[string]any{"OrderId": "1"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	s := &Saxo{
		baseURL: srv.URL, tokens: saxoTestToken("tok"), http: srv.Client(),
		clientKey: "CK", accountKey: "ACC", preferCcy: "EUR",
		fx:  newFXConverter(srv.Client()),
		uic: map[string]instrument{"NVDA": {Uic: 1, AssetType: "Stock", Currency: "USD"}},
	}
	s.fx.baseURL = "http://127.0.0.1:1" // nothing listens here: fx fetch fails

	_, err := s.SubmitOrder(context.Background(), Order{
		Symbol: "NVDA", Side: Buy, Qty: 1, Type: Limit, LimitPx: 150,
	})
	if err == nil {
		t.Fatal("want an error: the fx rate needed to price this limit order is unavailable")
	}

	if orderPosted {
		t.Error("the order was posted to the broker despite the unpriced limit — " +
			"it must be refused before submission, not sent unconverted")
	}
}

func TestSaxoPlaceBracketRefusesWhenFXFails(t *testing.T) {
	orderPosted := false
	mux := http.NewServeMux()
	mux.HandleFunc("POST /trade/v2/orders", func(w http.ResponseWriter, r *http.Request) {
		orderPosted = true
		_ = json.NewEncoder(w).Encode(map[string]any{"OrderId": "1"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	s := &Saxo{
		baseURL: srv.URL, tokens: saxoTestToken("tok"), http: srv.Client(),
		clientKey: "CK", accountKey: "ACC", preferCcy: "EUR",
		fx:  newFXConverter(srv.Client()),
		uic: map[string]instrument{"NVDA": {Uic: 1, AssetType: "Stock", Currency: "USD"}},
	}
	s.fx.baseURL = "http://127.0.0.1:1"

	_, err := s.PlaceBracket(context.Background(), BracketOrder{
		Symbol: "NVDA", Side: Buy, Qty: 1, EntryType: Market,
		StopLoss: 140, TakeProfit: 170,
	})
	if err == nil {
		t.Fatal("want an error: the fx rate needed to price this bracket is unavailable")
	}

	if orderPosted {
		t.Error("the bracket was posted to the broker despite unpriced stop/take-profit legs — " +
			"it must be refused before submission, not sent unconverted")
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
	body := buildModifyBody("ACC", inst, m, 5, "Limit", "")
	if body["OrderId"] != "ord-1" || body["OrderPrice"] != 160.0 || body["Amount"].(float64) != 5 ||
		body["BuySell"] != "Sell" || body["OrderType"] != "Limit" {
		t.Fatalf("modify body wrong: %#v", body)
	}
}

// TestBuildModifyBodyKeepsAStopLegAStop is the type half of BR2-2. The
// modification only knows Market and Limit, and the body always sent one of
// those: modifying a bracket's stop leg re-sent it as a sell LIMIT below the
// market, which fills at once — the opposite of a stop.
func TestBuildModifyBodyKeepsAStopLegAStop(t *testing.T) {
	inst := instrument{Uic: 211, AssetType: "Stock"}
	m := OrderModification{OrderID: "stop-1", Symbol: "AAPL", Side: Sell, Type: Limit, Qty: 5, Price: 140}

	if body := buildModifyBody("ACC", inst, m, 5, "StopIfTraded", ""); body["OrderType"] != "StopIfTraded" {
		t.Errorf("modifying a stop leg sent OrderType %v, want it kept as StopIfTraded", body["OrderType"])
	}
}

// TestSaxoModifyOrderRefusesWhenFXFails is the currency half of BR2-2:
// SubmitOrder and PlaceBracket convert a price to the instrument's currency or
// refuse, but ModifyOrder sent it as-is — a €150 limit went out as 150 USD
// while the engine's risk check treated the same number as euros.
func TestSaxoModifyOrderRefusesWhenFXFails(t *testing.T) {
	patched := false
	mux := http.NewServeMux()
	mux.HandleFunc("PATCH /trade/v2/orders", func(w http.ResponseWriter, _ *http.Request) {
		patched = true
		_ = json.NewEncoder(w).Encode(map[string]any{"OrderId": "1"})
	})
	mux.HandleFunc("GET /port/v1/orders/me", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"Data": []map[string]any{{"OrderId": "1", "OpenOrderType": "Limit"}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	s := &Saxo{
		baseURL: srv.URL, tokens: saxoTestToken("tok"), http: srv.Client(),
		clientKey: "CK", accountKey: "ACC", preferCcy: "EUR",
		fx:  newFXConverter(srv.Client()),
		uic: map[string]instrument{"NVDA": {Uic: 1, AssetType: "Stock", Currency: "USD"}},
	}
	s.fx.baseURL = "http://127.0.0.1:1" // nothing listens here: fx fetch fails

	_, err := s.ModifyOrder(context.Background(), OrderModification{
		OrderID: "1", Symbol: "NVDA", Side: Buy, Type: Limit, Qty: 1, Price: 150,
	})
	if err == nil || patched {
		t.Errorf("modify with no fx rate: err=%v, patched=%v — want it refused before sending", err, patched)
	}
}

// TestSaxoPicksTheListingTheSuffixNames and TestSaxoReportsTheWatchlistSymbol
// are the regression for BR2-4. "SAP.DE" never equalled a Saxo symbol like
// "SAP:xetr", so the instrument lookup took the first hit — here the NYSE
// ADR, priced in USD — and positions and orders came back as the bare "SAP",
// so the engine's SAP.DE record never matched the holding: a separate "SAP"
// was adopted and reconcile cleared SAP.DE's double-sell guard while its sell
// was still working.
func TestSaxoPicksTheListingTheSuffixNames(t *testing.T) {
	hits := []saxoInstrumentHit{
		{Symbol: "SAP:xnys", Identifier: 1},
		{Symbol: "SAP:xetr", Identifier: 2},
	}

	if got := pickSaxoInstrument(hits, "SAP.DE"); got.Identifier != 2 {
		t.Errorf("SAP.DE resolved to %s, want SAP:xetr", got.Symbol)
	}

	if got := pickSaxoInstrument(hits, "SAP"); got.Identifier != 1 {
		t.Errorf("a bare SAP resolved to %s, want the first exact ticker match", got.Symbol)
	}
}

func TestSaxoReportsTheWatchlistSymbol(t *testing.T) {
	s := &Saxo{uic: map[string]instrument{"SAP.DE": {Uic: 2}}}

	if got := s.symbolForUic(2, "SAP"); got != "SAP.DE" {
		t.Errorf("a position in Uic 2 reported as %q, want the watchlist's SAP.DE", got)
	}

	if got := s.symbolForUic(9, "XYZ"); got != "XYZ" {
		t.Errorf("an unresolved Uic reported as %q, want Saxo's own ticker as the fallback", got)
	}
}
