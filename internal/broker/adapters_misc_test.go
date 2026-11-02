package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSimSettersReachTheNextFill guards the paper broker's late wiring:
// cmd/bot installs the price oracle after the engine exists, and a costs or
// friction save calls SetCommission/SetSlippage (AP3-8). A setter that did
// not reach the next fill made every paper round trip read better than the
// cost model the engine books. nil must reset to "frictionless" and
// "free" respectively, and a nil oracle must be ignored rather than leave
// the Sim unable to price anything.
func TestSimSettersReachTheNextFill(t *testing.T) {
	t.Parallel()

	s := NewSim(10000, func(string) (float64, bool) { return 100, true }, nil)

	if s.Name() != "sim" {
		t.Errorf("Name = %q", s.Name())
	}

	s.SetPriceOracle(func(string) (float64, bool) { return 50, true })
	s.SetPriceOracle(nil) // ignored
	s.SetSlippage(func(_ string, buy bool, px float64) float64 {
		if buy {
			return px * 1.01
		}

		return px * 0.99
	})
	s.SetCommission(func(float64) float64 { return 5 })

	ctx := context.Background()

	res, err := s.SubmitOrder(ctx, Order{Symbol: "AAA", Side: Buy, Qty: 10, Type: Market})
	if err != nil {
		t.Fatalf("buy: %v", err)
	}

	if res.FilledPx != 50.5 {
		t.Errorf("buy filled at %v, want 50.5 (new oracle 50 + 1%% slippage)", res.FilledPx)
	}

	acct, _ := s.GetAccount(ctx)
	if want := 10000 - 505.0 - 5; acct.Cash != want {
		t.Errorf("cash after buy = %v, want %v (notional + the new 5 commission)", acct.Cash, want)
	}

	s.SetSlippage(nil)
	s.SetCommission(nil)

	res, err = s.SubmitOrder(ctx, Order{Symbol: "AAA", Side: Sell, Qty: 10, Type: Market})
	if err != nil || res.FilledPx != 50 {
		t.Fatalf("sell = %+v, %v; want a frictionless fill at 50", res, err)
	}

	acct, _ = s.GetAccount(ctx)
	if want := 10000 - 510.0 + 500; acct.Cash != want {
		t.Errorf("cash after a free sell = %v, want %v", acct.Cash, want)
	}
}

// TestAlpacaInstrumentCurrency guards the FX contract the engine's rate
// cache relies on: no preferred currency (or the account's own) is a real
// 1:1, a successful conversion returns the rate, and a FAILED one is an
// error — never a silent 1 that would overwrite a correct cached rate.
func TestAlpacaInstrumentCurrency(t *testing.T) {
	t.Parallel()

	fxOK := true
	fx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !fxOK {
			http.Error(w, "down", http.StatusServiceUnavailable)

			return
		}

		_, _ = w.Write([]byte(`{"rates":{"EUR":0.9}}`))
	}))
	t.Cleanup(fx.Close)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/account" {
			_ = json.NewEncoder(w).Encode(map[string]any{"currency": "USD"})
		}
	}))
	t.Cleanup(api.Close)

	ctx := context.Background()

	same := NewAlpaca(api.URL, "k", "s", "")
	if ccy, rate, err := same.InstrumentCurrency(ctx, "AAPL"); err != nil || ccy != "USD" || rate != 1 {
		t.Errorf("no preferCcy: %q %v %v", ccy, rate, err)
	}

	if same.Name() != "alpaca" {
		t.Errorf("Name = %q", same.Name())
	}

	eur := NewAlpaca(api.URL, "k", "s", "EUR")
	eur.fx.baseURL = fx.URL

	if ccy, rate, err := eur.InstrumentCurrency(ctx, "AAPL"); err != nil || ccy != "USD" || rate != 0.9 {
		t.Errorf("USD->EUR: %q %v %v, want USD 0.9", ccy, rate, err)
	}

	broken := NewAlpaca(api.URL, "k", "s", "GBP")
	broken.fx.baseURL = fx.URL
	fxOK = false

	if _, rate, err := broken.InstrumentCurrency(ctx, "AAPL"); err == nil {
		t.Errorf("a failed FX fetch returned rate %v with no error", rate)
	}
}

// TestTrading212AssetClass guards the classifier fallback behind the data
// provider: the instrument's own metadata type is reported, an instrument
// with no type is an error rather than an empty class, and an unknown
// symbol is an error.
func TestTrading212AssetClass(t *testing.T) {
	t.Parallel()

	srv := t212FXServer(t,
		[]map[string]any{
			{"ticker": "VUSAl_EQ", "shortName": "VUSA", "isin": "IE00B3XXRP09", "currencyCode": "GBP", "type": "ETF"},
			{"ticker": "ODDl_EQ", "shortName": "ODD", "isin": "GB0000000001", "currencyCode": "GBP", "type": ""},
		},
		nil,
	)

	c := NewTrading212(srv.URL, "k", "s", nil, "")
	ctx := context.Background()

	if c.Name() != "trading212" {
		t.Errorf("Name = %q", c.Name())
	}

	if class, err := c.AssetClass(ctx, "VUSA"); err != nil || class != "ETF" {
		t.Errorf("AssetClass(VUSA) = %q, %v; want ETF", class, err)
	}

	if _, err := c.AssetClass(ctx, "ODD"); err == nil {
		t.Error("an instrument with no type was classified")
	}

	if _, err := c.AssetClass(ctx, "NOSUCH"); err == nil {
		t.Error("an unknown symbol was classified")
	}
}
