package engine

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
)

// PF3-3: two broker rows that merge into one symbol (Saxo SAP:xetr in EUR,
// SAP:xnys in USD) each wrote their own FX rate into the one fxRates entry,
// so the last row listed decided how every live tick was converted — off by
// EUR/USD when it was the other listing, and flipping with the broker's
// order. A symbol whose merging rows disagree is reported, not rated.
func TestMergedRowsInDifferentCurrenciesAreNotRatedByTheLastOne(t *testing.T) {
	t.Parallel()

	got := conflictingFXRates([]broker.Position{
		{Symbol: "SAP", FXRate: 1},
		{Symbol: "sap", FXRate: 0.92},
		{Symbol: "AAPL", FXRate: 0.92},
		{Symbol: "AAPL", FXRate: 0.92},
	})

	if !got["SAP"] || got["AAPL"] {
		t.Errorf("conflicts = %v, want SAP only (AAPL's rows agree)", got)
	}
}

// AP3-8: the paper broker took its commission as a method value at
// construction — a COPY of the costs config — so a costs save reached sizing
// and headroom but never the Sim: a flat commission raised to 5 was never
// debited and paper round trips read better than the engine's own cost model.
func TestACostsSaveReachesThePaperBroker(t *testing.T) {
	var cfg config.Config

	prices := NewPriceCache()
	prices.Set("X", 100)

	rm := risk.New(cfg.Limits, cfg.Costs)
	sim := broker.NewSim(10000, prices.Get, cfg.Costs.Commission)
	e := New(cfg.Engine, cfg.Exits, cfg.Safety, cfg.Strategy, market.NewMock(time.Hour),
		sim, rm, portfolio.New(), prices, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = e.Close() })

	e.SetCostsConfig(config.CostsConfig{CommissionFlat: 5})

	if _, err := sim.SubmitOrder(context.Background(), broker.Order{Symbol: "X", Side: broker.Buy, Qty: 1}); err != nil {
		t.Fatal(err)
	}

	acct, err := sim.GetAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if acct.Cash > 9895.0001 {
		t.Errorf("cash after a 1×100 buy = %v, want 9895 with the saved flat commission of 5", acct.Cash)
	}
}
