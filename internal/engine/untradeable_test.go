package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// refusal is the error a broker returns for an instrument it will never trade.
func refusal() error {
	return fmt.Errorf("%w: trading212 POST /equity/orders/market: "+
		`{"type":"/api-errors/instrument-invisible","detail":"Instrument can not be traded."}`,
		broker.ErrInstrumentUnavailable)
}

// A permanent refusal must suspend further entries. Without this the engine
// re-sent the order every eval_interval for as long as the signal held.
func TestPermanentRefusalSuspendsEntries(t *testing.T) {
	e, _ := forgetEngine()
	e.untradeable = map[string]string{}

	if blocked, _ := e.entryBlocked("QQQ"); blocked {
		t.Fatal("QQQ was blocked before any refusal")
	}

	e.noteUntradeable("QQQ", broker.Buy, refusal())

	blocked, why := e.entryBlocked("QQQ")
	if !blocked {
		t.Fatal("a permanent refusal did not suspend entries")
	}

	if why == "" {
		t.Error("no reason recorded; the operator needs to know why it stopped trying")
	}
}

// A transient failure must not. Suspending a symbol because the account was
// briefly short of cash would silently stop trading it for the rest of the run.
func TestTransientFailureDoesNotSuspendEntries(t *testing.T) {
	e, _ := forgetEngine()
	e.untradeable = map[string]string{}

	e.noteUntradeable("AAPL", broker.Buy, fmt.Errorf("trading212: status 500: server error"))

	if blocked, _ := e.entryBlocked("AAPL"); blocked {
		t.Error("a 500 suspended entries for the symbol")
	}
}

// The guard is armed by buys only. A refused SELL must never mark a symbol,
// because the block is consulted on the entry path and the position still has
// to be exitable — but more importantly, arming it from a sell would be a step
// toward a guard that can strand stock.
func TestARefusedSellDoesNotArmTheGuard(t *testing.T) {
	e, _ := forgetEngine()
	e.untradeable = map[string]string{}

	e.noteUntradeable("QQQ", broker.Sell, refusal())

	if blocked, _ := e.entryBlocked("QQQ"); blocked {
		t.Error("a refused sell armed the entry guard")
	}
}

// Untradeable exposes the set without handing out the engine's own map.
func TestUntradeableSnapshotIsACopy(t *testing.T) {
	e, _ := forgetEngine()
	e.untradeable = map[string]string{}

	e.noteUntradeable("QQQ", broker.Buy, refusal())

	snap := e.Untradeable()
	if len(snap) != 1 {
		t.Fatalf("snapshot has %d entries, want 1", len(snap))
	}

	delete(snap, "QQQ")

	if blocked, _ := e.entryBlocked("QQQ"); !blocked {
		t.Error("mutating the snapshot changed the engine's own state")
	}
}

// atTime runs fn with the engine clock fixed at t.
func atTime(t *testing.T, at time.Time, fn func()) {
	t.Helper()

	restore := now
	now = func() time.Time { return at }

	defer func() { now = restore }()

	fn()
}

// openSchedule is a broker that claims every venue is open, as Trading 212's
// cached schedule apparently did at 20:17 ET on a Friday.
type openSchedule struct{ broker.Broker }

func (openSchedule) TradingOpen(string, time.Time, time.Duration) (bool, bool) {
	return true, true
}

// An entry must not go out when the exchange calendar says the venue is shut,
// even if the broker's own schedule claims otherwise.
//
// This is the case from the live log: a QQQ buy submitted at 00:17 UTC — 20:17
// in New York, four hours after the close. marketOpen prefers the broker, which
// is right for an exit (better to act than be stuck holding) and wrong for an
// entry, where declining costs nothing.
func TestEntryNeedsBothTheBrokerAndTheCalendar(t *testing.T) {
	e, _ := forgetEngine()
	e.brk = openSchedule{}
	e.cfg = config.EngineConfig{MarketHoursCheck: true, PreOpenWindow: 5 * time.Minute}

	friday2017ET := time.Date(2026, 8, 8, 0, 17, 32, 0, time.UTC)

	atTime(t, friday2017ET, func() {
		if ok, why := e.entryAllowed("QQQ"); ok {
			t.Errorf("a US entry was allowed at 20:17 ET on a Friday (why=%q).\n"+
				"The broker schedule said open; the exchange calendar did not, "+
				"and for an entry the calendar must be able to veto.", why)
		}
	})

	// The exit path keeps the permissive behaviour on purpose: being unable to
	// sell is worse than trading in a session the calendar mis-models.
	atTime(t, friday2017ET, func() {
		if ok, _ := e.marketOpen("QQQ"); !ok {
			t.Error("the exit path stopped trusting the broker's schedule")
		}
	})
}

// Inside the session both agree and the entry proceeds.
func TestEntryAllowedInsideTheSession(t *testing.T) {
	e, _ := forgetEngine()
	e.brk = openSchedule{}
	e.cfg = config.EngineConfig{MarketHoursCheck: true, PreOpenWindow: 5 * time.Minute}

	// Wednesday 15:00 UTC = 11:00 ET, mid-session.
	atTime(t, time.Date(2026, 8, 5, 15, 0, 0, 0, time.UTC), func() {
		if ok, why := e.entryAllowed("QQQ"); !ok {
			t.Errorf("entry declined mid-session: %q", why)
		}
	})
}

// A scale-in is an entry and takes the same gates. It was the one buy path with
// neither, so it could add to a position at a venue that was closed, or re-send
// an order for an instrument the broker had already refused.
func TestScaleInRespectsTheEntryGates(t *testing.T) {
	e, _ := forgetEngine()
	e.untradeable = map[string]string{}
	e.cfg = config.EngineConfig{MarketHoursCheck: true, PreOpenWindow: 5 * time.Minute}

	e.noteUntradeable("QQQ", broker.Buy, refusal())

	why, ok := e.entryGates("QQQ")
	if ok {
		t.Fatal("the entry gates passed a refused instrument")
	}

	if !strings.Contains(why, "refuses") {
		t.Errorf("reason = %q, want it to name the broker's refusal", why)
	}
}

// refusingBroker always answers with the permanent refusal, and counts attempts.
type refusingBroker struct {
	broker.Broker

	attempts int
}

func (*refusingBroker) Name() string { return "refusing" }

func (b *refusingBroker) SubmitOrder(context.Context, broker.Order) (broker.OrderResult, error) {
	b.attempts++

	return broker.OrderResult{}, refusal()
}

func (*refusingBroker) GetAccount(context.Context) (broker.Account, error) {
	return broker.Account{Cash: 1000}, nil
}

func (*refusingBroker) GetPositions(context.Context) ([]broker.Position, error) {
	return nil, nil
}

// The whole point of the guard: the broker is asked ONCE.
//
// Before this, a live QQQ buy signal produced a rejected order every
// eval_interval — 30 seconds apart, indefinitely, each one an API call, a log
// line, an error row in the trade log and a notification.
func TestARefusedInstrumentIsOnlyEverAttemptedOnce(t *testing.T) {
	e := ledgerEngine(t)
	brk := &refusingBroker{}
	e.brk = brk
	e.untradeable = map[string]string{}
	e.cfg = config.EngineConfig{} // hours gate off; only the refusal can block

	delete(e.openPos, "X") // a fresh entry, not an add

	sig := strategy.Signal{Symbol: "QQQ", Action: strategy.Buy, Strength: 1, Reason: "test"}
	e.prices.Set("QQQ", 100)

	// Twenty evaluations: ten minutes of a live signal at eval_interval 30s.
	for range 20 {
		e.act(context.Background(), sig, nil, true)
	}

	if brk.attempts != 1 {
		t.Errorf("the broker was asked %d times, want exactly 1.\n"+
			"A permanent refusal must be learned on the first rejection; every "+
			"later evaluation has to be declined before any API call.", brk.attempts)
	}

	errs := 0

	for _, tr := range e.Trades() {
		if tr.Symbol == "QQQ" && tr.Status == "error" {
			errs++
		}
	}

	if errs != 1 {
		t.Errorf("recorded %d error rows, want 1 — each one is a notification", errs)
	}
}
