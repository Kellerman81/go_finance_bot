package api

import (
	"net/url"
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
)

// The whole wiring for min_positions_per_asset_class, end to end: the settings
// form decodes it, the engine rebuilds the guard from it, and the risk manager
// — a different object, handed the same guard — sizes the next buy against it.
// A break anywhere along that chain leaves the setting visible in the UI and
// enforcing nothing, which is the failure mode worth a test.
func TestAssetClassSlotReachesTheRiskManager(t *testing.T) {
	s := newTestServer(t)

	rec := postForm(t, s, "/v2/form/settings/portfolio", url.Values{
		"enabled":                       {"true"},
		"max_pct_per_asset_class":       {"stock:0.5"},
		"min_positions_per_asset_class": {"stock:2"},
		"asset_classes":                 {"AAPL:stock"},
	})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	cfg, _, classes := s.eng.PortfolioConfig()
	if got := cfg.MinPositionsPerAssetClass["stock"]; got != 2 {
		t.Fatalf("min_positions_per_asset_class[stock] = %d, want 2", got)
	}

	if classes["AAPL"] != "stock" {
		t.Fatalf("asset class map = %v, want AAPL as a stock", classes)
	}

	// Equity 1000, a 50% stock budget split in two: 250 per position.
	snap := portfolio.Snapshot{
		Cash: 1000, Equity: 1000, Holdings: map[string]portfolio.Holding{},
	}

	order := broker.Order{Symbol: "AAPL", Side: broker.Buy, Qty: 8, Type: broker.Market}

	authOrder, d := s.rm.Authorize(order, 100, snap)
	if !d.Approved {
		t.Fatalf("buy refused: %s", d.Reason)
	}

	if authOrder.Qty != 2.5 {
		t.Errorf("qty = %v, want 2.5 — the 250 slot at 100, not the 800 asked for", authOrder.Qty)
	}
}

// A count is a count. Catching it in the form names the field the operator
// typed; letting it through to the JSON layer would name a Go type instead.
func TestAssetClassSlotRejectsAFraction(t *testing.T) {
	s := newTestServer(t)

	rec := postForm(t, s, "/v2/form/settings/portfolio", url.Values{
		"enabled":                       {"true"},
		"max_pct_per_asset_class":       {"stock:0.5"},
		"min_positions_per_asset_class": {"stock:2.5"},
	})

	if rec.Code == 200 {
		t.Fatal("a fractional position count was accepted")
	}

	if body := rec.Body.String(); !strings.Contains(body, "min_positions_per_asset_class") {
		t.Errorf("error %q does not name the field", body)
	}
}

// Slots without a class budget used to be refused as a no-op. They are now a
// real setting — the class shares whatever fraction the named budgets left
// unclaimed — so the form must accept them. What the form must still refuse is
// an allocation claiming more than the whole book.
func TestAssetClassSlotWithoutABudgetIsAccepted(t *testing.T) {
	s := newTestServer(t)

	rec := postForm(t, s, "/v2/form/settings/portfolio", url.Values{
		"enabled":                       {"true"},
		"min_positions_per_asset_class": {"stock:2"},
	})

	if rec.Code != 200 {
		t.Fatalf("slots for an unbudgeted class were refused (%d): %s", rec.Code, rec.Body.String())
	}

	if got := (config.PortfolioConfig{
		Enabled:                   true,
		MinPositionsPerAssetClass: map[string]int{"stock": 2},
	}).Validate(); got != nil {
		t.Errorf("Validate refused slots that share the leftover budget: %v", got)
	}
}

// The budgets are fractions of one book, so the form must refuse an
// allocation that hands out more than all of it — the check an operator is
// most likely to need, since the caps are edited one field at a time.
func TestAssetClassBudgetsOverOneHundredPercentAreRefused(t *testing.T) {
	s := newTestServer(t)

	rec := postForm(t, s, "/v2/form/settings/portfolio", url.Values{
		"enabled":                 {"true"},
		"max_pct_per_asset_class": {"stock:0.7,etf:0.3,unclassified:0.1"},
	})

	if rec.Code == 200 {
		t.Fatal("budgets summing to 1.1 were accepted")
	}

	if body := rec.Body.String(); !strings.Contains(body, "max_pct_per_asset_class") {
		t.Errorf("error %q does not name the field", body)
	}
}
