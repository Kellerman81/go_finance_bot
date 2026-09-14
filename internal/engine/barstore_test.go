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

func TestRingFixedCapacityKeepsNewest(t *testing.T) {
	r := newRing(3)
	base := time.Now()
	for i := 0; i < 5; i++ {
		r.push(market.Candle{Close: float64(i), Time: base.Add(time.Duration(i) * time.Minute)})
	}
	if r.len() != 3 {
		t.Fatalf("ring len = %d, want 3 (capacity)", r.len())
	}
	got := r.appendTo(nil)
	// Only the last three (2,3,4) survive, in chronological order.
	for i, want := range []float64{2, 3, 4} {
		if got[i].Close != want {
			t.Errorf("ring[%d] = %.0f, want %.0f", i, got[i].Close, want)
		}
	}
}

func TestBarStoreIngestAndSeries(t *testing.T) {
	bs := newBarStore(100, time.Minute)
	bs.add("AAPL")
	base := time.Now().Truncate(time.Minute)
	// Two ticks in bar 0, then a tick in bar 1 completes bar 0.
	if bs.ingest("AAPL", 100, 0, base) {
		t.Error("first tick should not complete a bar")
	}
	bs.ingest("AAPL", 101, 0, base.Add(10*time.Second))
	if !bs.ingest("AAPL", 102, 0, base.Add(time.Minute)) {
		t.Error("tick in next bucket should complete the prior bar")
	}
	series := bs.series("AAPL")
	// One completed bar (high 101) + the in-progress bar (102).
	if len(series) != 2 {
		t.Fatalf("series len = %d, want 2 (1 completed + in-progress)", len(series))
	}
	if series[0].High != 101 {
		t.Errorf("completed bar high = %.0f, want 101", series[0].High)
	}
	if series[1].Close != 102 {
		t.Errorf("in-progress close = %.0f, want 102", series[1].Close)
	}
}

func TestBarStoreSeedKeepsRecent(t *testing.T) {
	bs := newBarStore(3, time.Minute)
	bs.add("X")
	base := time.Now()
	candles := make([]market.Candle, 10)
	for i := range candles {
		candles[i] = market.Candle{
			Close: float64(i),
			Time:  base.Add(time.Duration(i) * time.Minute),
		}
	}
	bs.seed("X", candles)
	series := bs.series("X")
	if len(series) != 3 {
		t.Fatalf("seeded series len = %d, want 3 (capacity)", len(series))
	}
	if series[0].Close != 7 || series[2].Close != 9 {
		t.Errorf("seeded window = [%.0f..%.0f], want [7..9]", series[0].Close, series[2].Close)
	}
}

func TestBarStoreRemove(t *testing.T) {
	bs := newBarStore(10, time.Minute)
	bs.add("X")
	if !bs.has("X") {
		t.Fatal("X should be registered")
	}
	bs.remove("X")
	if bs.has("X") {
		t.Error("X should be gone after remove")
	}
	if bs.series("X") != nil {
		t.Error("series of removed symbol should be nil")
	}
}

// TestEngineSeriesMatchesTheConfiguredBarInterval reproduces the AT-9 fix:
// internal/autotune and the Strategy tab's per-symbol optimizer used to call
// SeriesResampled(symbol, market.Resolution("1")) — a fixed 1-minute
// resampling that silently diverges from a configured sub-minute
// BarInterval (nothing in config.Validate forbids one, and this field is
// documented as configurable: "Larger bars (e.g. 5m) trade on coarser, less
// noisy candles"). With a 30s BarInterval, two completed 30s bars fold into
// a single resampled 1-minute bucket, so a caller backtesting against
// SeriesResampled(..., "1") was tuning indicator periods/exit levels
// against a bar size the live engine never actually evaluates on. Series
// must return exactly what evaluateSignals sees — no resampling at all.
func TestEngineSeriesMatchesTheConfiguredBarInterval(t *testing.T) {
	provider := market.NewMock(time.Hour)
	defer provider.Close()

	pf := portfolio.New()
	pf.SetCash(1000)
	brk := broker.NewSim(1000, func(string) (float64, bool) { return 0, false }, nil)
	rm := risk.New(config.Limits{MaxOpenPositions: 10}, config.CostsConfig{})
	prices := NewPriceCache()

	e := New(
		config.EngineConfig{EvalInterval: time.Hour, BarInterval: 30 * time.Second},
		config.ExitsConfig{}, config.SafetyConfig{}, config.StrategyConfig{},
		provider, brk, rm, pf, prices, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	defer e.Close()

	e.barsStore.add("X")

	base := time.Now().Truncate(time.Minute)
	e.ingest(market.Quote{Symbol: "X", Price: 100, Time: base})
	e.ingest(market.Quote{Symbol: "X", Price: 101, Time: base.Add(30 * time.Second)})
	e.ingest(market.Quote{Symbol: "X", Price: 102, Time: base.Add(60 * time.Second)})

	raw := e.Series("X")
	if len(raw) != 3 {
		t.Fatalf("Series len = %d, want 3 (one bar per 30s BarInterval bucket)", len(raw))
	}

	resampledTo1m := e.SeriesResampled("X", market.Resolution("1"))
	if len(resampledTo1m) != 2 {
		t.Fatalf("SeriesResampled(\"1\") len = %d, want 2 (two 30s bars fold into the first 1-minute bucket) — "+
			"this is exactly the divergence Series must avoid", len(resampledTo1m))
	}

	if len(raw) == len(resampledTo1m) {
		t.Fatal("Series and SeriesResampled(\"1\") returned the same length — " +
			"this BarInterval choice failed to demonstrate the divergence")
	}
}
