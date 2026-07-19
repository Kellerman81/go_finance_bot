package engine

import (
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/market"
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
	if bs.ingest("AAPL", 100, base) {
		t.Error("first tick should not complete a bar")
	}
	bs.ingest("AAPL", 101, base.Add(10*time.Second))
	if !bs.ingest("AAPL", 102, base.Add(time.Minute)) {
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
