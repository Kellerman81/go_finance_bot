package engine

import (
	"context"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
)

// rejectingBroker confirms every sell as a definite, terminal rejection —
// Status/FilledQty are final facts, not "haven't heard back yet". Trading212,
// Saxo and Alpaca all report a real rejection this way (Terminal: true,
// FilledQty: 0) when e.g. the venue refuses an order outright.
type rejectingBroker struct{ broker.Broker }

func (rejectingBroker) Name() string { return "rejecting" }

func (rejectingBroker) SubmitOrder(_ context.Context, o broker.Order) (broker.OrderResult, error) {
	return broker.OrderResult{
		ID: "1", Symbol: o.Symbol, Side: o.Side, Qty: o.Qty,
		Status: "rejected", SubmittedAt: time.Now(),
		Terminal: true, // confirmed: definitely nothing filled
	}, nil
}

func (rejectingBroker) GetAccount(context.Context) (broker.Account, error) {
	return broker.Account{Cash: 1000}, nil
}

func (rejectingBroker) GetPositions(context.Context) ([]broker.Position, error) { return nil, nil }

// TestExitPositionClearsPendingSellOnConfirmedRejection reproduces the
// pendingSell-stuck-forever finding: on a broker that doesn't implement
// OrderLister (Sim, Alpaca), reconcile can never clear a stale pendingSell
// marker. A confirmed rejection (as opposed to a genuinely unconfirmed
// result) is definitive — the position is unchanged — so the dedupe guard
// must be cleared immediately rather than left to block every future exit
// attempt for this symbol forever.
func TestExitPositionClearsPendingSellOnConfirmedRejection(t *testing.T) {
	e := ledgerEngine(t)
	e.brk = rejectingBroker{}

	ok := e.exitPosition(context.Background(), "X", 2, 120, "test exit", true, false, e.pf.Snapshot())
	if ok {
		t.Error("exitPosition reported a real fill for a confirmed rejection")
	}

	if _, stillPending := e.pendingSellQty("X"); stillPending {
		t.Error("pendingSell guard still set after a confirmed rejection — " +
			"this symbol can never be exited again on a broker without OrderLister")
	}

	// The position itself must be untouched: nothing was actually sold.
	if p, still := e.openPos["X"]; !still || p.Qty != 2 {
		t.Errorf("position must survive a rejected exit untouched, got %+v", p)
	}
}

// TestManualOrderClearsPendingSellOnConfirmedRejection is the same case
// through the manual-order path.
func TestManualOrderClearsPendingSellOnConfirmedRejection(t *testing.T) {
	e := ledgerEngine(t)
	e.brk = rejectingBroker{}

	rec, err := e.ManualOrder(context.Background(), "X", broker.Sell, 2)
	if err != nil {
		t.Fatalf("ManualOrder: %v", err)
	}

	if rec.Win != nil || rec.PnL != nil {
		t.Errorf("a rejected manual sale must not book a win/loss verdict: %+v", rec)
	}

	if _, stillPending := e.pendingSellQty("X"); stillPending {
		t.Error("pendingSell guard still set after a confirmed rejection — " +
			"this symbol can never be exited again on a broker without OrderLister")
	}
}

// fillingBroker fills every order in full, at once, and keeps a position
// ledger so syncPortfolio sees what a real venue would report afterwards.
type fillingBroker struct {
	broker.Broker

	held   float64
	orders int
}

func (*fillingBroker) Name() string { return "filling" }

func (b *fillingBroker) SubmitOrder(_ context.Context, o broker.Order) (broker.OrderResult, error) {
	b.orders++

	if o.Side == broker.Sell {
		b.held -= o.Qty
	} else {
		b.held += o.Qty
	}

	return broker.OrderResult{
		ID: "f", Symbol: o.Symbol, Side: o.Side, Qty: o.Qty,
		FilledQty: o.Qty, FilledPx: 120, Status: "filled", Terminal: true,
	}, nil
}

func (*fillingBroker) GetAccount(context.Context) (broker.Account, error) {
	return broker.Account{Cash: 1000}, nil
}

func (b *fillingBroker) GetPositions(context.Context) ([]broker.Position, error) {
	if b.held <= 0 {
		return nil, nil
	}

	return []broker.Position{{Symbol: "X", Qty: b.held, AvgPrice: 100, Current: 120}}, nil
}

// TestPartialManualSaleDoesNotBlockTheStopLoss is the regression for EN2-3:
// a manual sale of PART of a position disabled every later exit of it.
//
// ManualOrder planted the pendingSell marker with the quantity SOLD (3), where
// every other site stores the quantity HELD at submit. The guards read
// "held >= marker" as "the sell has not landed yet", so after selling 3 of 10
// the remaining 7 >= 3 held true forever, and a confirmed fill never cleared
// it. On a broker without an OrderLister (Sim, Alpaca) nothing else ever does:
// the stop-loss sent zero orders until a restart — invariant 1 broken.
func TestPartialManualSaleDoesNotBlockTheStopLoss(t *testing.T) {
	e := ledgerEngine(t)
	b := &fillingBroker{held: 10}
	e.brk = b
	e.openPos["X"].Qty, e.openPos["X"].OrigQty = 10, 10
	e.syncPortfolio(context.Background())

	if _, err := e.ManualOrder(context.Background(), "X", broker.Sell, 3); err != nil {
		t.Fatalf("ManualOrder: %v", err)
	}

	if q, pending := e.pendingSellQty("X"); pending {
		t.Errorf("pendingSell = %v still set after a CONFIRMED manual fill", q)
	}

	h, _ := e.pf.Snapshot().Holding("X")
	before := b.orders

	if !e.exitPosition(context.Background(), "X", h.Qty, 120, "stop-loss", true, false, e.pf.Snapshot()) {
		t.Fatal("stop-loss on the remaining 7 shares did not fire after a partial manual sale")
	}

	if sent := b.orders - before; sent != 1 {
		t.Errorf("stop-loss sent %d orders, want 1", sent)
	}
}

// TestUnconfirmedManualSaleMarksTheHeldQuantity covers the other half of
// EN2-3: while a manual sale is genuinely unconfirmed the marker must stay,
// and it must hold the quantity HELD at submit — so the guard lifts as soon
// as the broker reports the holding shrank, as it does on every other path.
func TestUnconfirmedManualSaleMarksTheHeldQuantity(t *testing.T) {
	e := ledgerEngine(t) // asyncBroker: accepts, never confirms
	e.pf.SyncPositions([]portfolio.Holding{{Symbol: "X", Qty: 10, AvgPrice: 100, LastPrice: 120}})
	e.openPos["X"].Qty, e.openPos["X"].OrigQty = 10, 10

	if _, err := e.ManualOrder(context.Background(), "X", broker.Sell, 3); err != nil {
		t.Fatalf("ManualOrder: %v", err)
	}

	q, pending := e.pendingSellQty("X")
	if !pending || q != 10 {
		t.Errorf("pendingSell = %v (set=%v), want the 10 held at submit, not the 3 sold", q, pending)
	}
}
