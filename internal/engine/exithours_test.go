package engine

import (
	"context"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
)

// max_hold is the only exit that can newly trigger while a venue is closed:
// every other barrier is compared against a price, and out of hours the price
// cache is frozen at the last tick, so a barrier that did not trip at the close
// will not trip overnight. The clock keeps moving regardless.
//
// checkExits runs every eval_interval, so an exit that fires into a closed venue
// used to resubmit every 30 seconds until it reopened.

// countingBroker records how many orders were submitted.
type countingBroker struct {
	broker.Broker

	orders int
}

func (b *countingBroker) Name() string { return "counting" }

func (b *countingBroker) SubmitOrder(
	_ context.Context,
	o broker.Order,
) (broker.OrderResult, error) {
	b.orders++

	return broker.OrderResult{
		ID: "1", Symbol: o.Symbol, Side: o.Side, Qty: o.Qty,
		FilledQty: o.Qty, FilledPx: 100, Status: "filled", SubmittedAt: time.Now(),
	}, nil
}

func (*countingBroker) GetAccount(context.Context) (broker.Account, error) {
	return broker.Account{Cash: 1000}, nil
}

func (*countingBroker) GetPositions(context.Context) ([]broker.Position, error) {
	return nil, nil
}

// scheduledBroker answers the market-hours question the way a real broker does,
// via broker.MarketScheduler. open/known are what TradingOpen returns.
type scheduledBroker struct {
	countingBroker

	open, known bool
}

func (b *scheduledBroker) TradingOpen(string, time.Time, time.Duration) (bool, bool) {
	return b.open, b.known
}

// hoursEngine builds an engine holding one position, with the market-hours gate
// on and a broker that reports the given schedule.
func hoursEngine(t *testing.T, symbol string, open, known bool) (*Engine, *scheduledBroker) {
	t.Helper()

	e := ledgerEngine(t)
	brk := &scheduledBroker{open: open, known: known}
	e.brk = brk
	e.cfg = config.EngineConfig{MarketHoursCheck: true}

	// Re-key the fixture's position onto the symbol under test: the exchange
	// calendar reads the suffix, so the name is the input that matters here.
	delete(e.openPos, "X")

	e.openPos[symbol] = &OpenPosition{
		Symbol: symbol, Qty: 2, OrigQty: 2, EntryPrice: 100,
		EntryTime: time.Now().Add(-time.Hour),
	}
	e.pf.SyncPositions([]portfolio.Holding{
		{Symbol: symbol, Qty: 2, AvgPrice: 100, LastPrice: 120},
	})
	e.prices.Set(symbol, 120)

	return e, brk
}

// An exit must not be sent to a venue that cannot fill it.
func TestExitIsDeferredWhileTheVenueIsClosed(t *testing.T) {
	e, brk := hoursEngine(t, "AAPL", false, true) // venue closed, broker is sure

	if e.exitPosition(context.Background(), "AAPL", 2, 120,
		"max hold", true, false, e.pf.Snapshot()) {
		t.Error("exitPosition reported a fill with the venue closed")
	}

	if brk.orders != 0 {
		t.Errorf("submitted %d orders into a closed venue.\ncheckExits runs every "+
			"eval_interval, so this repeats until the venue reopens — thousands of "+
			"rejected orders across a weekend against a rate-limited broker.",
			brk.orders)
	}

	if _, still := e.openPos["AAPL"]; !still {
		t.Error("the position record was cleared by an exit that never went out")
	}
}

// And it must still fire normally once the venue is open, or the gate would
// simply have disabled protective exits.
func TestExitStillFiresWhileTheVenueIsOpen(t *testing.T) {
	e, brk := hoursEngine(t, "AAPL", true, true) // venue open

	if !e.exitPosition(context.Background(), "AAPL", 2, 120,
		"max hold", true, false, e.pf.Snapshot()) {
		t.Fatal("exitPosition declined with the venue open")
	}

	if brk.orders != 1 {
		t.Errorf("submitted %d orders, want 1", brk.orders)
	}
}

// TestExitIsADryRunWhileHaltedByDefault checks the pre-existing default
// behaviour is unchanged: with bypassHalt false, a halted/disabled trading
// toggle still turns an exit into a dry-run, not a real order.
func TestExitIsADryRunWhileHaltedByDefault(t *testing.T) {
	e, brk := hoursEngine(t, "AAPL", true, true) // venue open

	if e.exitPosition(context.Background(), "AAPL", 2, 120,
		"max hold", false, false, e.pf.Snapshot()) {
		t.Error("exitPosition reported a real fill with trading off and no bypass")
	}

	if brk.orders != 0 {
		t.Errorf("submitted %d orders while trading was off, want 0 (dry-run)", brk.orders)
	}
}

// TestExitBypassesTradingHaltWhenConfigured checks the opt-in: with
// bypassHalt true, a protective exit keeps submitting a REAL order even
// though trading is off — this is the option added so a stop-loss can still
// cap a loss during the very drawdown that halted the bot.
func TestExitBypassesTradingHaltWhenConfigured(t *testing.T) {
	e, brk := hoursEngine(t, "AAPL", true, true) // venue open

	if !e.exitPosition(context.Background(), "AAPL", 2, 120,
		"max hold", false, true, e.pf.Snapshot()) {
		t.Fatal("exitPosition declined despite bypassHalt")
	}

	if brk.orders != 1 {
		t.Errorf("submitted %d orders with bypassHalt set, want 1 (a real fill)", brk.orders)
	}
}

// A symbol the exchange calendar does not recognise must stay exitable — at any
// hour, weekend included, since an unknown venue has no hours to be outside of.
// Getting this backwards would strand exactly the awkward holdings with no stop
// and no time barrier, permanently. (The entry path resolves the same
// uncertainty the other way; see TestWeekendBlocksAnEntryOnAnUnknownVenue.)
func TestUnknownSymbolIsAlwaysExitable(t *testing.T) {
	// The broker cannot answer, so the suffix calendar decides — and it does not
	// recognise .ZZ, which must mean "allowed", not "blocked".
	e, brk := hoursEngine(t, "FOO.ZZ", false, false)

	if !e.exitPosition(context.Background(), "FOO.ZZ", 2, 120,
		"max hold", true, false, e.pf.Snapshot()) {
		t.Error("an exit for a symbol with no exchange mapping was blocked")
	}

	if brk.orders != 1 {
		t.Errorf("submitted %d orders, want 1", brk.orders)
	}
}
