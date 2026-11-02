package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// saxoGridServer answers the instrument search, the instrument details (tick
// size 0.01) and the order POST, capturing the order body.
func saxoGridServer(t *testing.T, body *map[string]any) *Saxo {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /ref/v1/instruments", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Data":[{"Identifier":211,"AssetType":"Stock","Symbol":"AAPL:xnas","CurrencyCode":"USD"}]}`))
	})
	mux.HandleFunc("GET /ref/v1/instruments/details/211/Stock", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"TickSize":0.01}`))
	})
	mux.HandleFunc("POST /trade/v2/orders", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(body)
		_, _ = w.Write([]byte(`{"OrderId":"o1"}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &Saxo{
		baseURL: srv.URL, tokens: saxoTestToken("tok"), http: srv.Client(),
		clientKey: "CK", accountKey: "ACC",
		uic: map[string]instrument{}, uicSymbol: map[int]string{}, ticks: map[int]saxoTickScheme{},
	}
}

// BR3-5 / BR3-6: Saxo refuses an order price off the instrument's tick grid,
// and every engine price is off it (a passive offset, then an FX division:
// 163.04347826). Limit execution — which only Saxo actually rests — had every
// clip refused. And a quantity like 100 × 0.29 = 28.999999999999996 was
// floored to 28 by a bare math.Floor.
func TestASaxoLimitGoesOutOnTheTickGridAndWholeShares(t *testing.T) {
	var body map[string]any

	s := saxoGridServer(t, &body)

	held, frac := 100.0, 0.29 // at run time: a constant 100 * 0.29 is exactly 29

	_, err := s.SubmitOrder(context.Background(), Order{
		Symbol: "AAPL", Side: Buy, Qty: held * frac, Type: Limit, LimitPx: 163.04347826086956,
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := body["OrderPrice"]; got != 163.04 {
		t.Errorf("OrderPrice = %v, want 163.04 on the 0.01 grid", got)
	}

	if got := body["Amount"]; got != 29.0 {
		t.Errorf("Amount = %v, want 29 whole shares from 28.999999999999996", got)
	}
}

// BR3-7: a modification always re-sent GoodTillCancel, turning the bot's
// DayOrder limits into orders that outlive the session — and fill days later
// untracked once their watcher is gone. It keeps the working order's own.
func TestASaxoModifyKeepsTheOrdersDuration(t *testing.T) {
	t.Parallel()

	body := buildModifyBody("ACC", instrument{Uic: 211, AssetType: "Stock"},
		OrderModification{OrderID: "o1", Price: 160, Qty: 5}, 5, "Limit", "DayOrder")

	if d := body["OrderDuration"].(map[string]any)["DurationType"]; d != "DayOrder" {
		t.Errorf("a DayOrder limit was modified as %v", d)
	}
}

// BR3-8: a working order's price was reported in the instrument's currency
// beside a book in preferCcy, and ModifyOrder reads its input as preferCcy —
// a 160 typed against a 163.00 USD order went out as 173.9 USD. The price is
// reported converted, or labelled with its currency when it cannot be.
func TestASaxoWorkingOrderPriceIsInTheReportingCurrency(t *testing.T) {
	t.Parallel()

	fx := newFXConverter(nil)
	fx.cache["USDEUR"] = fxQuote{rate: 0.92, at: time.Now()}

	s := &Saxo{preferCcy: "EUR", fx: fx}

	if p, ccy := s.reportingPrice(context.Background(), 163, "USD"); ccy != "" || p < 149.95 || p > 149.97 {
		t.Errorf("163 USD reported as %v %q, want 149.96 in EUR", p, ccy)
	}
}

// BR3-9: two watchlist symbols resolving to one listing named the position
// by map iteration order, so it could flip between syncs. The first symbol
// that resolved it keeps it.
func TestASaxoUicKeepsTheSymbolThatResolvedItFirst(t *testing.T) {
	t.Parallel()

	s := &Saxo{
		uic:       map[string]instrument{"SAP": {Uic: 2}, "SAP.DE": {Uic: 2}},
		uicSymbol: map[int]string{2: "SAP.DE"},
	}

	for range 50 {
		if got := s.symbolForUic(2, "x"); got != "SAP.DE" {
			t.Fatalf("Uic 2 reported as %q, want the first-resolved SAP.DE", got)
		}
	}
}
