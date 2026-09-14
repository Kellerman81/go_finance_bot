package engine

import (
	"context"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
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
