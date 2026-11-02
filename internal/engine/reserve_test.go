package engine

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// restingLimitBroker accepts a limit buy without confirming a fill — modeling
// a real venue's resting order, exactly like asyncBroker but keyed off order
// Type so a test can submit both limit and market clips through one broker.
type restingLimitBroker struct{ broker.Broker }

func (restingLimitBroker) Name() string { return "resting" }

func (restingLimitBroker) SubmitOrder(_ context.Context, o broker.Order) (broker.OrderResult, error) {
	if o.Type == broker.Limit {
		return broker.OrderResult{
			ID: "resting-1", Symbol: o.Symbol, Side: o.Side, Qty: o.Qty,
			Status: "working", SubmittedAt: time.Now(),
			// FilledQty/Terminal deliberately zero/false: genuinely resting.
		}, nil
	}

	return broker.OrderResult{
		ID: "market-1", Symbol: o.Symbol, Side: o.Side, Qty: o.Qty,
		FilledQty: o.Qty, FilledPx: 100, Terminal: true, Status: "filled",
	}, nil
}

func (restingLimitBroker) GetAccount(context.Context) (broker.Account, error) {
	return broker.Account{Cash: 1000}, nil
}

func (restingLimitBroker) GetPositions(context.Context) ([]broker.Position, error) { return nil, nil }

// reserveEngine builds an engine over restingLimitBroker with tight cash so a
// held reservation is observable in a subsequent Authorize call.
func reserveEngine() *Engine {
	e, _ := forgetEngine()
	e.brk = restingLimitBroker{}
	e.rm = risk.New(config.Limits{
		MaxOrderValue: 1e9, MaxPerPosition: 1e9, MaxTotalInvested: 1e9,
		MaxDailySpend: 1e9, MaxOpenPositions: 10, CashReserve: 0,
	}, config.CostsConfig{})
	e.pf = portfolio.New()
	e.prices = NewPriceCache()
	e.barsStore = newBarStore(maxBars, time.Minute)
	e.signals = make(map[string]strategy.Signal)
	e.spreads = make(map[string]float64)
	e.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	e.tradingEnabled = true

	e.pf.SetCash(1000)

	return e
}

// TestARestingLimitBuyReservesCashUntilItsFateIsKnown reproduces the "no
// cash reservation between Authorize and RecordBuy" finding at the engine
// integration level: a limit buy that goes resting (the common case — a
// real venue does not confirm a limit fill synchronously) is never booked
// via RecordBuy until watchLimitOrder eventually resolves it, which can
// take up to the configured limit timeout. Before submitPlan reserved
// anything for a resting clip, a second buy evaluated in that window saw
// the exact same 1000 of cash the first clip is still trying to spend.
func TestARestingLimitBuyReservesCashUntilItsFateIsKnown(t *testing.T) {
	e := reserveEngine()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // let watchLimitOrder's goroutine exit via ctx.Done rather than linger

	plan := execPlan{orders: []broker.Order{
		{Symbol: "X", Side: broker.Buy, Qty: 10, Type: broker.Limit, LimitPx: 100}, // 1000 notional
	}}

	_, filled, unconfirmed, resting, err := e.submitPlan(ctx, plan, "X", 100)
	if err != nil {
		t.Fatalf("submitPlan: %v", err)
	}

	if !resting || filled != 0 || unconfirmed != 0 {
		t.Fatalf("submitPlan(filled=%v, unconfirmed=%v, resting=%v), want a purely resting clip",
			filled, unconfirmed, resting)
	}

	// A second buy for the whole remaining cash must be blocked: the first
	// clip's estimated 1000 already claims it, and RecordBuy for that clip
	// has not (and, being resting, will not for a while) fired.
	snap := e.pf.Snapshot()
	order := broker.Order{Symbol: "Y", Side: broker.Buy, Qty: 10, Type: broker.Market}

	_, d := e.rm.Authorize(order, 100, snap)
	if d.Approved {
		t.Fatalf("second buy against the same unspent cash should be blocked while the "+
			"resting clip's reservation stands, got approved: %+v", d)
	}
}

// TestASynchronouslyFilledLimitBuyDoesNotLeaveAStuckReservation confirms the
// counterpart: a limit clip the broker confirms immediately (Terminal) must
// not leave anything reserved behind for subsequent buys to trip over.
func TestASynchronouslyFilledLimitBuyDoesNotLeaveAStuckReservation(t *testing.T) {
	e := reserveEngine()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	plan := execPlan{orders: []broker.Order{
		{Symbol: "X", Side: broker.Buy, Qty: 10, Type: broker.Market}, // restingLimitBroker fills market clips synchronously
	}}

	_, filled, _, resting, err := e.submitPlan(ctx, plan, "X", 100)
	if err != nil {
		t.Fatalf("submitPlan: %v", err)
	}

	if resting || filled != 10 {
		t.Fatalf("submitPlan(filled=%v, resting=%v), want a synchronous fill of 10", filled, resting)
	}

	snap := e.pf.Snapshot() // cash unchanged in this harness; RecordBuy is the caller's job, not submitPlan's
	order := broker.Order{Symbol: "Y", Side: broker.Buy, Qty: 10, Type: broker.Market}

	_, d := e.rm.Authorize(order, 100, snap)
	if !d.Approved {
		t.Fatalf("a synchronously filled clip must not leave cash reserved: %s", d.Reason)
	}
}

// TestScaleInAddRecordsSpendAgainstMaxDailySpend reproduces a bug found
// while auditing addToPosition for the cash-reservation finding, independent
// of it: a scale-in add never called RecordBuy at all, so it never counted
// toward max_daily_spend — a second add in the same day was authorised
// against a daily-spend figure that didn't reflect the first add's real
// cost.
func TestScaleInAddRecordsSpendAgainstMaxDailySpend(t *testing.T) {
	e := reserveEngine()
	e.rm = risk.New(config.Limits{
		// max_order_value is what sizes the add now: sizing starts at the
		// money-limit headroom, so the per-order ceiling IS the order size.
		MaxOrderValue: 1000, MaxPerPosition: 1e9, MaxTotalInvested: 1e9,
		MaxDailySpend: 1200, MaxOpenPositions: 10, CashReserve: 0,
	}, config.CostsConfig{})

	e.cfg.ScaleIn = config.ScaleInConfig{
		Enabled: true, MaxAdds: 5, StepPct: 0.05, SizePct: 1,
	}

	e.pf.SetCash(1e6) // cash itself is not the binding constraint; max_daily_spend is
	e.prices.Set("X", 100)

	e.openPos["X"] = &OpenPosition{
		Symbol: "X", Qty: 10, OrigQty: 10, EntryPrice: 200, // price fell from 200 to 100: due for a scale-in add
		EntryTime: time.Now().Add(-time.Hour),
	}

	if got := e.rm.SpentToday(); got != 0 {
		t.Fatalf("SpentToday before any add = %v, want 0", got)
	}

	e.checkScaleIn(context.Background())

	spent := e.rm.SpentToday()
	if spent <= 0 {
		t.Fatalf("SpentToday after a filled scale-in add = %v, want > 0 — the add's cash "+
			"outlay must be recorded like every other buy path's is", spent)
	}
}

// TestCheckScaleInRefreshesTheSnapshotBetweenAdds reproduces the "Scale-in
// authorizes every position add against one stale pre-loop snapshot"
// finding: two positions both due for an add in the same checkScaleIn pass
// must not both be authorised against the SAME max_total_invested headroom
// figure — the second one needs to see what the first one already
// committed, or together they can blow through the cap.
//
// AAA and BBB each start at 10 shares @ price 100 (1000 invested each,
// 2000 combined). MaxTotalInvested is set to exactly 2500: AAA's add (500
// notional) exactly exhausts the remaining 500 of headroom, so BBB's add —
// processed second, since OpenPositions sorts by symbol — must see zero
// headroom left and be blocked.
func TestCheckScaleInRefreshesTheSnapshotBetweenAdds(t *testing.T) {
	e := reserveEngine()
	e.rm = risk.New(config.Limits{
		// max_order_value sizes each add (500) now that sizing starts from
		// the headroom rather than a separate order_size_usd.
		MaxOrderValue: 500, MaxPerPosition: 1e9, MaxTotalInvested: 2500,
		MaxDailySpend: 1e9, MaxOpenPositions: 10, CashReserve: 0,
	}, config.CostsConfig{})

	e.cfg.ScaleIn = config.ScaleInConfig{
		Enabled: true, MaxAdds: 5, StepPct: 0.05, SizePct: 1,
	}

	e.pf.SetCash(1e6)
	e.prices.Set("AAA", 100)
	e.prices.Set("BBB", 100)

	e.openPos["AAA"] = &OpenPosition{
		Symbol: "AAA", Qty: 10, OrigQty: 10, EntryPrice: 200,
		EntryTime: time.Now().Add(-time.Hour),
	}
	e.openPos["BBB"] = &OpenPosition{
		Symbol: "BBB", Qty: 10, OrigQty: 10, EntryPrice: 200,
		EntryTime: time.Now().Add(-time.Hour),
	}

	e.checkScaleIn(context.Background())

	if got := e.openPos["AAA"].Qty; got != 15 {
		t.Errorf("AAA.Qty after its add = %v, want 15 (its add should be fully "+
			"authorised — it exactly fits the pre-existing headroom)", got)
	}

	if got := e.openPos["BBB"].Qty; got != 10 {
		t.Errorf("BBB.Qty after checkScaleIn = %v, want 10 (unchanged) — "+
			"AAA's add already spent the last of max_total_invested's headroom, "+
			"so BBB's add must be blocked rather than authorised against a stale "+
			"snapshot that doesn't know about AAA's add yet", got)
	}
}

// TestReconcilingAnUnconfirmedBuyReleasesItsReservation guards the
// reservation an unconfirmed buy holds. It stays reserved while the outcome is
// unknown (fail closed), but it was never released at all — and once
// reservations counted against max_total_invested (LR2 RK2-2), an unconfirmed
// buy that DID fill was counted twice, in the snapshot and as reserved, until
// a restart. Reconciling the order by hand must release it.
func TestReconcilingAnUnconfirmedBuyReleasesItsReservation(t *testing.T) {
	e := ledgerEngine(t) // asyncBroker: accepts, never confirms
	e.rm = risk.New(config.Limits{
		MaxOrderValue: 1e9, MaxPerPosition: 1e9, MaxTotalInvested: 1000,
		MaxDailySpend: 1e9, MaxOpenPositions: 10,
	}, config.CostsConfig{})
	e.pf.SetCash(100000)
	e.prices.Set("Y", 100)

	if _, err := e.ManualOrder(context.Background(), "Y", broker.Buy, 10); err != nil {
		t.Fatal(err)
	}

	u := e.Unconfirmed()
	if len(u) != 1 {
		t.Fatalf("unconfirmed = %+v, want the one buy", u)
	}

	probe := broker.Order{Symbol: "Z", Side: broker.Buy, Qty: 5}
	if _, d := e.rm.Authorize(probe, 100, e.pf.Snapshot()); d.Approved {
		t.Fatal("the unconfirmed buy's reservation did not hold headroom; the scenario did not set up")
	}

	e.ClearUnconfirmed(u[0].OrderID)

	if _, d := e.rm.Authorize(probe, 100, e.pf.Snapshot()); !d.Approved {
		t.Errorf("after reconciling the order by hand its reservation still blocks buys: %s", d.Reason)
	}
}
