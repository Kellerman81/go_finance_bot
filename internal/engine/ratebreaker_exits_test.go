package engine

import (
	"context"
	"testing"
	"time"
)

// NT3-1: the order-rate breaker gated protective exits. On a crash day with
// max_orders_per_min 2, every stop past the second in the minute was refused —
// bypass_trading_halt ones included, though that setting's docs name this
// breaker as one it bypasses. Exits are never blocked: they are counted, and
// still go out.
func TestTheRateBreakerNeverRefusesAnExit(t *testing.T) {
	e := ledgerEngine(t)
	brk := &fillingBroker{held: 2}
	e.brk = brk
	e.safety.MaxOrdersPerMin = 2
	e.orderTimes = []time.Time{time.Now(), time.Now()} // the minute's budget is spent
	e.tradingEnabled = false                           // and the breaker has tripped

	if !e.exitPosition(context.Background(), "X", 2, 120, "stop-loss", false, true, e.pf.Snapshot()) {
		t.Fatalf("a bypass_trading_halt stop-loss was refused by the order-rate breaker (orders sent: %d)", brk.orders)
	}
}

// The common path of NT3-1: trading ON, the breaker at its limit, an ordinary
// stop-loss. The old code counted the exit against the minute's budget,
// tripped the breaker and refused the sale.
func TestAStopLossGoesOutWithTheRateBreakerAtItsLimit(t *testing.T) {
	e := ledgerEngine(t)
	brk := &fillingBroker{held: 2}
	e.brk = brk
	e.safety.MaxOrdersPerMin = 2
	e.orderTimes = []time.Time{time.Now(), time.Now()} // the minute's budget is spent
	e.tradingEnabled = true

	if !e.exitPosition(context.Background(), "X", 2, 120, "stop-loss", true, false, e.pf.Snapshot()) {
		t.Fatalf("a stop-loss was refused by the order-rate breaker at its limit (orders sent: %d)", brk.orders)
	}
}

// NT3-1's second half: every refused automated attempt re-tripped the breaker
// and recorded another "halted" — each an urgent-lane alert — so one trip could
// fill the 8-slot urgent queue and evict a real drawdown halt. The halt is
// announced once, on the transition.
func TestOneBreakerTripIsAnnouncedOnce(t *testing.T) {
	e := ledgerEngine(t)
	e.tradingEnabled = true
	e.safety.MaxOrdersPerMin = 1
	e.orderTimes = []time.Time{time.Now()}

	for range 4 {
		if e.guardOrderRate(true) {
			t.Fatal("an automated order passed a breaker at its limit")
		}
	}

	halts := 0

	for _, tr := range e.Trades() {
		if tr.Status == "halted" {
			halts++
		}
	}

	if halts != 1 {
		t.Errorf("%d halted records for one trip, want 1", halts)
	}
}
