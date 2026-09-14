package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSaxoPositionsConvertForeignCurrency verifies that a position in an
// instrument listed in a currency other than preferCcy gets its
// price/value/PL converted using the FX rate API, and that GetAccount
// converts balances from the account's own base currency.
func TestSaxoPositionsConvertForeignCurrency(t *testing.T) {
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
	mux.HandleFunc("/port/v1/balances", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"TotalValue": 1000.0, "CashBalance": 900.0,
			"MarginAvailableForTrading": 900.0, "Currency": "USD",
		})
	})
	mux.HandleFunc("/port/v1/netpositions", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Data": []map[string]any{
				{
					"NetPositionBase": map[string]any{"Uic": 1, "Amount": 0.4513},
					"NetPositionView": map[string]any{
						"AverageOpenPrice": 221.49, "CurrentPrice": 224.72,
						"MarketValue": 101.42, "ProfitLossOnTrade": 1.46,
					},
					"DisplayAndFormat": map[string]any{"Symbol": "NVDA:xnas", "Currency": "USD"},
				},
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	s := &Saxo{
		baseURL:    srv.URL,
		tokens:     saxoTestToken("tok"),
		http:       srv.Client(),
		clientKey:  "CK",
		accountKey: "ACC",
		preferCcy:  "EUR",
		fx:         newFXConverter(srv.Client()),
	}
	s.fx.baseURL = fx.URL

	ctx := context.Background()

	acct, err := s.GetAccount(ctx)
	if err != nil {
		t.Fatalf("account: %v", err)
	}

	if acct.Currency != "EUR" || acct.Cash != 900*0.86 || acct.Equity != 1000*0.86 {
		t.Errorf("account not converted: %+v", acct)
	}

	pos, err := s.GetPositions(ctx)
	if err != nil {
		t.Fatalf("positions: %v", err)
	}

	if len(pos) != 1 {
		t.Fatalf("got %d positions, want 1", len(pos))
	}

	p := pos[0]
	if p.Symbol != "NVDA" || p.Currency != "USD" || p.FXRate != 0.86 {
		t.Errorf("position mapping wrong: %+v", p)
	}

	if want := 224.72 * 0.86; p.Current != want {
		t.Errorf("Current = %v, want %v (unconverted USD price leaking through)", p.Current, want)
	}

	if want := 101.42 * 0.86; p.MarketValue != want {
		t.Errorf("MarketValue = %v, want %v", p.MarketValue, want)
	}

	if want := 1.46 * 0.86; p.UnrealizedPL != want {
		t.Errorf("UnrealizedPL = %v, want %v (Saxo's PL, unlike Trading 212's, is in instrument currency too)", p.UnrealizedPL, want)
	}
}

// TestSaxoGetPositionsFXFailureDoesNotClaimFXRate reproduces the
// FX-fallback finding: a failed rate fetch must not report FXRate as if it
// were a real value. The engine treats any nonzero FXRate as authoritative
// and caches it (see Engine.syncPortfolio), so a fallback "1" there would
// silently overwrite a previously-correct cached rate with a wrong one.
func TestSaxoGetPositionsFXFailureDoesNotClaimFXRate(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/port/v1/netpositions", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Data": []map[string]any{
				{
					"NetPositionBase": map[string]any{"Uic": 1, "Amount": 0.4513},
					"NetPositionView": map[string]any{
						"AverageOpenPrice": 221.49, "CurrentPrice": 224.72,
						"MarketValue": 101.42, "ProfitLossOnTrade": 1.46,
					},
					"DisplayAndFormat": map[string]any{"Symbol": "NVDA:xnas", "Currency": "USD"},
				},
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	s := &Saxo{
		baseURL:    srv.URL,
		tokens:     saxoTestToken("tok"),
		http:       srv.Client(),
		clientKey:  "CK",
		accountKey: "ACC",
		preferCcy:  "EUR",
		fx:         newFXConverter(srv.Client()),
	}
	// Nothing listens here, so the rate fetch fails deterministically without
	// depending on network access to the real FX API.
	s.fx.baseURL = "http://127.0.0.1:1"

	pos, err := s.GetPositions(context.Background())
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
