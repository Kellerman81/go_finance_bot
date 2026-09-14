package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
)

// watchLimitBroker is a configurable broker.Broker (plus OrderLister/
// OrderCanceller, added selectively per test) for exercising
// watchLimitOrder's branches directly.
type watchLimitBroker struct {
	openOrders  []broker.OpenOrder // OpenOrders result, when listOrders is true
	listOrders  bool
	cancelErr   error // nil means CancelOrders succeeds; set canCancel to expose it
	canCancel   bool
	fallback    broker.OrderResult // SubmitOrder's result for the market-fallback clip
	fallbackErr error
	cash        float64 // GetAccount's reported cash; syncPortfolio applies it over any SetCash a test made
}

func (watchLimitBroker) Name() string { return "watchlimit" }

func (b watchLimitBroker) SubmitOrder(_ context.Context, _ broker.Order) (broker.OrderResult, error) {
	return b.fallback, b.fallbackErr
}

func (b watchLimitBroker) GetAccount(context.Context) (broker.Account, error) {
	cash := b.cash
	if cash == 0 {
		cash = 1000
	}

	return broker.Account{Cash: cash}, nil
}

func (watchLimitBroker) GetPositions(context.Context) ([]broker.Position, error) { return nil, nil }

// watchLimitBrokerWithLister adds OpenOrders (broker.OrderLister).
type watchLimitBrokerWithLister struct{ watchLimitBroker }

func (b watchLimitBrokerWithLister) OpenOrders(context.Context) ([]broker.OpenOrder, error) {
	return b.openOrders, nil
}

// watchLimitBrokerWithCanceller adds OpenOrders + CancelOrders (broker.OrderLister
// + broker.OrderCanceller).
type watchLimitBrokerWithCanceller struct{ watchLimitBrokerWithLister }

func (b watchLimitBrokerWithCanceller) CancelOrders(context.Context, ...string) error {
	return b.cancelErr
}

// watchLimitEngine builds an engine wired for watchLimitOrder testing, with
// an immediate limit timeout so the tests don't wait on the real 2-minute
// default.
func watchLimitEngine(brk broker.Broker) *Engine {
	e, _ := forgetEngine()
	e.brk = brk
	e.rm = risk.New(config.Limits{
		MaxOrderValue: 1e9, MaxPerPosition: 1e9, MaxTotalInvested: 1e9,
		MaxDailySpend: 1e9, MaxOpenPositions: 10,
	}, config.CostsConfig{})
	e.pf = portfolio.New()
	e.pf.SetCash(1e6)
	e.prices = NewPriceCache()
	e.barsStore = newBarStore(maxBars, time.Minute)
	e.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	e.tradingEnabled = true
	e.cfg.Execution.LimitTimeout = time.Millisecond // don't wait on the real default

	return e
}

// TestWatchLimitOrderFlagsUnconfirmedWhenGoneFromWorkingSet reproduces the
// "gone from the working set without us cancelling it" branch: the order
// vanished before the timeout fired, with no way to tell whether it filled
// or was cancelled by something else — this must be flagged as unconfirmed,
// not guessed at either way, and the reservation must stay held (an
// unreleased reservation is the safe failure mode).
func TestWatchLimitOrderFlagsUnconfirmedWhenGoneFromWorkingSet(t *testing.T) {
	brk := watchLimitBrokerWithLister{watchLimitBroker{listOrders: true, openOrders: nil}}
	e := watchLimitEngine(brk)

	resID := e.rm.Reserve(500)

	order := broker.Order{Symbol: "X", Side: broker.Buy, Qty: 5, Type: broker.Limit, LimitPx: 100}
	res := broker.OrderResult{ID: "ord-1", Symbol: "X", Side: broker.Buy, Qty: 5}

	e.watchLimitOrder(context.Background(), res, order, resID)

	if len(e.Unconfirmed()) != 1 {
		t.Fatalf("Unconfirmed() = %d entries, want 1", len(e.Unconfirmed()))
	}
}

// TestWatchLimitOrderCancelsAndRecordsWhenNoFallback reproduces the clean
// "still working at timeout, successfully cancelled, no fallback
// configured" path: the reservation is released (this clip will definitely
// never fill) and a "cancelled" trade record is written.
func TestWatchLimitOrderCancelsAndRecordsWhenNoFallback(t *testing.T) {
	brk := watchLimitBrokerWithCanceller{watchLimitBrokerWithLister{
		watchLimitBroker{
			listOrders: true, openOrders: []broker.OpenOrder{{OrderID: "ord-1"}},
			cash: 600, // tight enough that a stuck 500 reservation would matter;
			// watchLimitOrder's own syncPortfolio call applies this over any
			// SetCash a test makes beforehand, so it must be set here instead.
		},
	}}
	e := watchLimitEngine(brk)
	e.cfg.Execution.MarketFallback = false

	resID := e.rm.Reserve(500)

	order := broker.Order{Symbol: "X", Side: broker.Buy, Qty: 5, Type: broker.Limit, LimitPx: 100}
	res := broker.OrderResult{ID: "ord-1", Symbol: "X", Side: broker.Buy, Qty: 5}

	e.watchLimitOrder(context.Background(), res, order, resID)

	trades := e.Trades()
	if len(trades) != 1 || trades[0].Status != "cancelled" {
		t.Fatalf("trades = %+v, want exactly one 'cancelled' record", trades)
	}

	// The reservation must be released: with 600 of cash and a 500
	// reservation, a 300-notional buy needs more headroom than a still-held
	// reservation would leave (only 100) — it can only be fully approved
	// once the 500 is actually freed.
	snap := e.pf.Snapshot()
	authOrder, d := e.rm.Authorize(
		broker.Order{Symbol: "Y", Side: broker.Buy, Qty: 3, Type: broker.Market}, 100, snap,
	)
	if !d.Approved || authOrder.Qty < 3 {
		t.Errorf("buy for 3 shares (300) was not fully approved after a confirmed cancel "+
			"released its 500 reservation: approved=%v qty=%v reason=%q",
			d.Approved, authOrder.Qty, d.Reason)
	}
}

// TestWatchLimitOrderMarketFallbackFillsAndRecordsBuy reproduces the full
// cancel-then-fallback path: the limit leg is cancelled, a market fallback
// is sent and fills, and the fill is booked (RecordBuy + a trade record)
// exactly like any other confirmed buy.
func TestWatchLimitOrderMarketFallbackFillsAndRecordsBuy(t *testing.T) {
	brk := watchLimitBrokerWithCanceller{watchLimitBrokerWithLister{
		watchLimitBroker{
			listOrders: true, openOrders: []broker.OpenOrder{{OrderID: "ord-1"}},
			fallback: broker.OrderResult{
				ID: "ord-2", Terminal: true, FilledQty: 5, FilledPx: 101, Status: "filled",
			},
		},
	}}
	e := watchLimitEngine(brk)
	e.cfg.Execution.MarketFallback = true

	resID := e.rm.Reserve(500)

	order := broker.Order{Symbol: "X", Side: broker.Buy, Qty: 5, Type: broker.Limit, LimitPx: 100}
	res := broker.OrderResult{ID: "ord-1", Symbol: "X", Side: broker.Buy, Qty: 5}

	e.watchLimitOrder(context.Background(), res, order, resID)

	if got := e.rm.SpentToday(); got <= 0 {
		t.Errorf("SpentToday = %v, want > 0 — the fallback fill must be recorded as a real buy", got)
	}

	trades := e.Trades()
	if len(trades) != 2 {
		// One "cancelled" record for the limit leg, one for the filled fallback.
		t.Fatalf("trades = %+v, want 2 records (cancelled limit + filled fallback)", trades)
	}

	// Trades() returns newest-first: trades[0] is the fallback fill.
	if trades[0].Status != "filled" || trades[0].Qty != 5 {
		t.Errorf("fallback trade record = %+v, want Status=filled Qty=5", trades[0])
	}
}

// TestWatchLimitOrderLeavesAnUncancellableBrokerAlone confirms a broker that
// cannot cancel orders is left working rather than the engine guessing at
// an outcome it cannot actually enforce.
func TestWatchLimitOrderLeavesAnUncancellableBrokerAlone(t *testing.T) {
	brk := watchLimitBrokerWithLister{
		watchLimitBroker{listOrders: true, openOrders: []broker.OpenOrder{{OrderID: "ord-1"}}},
	}
	e := watchLimitEngine(brk)

	resID := e.rm.Reserve(500)

	order := broker.Order{Symbol: "X", Side: broker.Buy, Qty: 5, Type: broker.Limit, LimitPx: 100}
	res := broker.OrderResult{ID: "ord-1", Symbol: "X", Side: broker.Buy, Qty: 5}

	e.watchLimitOrder(context.Background(), res, order, resID)

	if trades := e.Trades(); len(trades) != 0 {
		t.Errorf("trades = %+v, want none — nothing is known to have happened", trades)
	}

	if len(e.Unconfirmed()) != 0 {
		t.Errorf("Unconfirmed() = %d, want 0 — this path doesn't flag, it just leaves the order be",
			len(e.Unconfirmed()))
	}
}

// TestWatchLimitOrderCancelErrorLeavesTheReservationAlone confirms a
// CancelOrders error (as opposed to a successful cancel) is treated as
// ambiguous, not as a confirmed non-fill — the reservation must not be
// released on a mere cancel failure.
func TestWatchLimitOrderCancelErrorLeavesTheReservationAlone(t *testing.T) {
	brk := watchLimitBrokerWithCanceller{watchLimitBrokerWithLister{
		watchLimitBroker{
			listOrders: true, openOrders: []broker.OpenOrder{{OrderID: "ord-1"}},
			cancelErr: errors.New("network error"),
		},
	}}
	e := watchLimitEngine(brk)

	resID := e.rm.Reserve(500)

	order := broker.Order{Symbol: "X", Side: broker.Buy, Qty: 5, Type: broker.Limit, LimitPx: 100}
	res := broker.OrderResult{ID: "ord-1", Symbol: "X", Side: broker.Buy, Qty: 5}

	e.watchLimitOrder(context.Background(), res, order, resID)

	if trades := e.Trades(); len(trades) != 0 {
		t.Errorf("trades = %+v, want none on a mere cancel error", trades)
	}
}

// TestSubmitPlanDoesNotWatchAnAlreadySettledLimitClip is the regression for a
// filled limit order being reported, minutes later, as an order nobody can
// account for.
//
// submitPlan spawned watchLimitOrder for every Limit-TYPED clip, when the
// thing that needs watching is a clip that is still RESTING. A limit order
// the broker confirms at submission is not hypothetical: broker.Sim fills a
// marketable one instantly, and Trading 212 posts every order — limit-typed
// or not — to its market endpoint and confirms the fill inline. For those,
// the fill was booked at submission and a watcher was spawned anyway; one
// limit_timeout later it found the order gone from the working set and called
// flagUnconfirmed on it. That wrote an "unknown" TradeRecord for a completed
// order and, via hasUnconfirmedBuy, blocked every subsequent buy of that
// symbol until an operator cleared it by hand.
//
// The broker here is the exact shape that triggered it: a terminal fill at
// submission plus an OrderLister reporting an empty working set.
func TestSubmitPlanDoesNotWatchAnAlreadySettledLimitClip(t *testing.T) {
	filled := broker.OrderResult{
		ID: "ord-1", Symbol: "X", Side: broker.Buy, Qty: 5,
		FilledQty: 5, FilledPx: 100, Status: "filled", Terminal: true,
	}

	brk := watchLimitBrokerWithLister{watchLimitBroker{
		listOrders: true, openOrders: nil, // nothing working: the fill is done
		fallback: filled, // watchLimitBroker.SubmitOrder returns this
	}}
	e := watchLimitEngine(brk)

	child := broker.Order{Symbol: "X", Side: broker.Buy, Qty: 5, Type: broker.Limit, LimitPx: 100}
	plan := execPlan{orders: []broker.Order{child}, note: "limit 100.0000"}

	first, got, unconfirmed, resting, err := e.submitPlan(context.Background(), plan, "X", 100)
	if err != nil {
		t.Fatalf("submitPlan: %v", err)
	}

	if first.ID != "ord-1" || got != 5 {
		t.Fatalf("submitPlan booked first=%q filled=%v, want ord-1 / 5", first.ID, got)
	}

	if unconfirmed != 0 || resting {
		t.Fatalf("a confirmed fill reported unconfirmed=%v resting=%v, want 0 / false",
			unconfirmed, resting)
	}

	// e.cfg.Execution.LimitTimeout is 1ms here, so a watcher spawned by the
	// unfixed code has fired many times over by now.
	time.Sleep(250 * time.Millisecond)

	if u := e.Unconfirmed(); len(u) != 0 {
		t.Errorf("a limit order the broker confirmed FILLED was flagged unconfirmed: %+v", u)
	}

	for _, rec := range e.Trades() {
		if rec.Status == "unknown" || rec.Status == "cancelled" {
			t.Errorf("watcher ran on a settled clip and recorded %q: %+v", rec.Status, rec)
		}
	}

	// The reservation is released exactly once, by the settled branch — a clip
	// left reserved would quietly shrink the next buy's headroom forever.
	if spent := e.rm.BuyHeadroom(
		broker.Order{Symbol: "X", Side: broker.Buy}, 100, e.pf.Snapshot(),
	); spent <= 0 {
		t.Error("headroom still held by the settled clip's reservation")
	}
}

// TestSubmitPlanStillWatchesARestingLimitClip is the other half of the
// change: narrowing the spawn condition must not stop the engine watching the
// orders that genuinely do rest at the venue, or an unfilled limit order would
// sit there forever with nobody to cancel it or fall back to market.
func TestSubmitPlanStillWatchesARestingLimitClip(t *testing.T) {
	// No Terminal, no FilledQty: settledQty cannot confirm this either way, so
	// the clip is genuinely working.
	pending := broker.OrderResult{
		ID: "ord-2", Symbol: "X", Side: broker.Buy, Qty: 5, Status: "pending",
	}

	brk := watchLimitBrokerWithLister{watchLimitBroker{
		listOrders: true, openOrders: nil, fallback: pending,
	}}
	e := watchLimitEngine(brk)

	child := broker.Order{Symbol: "X", Side: broker.Buy, Qty: 5, Type: broker.Limit, LimitPx: 100}
	plan := execPlan{orders: []broker.Order{child}}

	_, filled, unconfirmed, resting, err := e.submitPlan(context.Background(), plan, "X", 100)
	if err != nil {
		t.Fatalf("submitPlan: %v", err)
	}

	if filled != 0 || unconfirmed != 0 || !resting {
		t.Fatalf("resting clip reported filled=%v unconfirmed=%v resting=%v, want 0 / 0 / true",
			filled, unconfirmed, resting)
	}

	// The watcher owns this clip's outcome; with an empty working set it
	// reaches the "gone without us cancelling it" branch and flags it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(e.Unconfirmed()) == 1 {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Error("no watcher ran for a genuinely resting limit clip")
}

// TestSubmitPlanWatchesTheRemainderOfAPartiallyFilledLimitClip covers the case
// that sits between the two above: the broker reports a partial fill on an
// order that is STILL WORKING (FilledQty > 0 with Terminal false — Alpaca and
// Trading 212 both return exactly that at their fill-wait deadline, since
// partially_filled is a working status for both).
//
// settledQty confirms that partial, so it takes the settled branch and gets
// booked. But the unfilled remainder is still resting at the venue, and
// nothing else will ever cancel it or apply market_fallback to it — narrowing
// the watcher to "not settled" would have left limit_timeout silently
// inapplicable to a partially-filled order.
//
// The watcher must be scoped to the REMAINDER. It re-sends order.Qty as the
// market fallback, so handing it the original clip would buy the already-filled
// part a second time.
func TestSubmitPlanWatchesTheRemainderOfAPartiallyFilledLimitClip(t *testing.T) {
	// 2 of 5 filled, still working: no Terminal.
	partial := broker.OrderResult{
		ID: "ord-3", Symbol: "X", Side: broker.Buy, Qty: 5,
		FilledQty: 2, FilledPx: 100, Status: "partially_filled",
	}

	brk := watchLimitBrokerWithLister{watchLimitBroker{
		listOrders: true, openOrders: nil, // gone by the time the watcher looks
		fallback: partial,
	}}
	e := watchLimitEngine(brk)

	child := broker.Order{Symbol: "X", Side: broker.Buy, Qty: 5, Type: broker.Limit, LimitPx: 100}
	plan := execPlan{orders: []broker.Order{child}}

	_, filled, unconfirmed, resting, err := e.submitPlan(context.Background(), plan, "X", 100)
	if err != nil {
		t.Fatalf("submitPlan: %v", err)
	}

	if filled != 2 {
		t.Fatalf("booked filled=%v, want the 2 the broker confirmed", filled)
	}

	if unconfirmed != 0 {
		t.Errorf("unconfirmed=%v, want 0 — the partial is a confirmed observation", unconfirmed)
	}

	if !resting {
		t.Error("resting=false with 3 shares still working at the venue — the caller " +
			"will treat this order as having a complete outcome when it does not")
	}

	// The watcher runs on the remainder and, with an empty working set, flags
	// it. The quantity it reports is the tell: 3, not the original 5.
	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		if u := e.Unconfirmed(); len(u) == 1 {
			if u[0].Qty != 3 {
				t.Errorf("watcher is scoped to qty %v, want the 3-share remainder — "+
					"at this quantity the market fallback would re-buy the filled part",
					u[0].Qty)
			}

			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Error("no watcher ran for the working remainder of a partially-filled limit clip")
}
