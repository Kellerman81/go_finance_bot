package engine

import (
	"context"
	"io"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
)

// CM2-3: cmd/bot gave the paper broker a flat spread_pct closure, and only a
// costs edit in the UI swapped in the per-symbol model — so with
// measure_spread on, a thin ETF measured at 40 bps was charged 5 bps in paper
// trading and 40 in the backtest, until someone saved the costs form. A
// simulated broker must charge the measured spread from the first fill.
func TestTheStartupSimChargesTheMeasuredSpread(t *testing.T) {
	provider := market.NewMock(time.Hour)
	defer provider.Close()

	prices := NewPriceCache()
	prices.Set("ETF", 100)

	costs := config.CostsConfig{SpreadPct: 0.0005, MeasureSpread: true}
	sim := broker.NewSim(100000, prices.Get, costs.Commission)

	e := New(
		config.EngineConfig{EvalInterval: time.Hour}, config.ExitsConfig{}, config.SafetyConfig{},
		config.StrategyConfig{}, provider, sim, risk.New(config.Limits{}, costs),
		portfolio.New(), prices, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	defer e.Close()

	e.mu.Lock()
	e.spreads["ETF"] = 0.004 // what a screening pass measured
	e.mu.Unlock()

	res, err := sim.SubmitOrder(context.Background(),
		broker.Order{Symbol: "ETF", Side: broker.Buy, Qty: 1, Type: broker.Market})
	if err != nil {
		t.Fatal(err)
	}

	if want := 100 * (1 + 0.004/2); math.Abs(res.FilledPx-want) > 1e-9 {
		t.Errorf("filled at %.4f, want %.4f (half the measured 40 bps spread)", res.FilledPx, want)
	}
}
