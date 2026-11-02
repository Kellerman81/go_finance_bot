package engine

import (
	"context"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
)

// EN3-3: a MARKET clip the broker reports partly filled and still working at
// its fill-wait deadline (Alpaca and Trading 212 return FilledQty > 0 with
// Terminal false) booked the partial and forgot the rest: no watcher, no
// unconfirmed order, the reservation released — so when the remainder filled,
// it never counted against max_daily_spend.
func TestAMarketBuysWorkingRemainderIsTracked(t *testing.T) {
	partial := broker.OrderResult{
		ID: "m-1", Symbol: "X", Side: broker.Buy, Qty: 5,
		FilledQty: 2, FilledPx: 100, Status: "partially_filled",
	}
	e := watchLimitEngine(watchLimitBroker{fallback: partial})

	child := broker.Order{Symbol: "X", Side: broker.Buy, Qty: 5, Type: broker.Market}

	_, filled, unconfirmed, _, err := e.submitPlan(context.Background(), execPlan{orders: []broker.Order{child}}, "X", 100)
	if err != nil {
		t.Fatal(err)
	}

	if filled != 2 || unconfirmed != 3 {
		t.Errorf("filled %v / unconfirmed %v, want 2 / 3", filled, unconfirmed)
	}

	if !e.rm.HasReservation("X") {
		t.Error("the working remainder holds no reservation; its fill would never count against the budget")
	}
}

// partialSellBroker fills half of every sell at its wait deadline and leaves
// the rest working, and reports the holding the test sets.
type partialSellBroker struct {
	asyncBroker

	held   float64
	orders []float64
}

func (b *partialSellBroker) SubmitOrder(_ context.Context, o broker.Order) (broker.OrderResult, error) {
	b.orders = append(b.orders, o.Qty)

	return broker.OrderResult{
		ID: "s", Symbol: o.Symbol, Side: o.Side, Qty: o.Qty,
		FilledQty: o.Qty / 2, FilledPx: 120, Status: "partially_filled",
	}, nil
}

func (b *partialSellBroker) GetPositions(context.Context) ([]broker.Position, error) {
	return []broker.Position{{Symbol: "X", Qty: b.held, AvgPrice: 100, Current: 120}}, nil
}

// EN3-3, the sell side: the partial fill dropped the holding below the
// pending-sell marker, so the next evaluation sold the still-working remainder
// again — an oversell, or a refusal recorded every 30 s.
func TestAMarketSellsWorkingRemainderIsNotSoldAgain(t *testing.T) {
	e := ledgerEngine(t) // 2 of X
	b := &partialSellBroker{held: 2}
	e.brk = b

	e.exitPosition(context.Background(), "X", 2, 120, "stop-loss", true, false, e.pf.Snapshot())

	b.held = 1 // the filled half is gone; the other half still works
	e.syncPortfolio(context.Background())

	e.exitPosition(context.Background(), "X", 1, 120, "stop-loss", true, false, e.pf.Snapshot())

	if len(b.orders) != 1 {
		t.Errorf("orders sent %v: the working remainder was sold a second time", b.orders)
	}

	if len(e.Unconfirmed()) == 0 {
		t.Error("the working remainder was not reported as unconfirmed")
	}
}
