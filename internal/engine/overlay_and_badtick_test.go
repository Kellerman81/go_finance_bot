package engine

import (
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

// PF2-3: right after AAPL closes the broker still reports a 0-qty AAPL row;
// the bot re-buys AAPL and the fill is pending. The overlay turned that
// non-position into a real one without counting it, so OpenPositions read 1
// while two were carried, and max_open_positions 2 admitted a third symbol.
func TestPendingOverlayOnAZeroRowCountsAsAPosition(t *testing.T) {
	e, _ := forgetEngine()
	e.pf.SetCash(1000)
	e.pf.SyncPositions([]portfolio.Holding{
		{Symbol: "AAPL", Qty: 0},
		{Symbol: "MSFT", Qty: 1, MarketValue: 100},
	})

	e.openPos["AAPL"] = &OpenPosition{Symbol: "AAPL", Qty: 5, EntryPrice: 2}

	var snap portfolio.Snapshot

	atTime(t, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), func() {
		snap = e.pendingExposure(e.pf.Snapshot())
	})

	if snap.OpenPositions != 2 {
		t.Errorf("OpenPositions = %d, want 2 (MSFT held + AAPL pending)", snap.OpenPositions)
	}
}

// PF2-5: a zero print reached the price cache, and the next positions sync
// valued the holding at the cached 0 — a fake total loss the drawdown
// protection reads. ingest must drop it before anything sees it.
func TestIngestDropsANonPositivePrint(t *testing.T) {
	provider := market.NewMock(time.Hour)
	defer provider.Close()

	pf := portfolio.New()
	brk := broker.NewSim(1000, func(string) (float64, bool) { return 0, false }, nil)
	prices := NewPriceCache()

	e := New(
		config.EngineConfig{EvalInterval: time.Hour}, config.ExitsConfig{}, config.SafetyConfig{},
		config.StrategyConfig{}, provider, brk, risk.New(config.Limits{}, config.CostsConfig{}),
		pf, prices, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	defer e.Close()

	e.barsStore.add("X")
	e.ingest(market.Quote{Symbol: "X", Price: 100, Time: time.Now()})
	e.ingest(market.Quote{Symbol: "X", Price: 0, Time: time.Now()})

	if got, _ := prices.Get("X"); got != 100 {
		t.Errorf("cached price = %v after a zero print, want the last good 100", got)
	}
}
