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

// EX2-3: the backtest ratchets the trailing peak on every bar's high; live
// only saw the price current at each 30 s evaluation. A spike 100 → 106 → 101
// between two evaluations armed the backtest's tight trail and exited at
// +3.88%, while live never noticed the spike and held on the loose trail. The
// live peak must see every tick of a held symbol, and only of a held one.
func TestEveryTickOfAHeldSymbolRaisesItsTrailingPeak(t *testing.T) {
	provider := market.NewMock(time.Hour)
	defer provider.Close()

	pf := portfolio.New()
	pf.SyncPositions([]portfolio.Holding{{Symbol: "X", Qty: 10, AvgPrice: 100, LastPrice: 100, MarketValue: 1000}})

	brk := broker.NewSim(1000, func(string) (float64, bool) { return 0, false }, nil)

	e := New(
		config.EngineConfig{EvalInterval: time.Hour}, config.ExitsConfig{}, config.SafetyConfig{},
		config.StrategyConfig{}, provider, brk, risk.New(config.Limits{}, config.CostsConfig{}),
		pf, NewPriceCache(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	defer e.Close()

	e.barsStore.add("X")
	e.barsStore.add("Y")

	base := time.Now()
	for i, px := range []float64{100, 106, 101} {
		e.ingest(market.Quote{Symbol: "X", Price: px, Time: base.Add(time.Duration(i) * time.Second)})
	}

	e.ingest(market.Quote{Symbol: "Y", Price: 50, Time: base})

	e.mu.RLock()
	peakX, hasY := e.peaks["X"], e.peaks["Y"] != 0
	e.mu.RUnlock()

	if peakX != 106 {
		t.Errorf("peak = %v, want 106: the intra-window spike was missed", peakX)
	}

	if hasY {
		t.Error("an unheld symbol got a trailing peak; syncPortfolio would prune it every sync")
	}
}

// Found by the verify-change pass on EX2-3: with every tick ratcheting the
// permanent trailing peak, one bad high print from the feed (100 → 130 → 100)
// armed the tight trail against a price that never traded. An isolated
// outlier must not move the peak; a jump the next tick confirms must.
func TestAnIsolatedBadPrintDoesNotRaiseThePeak(t *testing.T) {
	provider := market.NewMock(time.Hour)
	defer provider.Close()

	pf := portfolio.New()
	pf.SyncPositions([]portfolio.Holding{{Symbol: "X", Qty: 10, AvgPrice: 100, LastPrice: 100, MarketValue: 1000}})

	brk := broker.NewSim(1000, func(string) (float64, bool) { return 0, false }, nil)

	e := New(
		config.EngineConfig{EvalInterval: time.Hour}, config.ExitsConfig{}, config.SafetyConfig{},
		config.StrategyConfig{}, provider, brk, risk.New(config.Limits{}, config.CostsConfig{}),
		pf, NewPriceCache(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	defer e.Close()

	e.barsStore.add("X")

	base := time.Now()
	for i, px := range []float64{100, 130, 100} {
		e.ingest(market.Quote{Symbol: "X", Price: px, Time: base.Add(time.Duration(i) * time.Second)})
	}

	e.mu.RLock()
	afterOutlier := e.peaks["X"]
	e.mu.RUnlock()

	if afterOutlier != 100 {
		t.Errorf("peak = %v after an isolated 130 print, want 100", afterOutlier)
	}

	for i, px := range []float64{130, 131} { // a real gap, confirmed by the next tick
		e.ingest(market.Quote{Symbol: "X", Price: px, Time: base.Add(time.Duration(10+i) * time.Second)})
	}

	e.mu.RLock()
	confirmed := e.peaks["X"]
	e.mu.RUnlock()

	if confirmed != 131 {
		t.Errorf("peak = %v after a confirmed move to 131, want 131", confirmed)
	}
}
