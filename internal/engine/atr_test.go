package engine

import (
	"io"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/strategy/indicators"
)

// atrEngine builds an engine holding one symbol's seeded candle history.
func atrEngine(t *testing.T, cfg config.ExitsConfig, candles []market.Candle) *Engine {
	t.Helper()

	e := &Engine{
		exits:     cfg,
		barsStore: newBarStore(3000, time.Minute),
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	e.barsStore.add("X")
	e.barsStore.seed("X", candles)

	return e
}

// atrCandles builds a deterministic series.
func atrCandles(n int) []market.Candle {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	out := make([]market.Candle, n)
	price := 100.0

	for i := range out {
		// A repeating but non-trivial shape, so the ATR is not constant.
		price *= 1 + math.Sin(float64(i)/7)*0.004

		half := price * (0.002 + math.Abs(math.Cos(float64(i)/11))*0.006)
		out[i] = market.Candle{
			Time: base.Add(time.Duration(i) * time.Minute),
			Open: price, High: price + half, Low: price - half, Close: price,
		}
	}

	return out
}

// atrFor must read exactly the window the backtester's atrAt reads — the last
// period*3 bars — so a live stop and a backtested stop are the same number.
//
// Wilder's ATR is a recursive average: old terms decay geometrically but never
// vanish. Reading the whole history live while the backtest read a bounded
// window produced two slightly different stops from one configuration, which is
// exactly the live/backtest divergence internal/exits exists to prevent.
func TestATRForUsesTheBacktestWindow(t *testing.T) {
	t.Parallel()

	const period = 14

	cfg := config.ExitsConfig{Enabled: true, ATRPeriod: period, ATRStopMult: 2}
	candles := atrCandles(1000)
	e := atrEngine(t, cfg, candles)

	// The same window atrAt takes: the last period*3 bars.
	window := candles[len(candles)-period*3:]
	high := make([]float64, len(window))
	low := make([]float64, len(window))
	closes := make([]float64, len(window))

	for i, c := range window {
		high[i], low[i], closes[i] = c.High, c.Low, c.Close
	}

	want, ok := indicators.ATRLast(high, low, closes, period)
	if !ok {
		t.Fatal("reference ATR undefined")
	}

	if got := e.atrFor("X"); math.Abs(got-want) > 1e-9 {
		t.Errorf("atrFor = %v, want %v (the last %d bars)", got, want, period*3)
	}
}

// With the ATR stops switched off, no window is read at all.
func TestATRForSkipsWorkWhenUnused(t *testing.T) {
	t.Parallel()

	e := atrEngine(t, config.ExitsConfig{Enabled: true, StopLossPct: 0.05}, atrCandles(200))

	if got := e.atrFor("X"); got != 0 {
		t.Errorf("atrFor = %v with no ATR stop configured, want 0", got)
	}
}

// Too little history is reported as no value rather than a wrong one.
func TestATRForNeedsHistory(t *testing.T) {
	t.Parallel()

	cfg := config.ExitsConfig{Enabled: true, ATRPeriod: 14, ATRStopMult: 2}

	if got := atrEngine(t, cfg, atrCandles(10)).atrFor("X"); got != 0 {
		t.Errorf("atrFor = %v on 10 bars with period 14, want 0", got)
	}

	if got := atrEngine(t, cfg, nil).atrFor("UNKNOWN"); got != 0 {
		t.Errorf("atrFor = %v for an untracked symbol, want 0", got)
	}
}

// The exit sweep calls this per held symbol on every cycle, for as long as the
// bot runs. It must not allocate once its buffers are warm.
func TestATRForDoesNotAllocateWhenWarm(t *testing.T) {
	cfg := config.ExitsConfig{Enabled: true, ATRPeriod: 14, ATRStopMult: 2}
	e := atrEngine(t, cfg, atrCandles(1000))

	e.atrFor("X") // warm the scratch buffers

	if avg := testing.AllocsPerRun(50, func() { e.atrFor("X") }); avg != 0 {
		t.Errorf("atrFor allocated %.1f times per call once warm, want 0", avg)
	}
}

// ----- the bounded ring read the above depends on -----

func TestRingTailReturnsTheNewestBars(t *testing.T) {
	t.Parallel()

	candles := atrCandles(100)
	s := newBarStore(3000, time.Minute)

	s.add("X")
	s.seed("X", candles)

	got := s.tail("X", 10, nil)
	if len(got) != 10 {
		t.Fatalf("tail(10) returned %d bars", len(got))
	}

	for i, c := range got {
		if want := candles[len(candles)-10+i]; c.Time != want.Time {
			t.Fatalf("bar %d is %v, want %v — tail must return the NEWEST bars",
				i, c.Time, want.Time)
		}
	}

	// Asking for everything must still return everything.
	if all := s.tail("X", 0, nil); len(all) != len(candles) {
		t.Errorf("tail(0) returned %d bars, want all %d", len(all), len(candles))
	}

	// And more than is held is not an error.
	if more := s.tail("X", 5000, nil); len(more) != len(candles) {
		t.Errorf("tail(5000) returned %d bars, want all %d", len(more), len(candles))
	}
}

// Reusing a caller's buffer is what keeps the exit sweep allocation-free.
func TestRingTailReusesTheProvidedBuffer(t *testing.T) {
	t.Parallel()

	s := newBarStore(3000, time.Minute)

	s.add("X")
	s.seed("X", atrCandles(100))

	buf := make([]market.Candle, 0, 64)

	first := s.tail("X", 20, buf[:0])
	if cap(first) != cap(buf) {
		t.Errorf("tail allocated a new buffer (cap %d) instead of using the one given (cap %d)",
			cap(first), cap(buf))
	}

	// A second read into the same buffer must not append to the first result.
	second := s.tail("X", 20, first[:0])
	if len(second) != 20 {
		t.Errorf("second read returned %d bars, want 20", len(second))
	}
}

// series() must keep returning the whole history — several callers depend on it.
func TestSeriesStillReturnsEverything(t *testing.T) {
	t.Parallel()

	candles := atrCandles(250)
	s := newBarStore(3000, time.Minute)

	s.add("X")
	s.seed("X", candles)

	got := s.series("X")
	if len(got) != len(candles) {
		t.Fatalf("series returned %d bars, want %d", len(got), len(candles))
	}

	if got[0].Time != candles[0].Time || got[len(got)-1].Time != candles[len(candles)-1].Time {
		t.Error("series is not in chronological order")
	}
}
