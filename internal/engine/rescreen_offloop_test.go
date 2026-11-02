package engine

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
	"github.com/Kellerman81/go_finance_bot/internal/screener"
)

// risingHistory serves 300 one-minute bars ending at `to`, enough to pass the
// screener's history filter.
type risingHistory struct{}

func (risingHistory) Candles(_ context.Context, _ string, _ market.Resolution, _, to time.Time) ([]market.Candle, error) {
	out := make([]market.Candle, 300)
	for i := range out {
		p := 100 + float64(i)*0.05
		out[i] = market.Candle{
			Time: to.Add(-time.Duration(300-i) * time.Minute), Open: p, High: p * 1.01, Low: p * 0.99,
			Close: p, Volume: 1e6,
		}
	}

	return out, nil
}

// slowProber stands in for autotune.ScreenProbe with probe.autotune on — a
// search per candidate that takes seconds to minutes. It notes whether the
// held symbol's price stayed fresh while it ran, i.e. whether the Run loop
// was still ingesting ticks.
type slowProber struct {
	calls  atomic.Int32
	during atomic.Bool
	prices *PriceCache
	symbol string
}

func (p *slowProber) Probe(context.Context, string, []market.Candle) (screener.Probe, bool) {
	n := p.calls.Add(1)

	time.Sleep(400 * time.Millisecond)

	if _, fresh := p.prices.GetFresh(p.symbol, 150*time.Millisecond); n > 1 {
		p.during.Store(fresh)
	}

	time.Sleep(400 * time.Millisecond)

	return screener.Probe{}, false
}

// AT3-1 / SC3-1: only the startup screening pass ran off the Run loop; every
// scheduled pass — history fetches, the probe's backtests and autotune
// searches, warmups — ran inline, so ticks were not ingested and checkExits
// did not run for the length of a pass. Production has the probe enabled; a
// pass took minutes. The held symbol's price must stay fresh during a
// scheduled pass.
func TestAScheduledScreeningPassDoesNotStopTheRunLoop(t *testing.T) {
	var cfg config.Config
	cfg.Engine.EvalInterval = 20 * time.Millisecond

	prices := NewPriceCache()
	feed := market.NewMock(10 * time.Millisecond)
	pf := portfolio.New()
	pf.SetCash(10000)
	brk := broker.NewSim(10000, prices.Get, cfg.Costs.Commission)
	e := New(cfg.Engine, cfg.Exits, cfg.Safety, cfg.Strategy, feed, brk, risk.New(cfg.Limits, cfg.Costs),
		pf, prices, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	t.Cleanup(func() {
		e.Close()
		feed.Close()
	})

	if err := e.AddSymbols("HELD"); err != nil {
		t.Fatal(err)
	}

	p := &slowProber{prices: prices, symbol: "HELD"}
	e.SetHistorySource(risingHistory{})
	e.SetScreenProber(p)
	e.SetScreenerConfig(config.ScreenerConfig{
		Enabled: true, Interval: 1500 * time.Millisecond, Universe: []string{"CAND"},
		Filters: []string{"history"}, KeepManual: true,
		Probe: config.ScreenerProbeConfig{Enabled: true, Backtest: true},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go e.Run(ctx)

	deadline := time.Now().Add(4 * time.Second)
	for p.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	time.Sleep(900 * time.Millisecond)

	if p.calls.Load() < 2 {
		t.Fatalf("the scheduled pass never probed (calls=%d)", p.calls.Load())
	}

	if !p.during.Load() {
		t.Error("the held symbol's price went stale during a scheduled screening pass: the Run loop was blocked")
	}
}

// SC3-1's other half: passes overlapped — the startup pass and the first
// scheduled one ran Screen at once, and the API's rescreen could join them.
// A second pass started while one runs does nothing and says so.
func TestScreeningPassesDoNotOverlap(t *testing.T) {
	e := ledgerEngine(t)
	e.screening.Store(true) // a pass is in progress

	if e.rescreen(context.Background()) {
		t.Error("a second screening pass ran while one was in progress")
	}

	e.screening.Store(false)

	if !e.rescreen(context.Background()) {
		t.Error("a pass did not run with none in progress")
	}
}
