package engine

import (
	"log/slog"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

func TestBuyNotionalMinPositions(t *testing.T) {
	rm := risk.New(config.Limits{MaxTotalInvested: 400}, config.CostsConfig{})
	cases := []struct {
		size float64
		min  int
		want float64
	}{
		{250, 0, 250},  // fixed order size when min_positions = 0
		{250, 4, 100},  // capped at 400/4 = 100 so the budget holds >= 4 positions
		{50, 4, 50},    // order size smaller than the slice -> even more positions
		{250, 8, 50},   // 400/8
	}
	for _, c := range cases {
		e := &Engine{cfg: config.EngineConfig{OrderSizeUSD: c.size, MinPositions: c.min}, rm: rm}
		if got := e.buyNotional(); got != c.want {
			t.Errorf("size=%v min=%d: got %v, want %v", c.size, c.min, got, c.want)
		}
	}
}

func TestConfirmationStreak(t *testing.T) {
	e := &Engine{
		cfg:     config.EngineConfig{ConfirmBuy: 2, ConfirmSell: 3},
		streaks: make(map[string]signalStreak),
	}
	// Two BUYs in a row are needed: first is unconfirmed, second confirms.
	if c := e.bumpStreak("X", strategy.Buy); c >= e.confirmsNeeded(strategy.Buy) {
		t.Fatal("1st BUY should not yet be confirmed")
	}
	if c := e.bumpStreak("X", strategy.Buy); c < e.confirmsNeeded(strategy.Buy) {
		t.Fatal("2nd consecutive BUY should be confirmed")
	}
	// A HOLD breaks the run; the next SELL starts a fresh streak.
	e.bumpStreak("X", strategy.Hold)
	for i := 1; i <= 2; i++ {
		if c := e.bumpStreak("X", strategy.Sell); c >= e.confirmsNeeded(strategy.Sell) {
			t.Fatalf("SELL #%d should not yet be confirmed (need 3)", i)
		}
	}
	if c := e.bumpStreak("X", strategy.Sell); c < e.confirmsNeeded(strategy.Sell) {
		t.Fatal("3rd consecutive SELL should be confirmed")
	}
}

func TestKeepInterval(t *testing.T) {
	e := &Engine{
		cfg:     config.EngineConfig{KeepInterval: time.Hour},
		openPos: make(map[string]*OpenPosition),
	}
	// Just bought -> too young to strategy-sell.
	e.openPos["X"] = &OpenPosition{Symbol: "X", EntryTime: time.Now()}
	if e.heldLongEnough("X") {
		t.Error("a position bought just now should not be sellable within keep_interval")
	}
	// Held longer than the interval -> sellable.
	e.openPos["Y"] = &OpenPosition{Symbol: "Y", EntryTime: time.Now().Add(-2 * time.Hour)}
	if !e.heldLongEnough("Y") {
		t.Error("a position older than keep_interval should be sellable")
	}
	// No record (e.g. pre-existing position) -> not blocked.
	if !e.heldLongEnough("Z") {
		t.Error("unknown position should not be blocked by keep_interval")
	}
}

func newRateTestEngine(limit int) *Engine {
	return &Engine{
		safety:         config.SafetyConfig{MaxOrdersPerMin: limit},
		store:          nopStore{},
		log:            slog.Default(),
		tradingEnabled: true,
	}
}

func TestCircuitBreakerHaltsAutomatedTrading(t *testing.T) {
	e := newRateTestEngine(3)

	for i := 0; i < 3; i++ {
		if !e.guardOrderRate(true) {
			t.Fatalf("order %d should be allowed", i+1)
		}
	}
	// The 4th automated order in the window must trip the breaker.
	if e.guardOrderRate(true) {
		t.Fatal("4th order should be blocked by the circuit breaker")
	}
	if e.tradingEnabled {
		t.Fatal("trading must be disabled after the breaker trips")
	}
}

func TestManualOrdersBypassBreaker(t *testing.T) {
	e := newRateTestEngine(1)
	// Manual (non-automated) orders are explicit and not rate-halted.
	for i := 0; i < 5; i++ {
		if !e.guardOrderRate(false) {
			t.Fatalf("manual order %d should be allowed", i+1)
		}
	}
	if !e.tradingEnabled {
		t.Fatal("manual orders must not trip the breaker")
	}
}

func TestBreakerDisabledWhenLimitZero(t *testing.T) {
	e := newRateTestEngine(0)
	for i := 0; i < 100; i++ {
		if !e.guardOrderRate(true) {
			t.Fatalf("order %d should be allowed when breaker disabled", i+1)
		}
	}
}
