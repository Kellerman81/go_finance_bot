package engine

import (
	"context"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
)

// fakeRealBroker stands in for the real, configured broker. It reports a
// fixed account/position snapshot and counts SubmitOrder calls, so a test can
// assert that orders placed while test mode is active never reach it.
type fakeRealBroker struct {
	cash      float64
	positions []broker.Position
	submits   int
}

func (b *fakeRealBroker) Name() string { return "fake-real" }

func (b *fakeRealBroker) SubmitOrder(context.Context, broker.Order) (broker.OrderResult, error) {
	b.submits++

	return broker.OrderResult{Status: "filled", Terminal: true}, nil
}

func (b *fakeRealBroker) GetAccount(context.Context) (broker.Account, error) {
	return broker.Account{Cash: b.cash, Equity: b.cash}, nil
}

func (b *fakeRealBroker) GetPositions(context.Context) ([]broker.Position, error) {
	return b.positions, nil
}

// testModeEngine builds a bare engine wired enough to exercise EnableTestMode:
// a real risk.Manager (for CostsConfig), a price oracle, and the given
// starting broker.
func testModeEngine(brk broker.Broker) (*Engine, *fakeRealBroker) {
	e, _ := forgetEngine()
	e.brk = brk
	e.rm = risk.New(config.Limits{}, config.CostsConfig{})
	e.prices.Set("AAPL", 150)
	e.prices.Set("MSFT", 300)

	fb, _ := brk.(*fakeRealBroker)

	return e, fb
}

func TestEnableTestModeSeedsSimFromTheRealBrokerSnapshot(t *testing.T) {
	real := &fakeRealBroker{
		cash:      5000,
		positions: []broker.Position{{Symbol: "MSFT", Qty: 2, AvgPrice: 280}},
	}
	e, _ := testModeEngine(real)

	if err := e.EnableTestMode(context.Background(), time.Hour); err != nil {
		t.Fatalf("EnableTestMode: %v", err)
	}

	if !e.TestModeStatus().Active {
		t.Fatal("TestModeStatus().Active is false after EnableTestMode")
	}

	if e.brk.Name() != "sim" {
		t.Fatalf("e.brk = %q after enabling test mode, want the internal sim", e.brk.Name())
	}

	acct, err := e.brk.GetAccount(context.Background())
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}

	if acct.Cash != 5000 {
		t.Errorf("seeded cash = %v, want the real broker's 5000", acct.Cash)
	}

	positions, err := e.brk.GetPositions(context.Background())
	if err != nil {
		t.Fatalf("GetPositions: %v", err)
	}

	if len(positions) != 1 || positions[0].Symbol != "MSFT" || positions[0].Qty != 2 {
		t.Errorf("seeded positions = %+v, want the real broker's MSFT holding", positions)
	}
}

func TestTestModeOrdersNeverReachTheRealBroker(t *testing.T) {
	real := &fakeRealBroker{cash: 10000}
	e, fb := testModeEngine(real)

	if err := e.EnableTestMode(context.Background(), time.Hour); err != nil {
		t.Fatalf("EnableTestMode: %v", err)
	}

	if _, err := e.brk.SubmitOrder(context.Background(),
		broker.Order{Symbol: "AAPL", Side: broker.Buy, Qty: 1, Type: broker.Market}); err != nil {
		t.Fatalf("submit through the sim: %v", err)
	}

	if fb.submits != 0 {
		t.Errorf("real broker saw %d submits, want 0 — orders must be routed to the sim while test mode is active", fb.submits)
	}
}

func TestEndTestModeRestoresTheRealBroker(t *testing.T) {
	real := &fakeRealBroker{cash: 10000}
	e, _ := testModeEngine(real)

	if err := e.EnableTestMode(context.Background(), time.Hour); err != nil {
		t.Fatalf("EnableTestMode: %v", err)
	}

	e.EndTestMode("test done")

	if e.TestModeStatus().Active {
		t.Error("TestModeStatus().Active is still true after EndTestMode")
	}

	if e.brk != real {
		t.Error("e.brk was not restored to the original real broker instance")
	}
}

func TestEndTestModeIsANoOpWhenInactive(t *testing.T) {
	real := &fakeRealBroker{cash: 10000}
	e, _ := testModeEngine(real)

	e.EndTestMode("nothing to end")

	if e.brk != real {
		t.Error("EndTestMode with no active window must not touch e.brk")
	}
}

func TestEnableTestModeRefusesDuringAnActiveHalt(t *testing.T) {
	real := &fakeRealBroker{cash: 10000}
	e, _ := testModeEngine(real)

	e.SetProtectionsConfig(config.ProtectionsConfig{Enabled: true, MaxDrawdownPct: 0.20})

	p := e.Protections()
	p.ObserveEquity(100000)

	if reason := p.ObserveEquity(70000); reason == "" {
		t.Fatal("want a halt at 30% drawdown")
	}

	if err := e.EnableTestMode(context.Background(), time.Hour); err == nil {
		t.Fatal("EnableTestMode succeeded despite an active protection halt")
	}

	if e.TestModeStatus().Active {
		t.Error("test mode reports active despite EnableTestMode refusing")
	}

	if e.brk != real {
		t.Error("e.brk was swapped despite EnableTestMode refusing")
	}
}

func TestCheckTestModeExpiryEndsAfterTheDeadline(t *testing.T) {
	real := &fakeRealBroker{cash: 10000}
	e, _ := testModeEngine(real)

	if err := e.EnableTestMode(context.Background(), time.Hour); err != nil {
		t.Fatalf("EnableTestMode: %v", err)
	}

	// Not yet expired.
	e.checkTestModeExpiry(context.Background())

	if !e.TestModeStatus().Active {
		t.Fatal("test mode ended before its deadline")
	}

	e.mu.Lock()
	e.testModeDeadline = time.Now().Add(-time.Second)
	e.mu.Unlock()

	e.checkTestModeExpiry(context.Background())

	if e.TestModeStatus().Active {
		t.Error("test mode still active after its deadline elapsed")
	}

	if e.brk != real {
		t.Error("the real broker was not restored once the deadline elapsed")
	}
}

func TestCheckTestModeExpiryNeverEndsAZeroDeadlineWindow(t *testing.T) {
	real := &fakeRealBroker{cash: 10000}
	e, _ := testModeEngine(real)

	if err := e.EnableTestMode(context.Background(), 0); err != nil {
		t.Fatalf("EnableTestMode: %v", err)
	}

	e.checkTestModeExpiry(context.Background())

	if !e.TestModeStatus().Active {
		t.Error("a zero-duration (no auto-expiry) window ended on its own")
	}
}

func TestTradingOnIsTrueDuringTestModeEvenWhenTradingDisabled(t *testing.T) {
	real := &fakeRealBroker{cash: 10000}
	e, _ := testModeEngine(real)

	e.tradingEnabled = false

	if e.tradingOn() {
		t.Fatal("tradingOn() true before test mode is even active")
	}

	if err := e.EnableTestMode(context.Background(), time.Hour); err != nil {
		t.Fatalf("EnableTestMode: %v", err)
	}

	if !e.tradingOn() {
		t.Error("tradingOn() is false while test mode is active, despite trading_enabled being false — test mode must override it")
	}
}

// TestTripBreakerEndsAnActiveTestModeWindow reproduces the four circuit-
// breaker call sites (quote-stream-closed, order-rate breaker, drawdown
// halt, restored halt): each must end a running training window, not just
// flip tradingEnabled, since a broken market-data/risk assumption invalidates
// the simulation just as much as real trading.
func TestTripBreakerEndsAnActiveTestModeWindow(t *testing.T) {
	real := &fakeRealBroker{cash: 10000}
	e, _ := testModeEngine(real)

	if err := e.EnableTestMode(context.Background(), time.Hour); err != nil {
		t.Fatalf("EnableTestMode: %v", err)
	}

	e.tripBreaker("test breaker")

	if e.TestModeStatus().Active {
		t.Error("test mode still active after tripBreaker")
	}

	if e.tradingEnabled {
		t.Error("tradingEnabled still true after tripBreaker")
	}

	if e.brk != real {
		t.Error("the real broker was not restored after tripBreaker ended test mode")
	}
}

// TestTripBreakerLockedFromAnAlreadyLockedSectionDoesNotDeadlock reproduces
// the mutex-reentrancy hazard the order-rate breaker site has: it already
// holds e.mu.Lock() when it needs to trip the breaker, so it must use the
// lock-free tripBreakerLocked (never the lock-acquiring tripBreaker) and
// finish the EndTestMode call only after unlocking.
func TestTripBreakerLockedFromAnAlreadyLockedSectionDoesNotDeadlock(t *testing.T) {
	real := &fakeRealBroker{cash: 10000}
	e, _ := testModeEngine(real)

	if err := e.EnableTestMode(context.Background(), time.Hour); err != nil {
		t.Fatalf("EnableTestMode: %v", err)
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		e.mu.Lock()
		wasActive := e.tripBreakerLocked()
		e.mu.Unlock()

		if wasActive {
			e.EndTestMode("simulated order-rate trip")
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlocked calling tripBreakerLocked from an already-locked section")
	}

	if e.TestModeStatus().Active {
		t.Error("test mode still active after the locked breaker path ran")
	}
}

func TestTestModeConfigRoundTrips(t *testing.T) {
	e, _ := forgetEngine()

	cfg := config.TestModeConfig{Enabled: true, Duration: 6 * time.Hour}
	e.SetTestModeConfig(cfg)

	if got := e.TestModeConfig(); got != cfg {
		t.Errorf("TestModeConfig() = %+v, want %+v", got, cfg)
	}
}

// TestTestModeDoesNotSpendTheRealDailyBudget reproduces a production incident.
//
// risk.Manager has no idea test mode exists: every filled buy calls RecordBuy
// and the rolling total is persisted. So a training window spent the real
// max_daily_spend allowance on orders that never reached a broker — 400.38 of
// simulated buys against a 500 cap — and every real signal for the rest of
// that day was refused with "max_daily_spend leaves 99.02, below
// min_order_size_usd 100.00". The operator's cash was untouched and irrelevant.
func TestTestModeDoesNotSpendTheRealDailyBudget(t *testing.T) {
	e, _ := testModeEngine(&fakeRealBroker{cash: 5000})
	e.rm = risk.New(config.Limits{MaxDailySpend: 500}, config.CostsConfig{})

	// A real buy before the window: this one must survive.
	e.rm.RecordBuy(60)
	realSpend := e.rm.SpentToday()

	if err := e.EnableTestMode(context.Background(), time.Hour); err != nil {
		t.Fatalf("EnableTestMode: %v", err)
	}

	// Simulated buys during the window.
	e.rm.RecordBuy(200)
	e.rm.RecordBuy(200)

	if during := e.rm.SpentToday(); during <= realSpend {
		t.Fatalf("test setup: simulated buys did not move spentToday (%v)", during)
	}

	e.EndTestMode("done")

	if got := e.rm.SpentToday(); got != realSpend {
		t.Errorf("daily spend after the window = %v, want the pre-window %v — "+
			"simulated orders consumed the real budget", got, realSpend)
	}
}
