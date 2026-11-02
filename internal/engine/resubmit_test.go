package engine

import (
	"context"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// countingAsyncBroker accepts every order without confirming it, like
// asyncBroker, and counts what it was sent. Its position report stays at the
// ledgerEngine fixture's 2 shares of X — an unconfirmed order has not moved
// the venue's holding, which is exactly why the snapshot keeps showing it.
type countingAsyncBroker struct {
	asyncBroker

	orders int
}

func (b *countingAsyncBroker) SubmitOrder(ctx context.Context, o broker.Order) (broker.OrderResult, error) {
	b.orders++

	return b.asyncBroker.SubmitOrder(ctx, o)
}

func (*countingAsyncBroker) GetPositions(context.Context) ([]broker.Position, error) {
	return []broker.Position{{Symbol: "X", Qty: 2, AvgPrice: 100, Current: 120}}, nil
}

// TestUnconfirmedScaleInIsNotResent is the regression for EN2-1. A scale-in
// add the broker never confirmed is flagged, not booked, so Adds does not
// move and the rung stays due. checkScaleIn did not make the
// hasUnconfirmedBuy check act makes before every buy, so the same add went to
// the broker again every evaluation — three passes, three real buy orders,
// three halt-priority "unknown order" alerts.
func TestUnconfirmedScaleInIsNotResent(t *testing.T) {
	e := ledgerEngine(t)
	b := &countingAsyncBroker{}
	e.brk = b
	e.cfg.ScaleIn = config.ScaleInConfig{Enabled: true, MaxAdds: 2, StepPct: 0.05, SizePct: 0.5}
	e.prices.Set("X", 90) // 10% under the 100 entry: the first rung is due

	for range 3 {
		e.checkScaleIn(context.Background())
	}

	if b.orders != 1 {
		t.Errorf("3 evaluations sent %d scale-in buys while the first was unconfirmed, want 1", b.orders)
	}
}

// TestScaleInWaitsForAnInFlightSell pins the second half of EN2-1: recordOpen
// clears the pendingSell marker, so an add filling while a stop-exit is in
// flight would re-arm that exit for resubmission. No add while a sell is
// pending.
func TestScaleInWaitsForAnInFlightSell(t *testing.T) {
	e := ledgerEngine(t)
	b := &countingAsyncBroker{}
	e.brk = b
	e.cfg.ScaleIn = config.ScaleInConfig{Enabled: true, MaxAdds: 2, StepPct: 0.05, SizePct: 0.5}
	e.prices.Set("X", 90)
	e.markPendingSell("X", 2)

	e.checkScaleIn(context.Background())

	if b.orders != 0 {
		t.Errorf("scale-in sent %d buys while a sell of the position was still in flight", b.orders)
	}
}

// TestUnconfirmedScaleOutRungIsNotResent is the regression for EN2-2. The
// exit dedupe guard compared the quantity ABOUT TO BE SOLD with the marker,
// which records the holding: a scale-out tranche is always smaller, so the
// guard never stopped one. An unconfirmed rung was not marked taken and fired
// again every evaluation — 3 evaluations, 3 sells — where a full exit under
// the same conditions sent exactly 1.
func TestUnconfirmedScaleOutRungIsNotResent(t *testing.T) {
	e := ledgerEngine(t) // holds 2 of X
	b := &countingAsyncBroker{}
	e.brk = b

	for range 3 {
		e.exitPosition(context.Background(), "X", 1, 120, "scale-out rung 1", true, false, e.pf.Snapshot())
	}

	if b.orders != 1 {
		t.Errorf("3 evaluations of one unconfirmed rung sent %d sells, want 1", b.orders)
	}
}

// TestFullExitStillFiresAfterAConfirmedScaleOut guards the other direction of
// the EN2-2 fix: once a tranche has landed (the holding is now below the
// marker), the later full exit of the rest must go through.
func TestFullExitStillFiresAfterAConfirmedScaleOut(t *testing.T) {
	e := ledgerEngine(t)
	b := &countingAsyncBroker{}
	e.brk = b
	e.markPendingSell("X", 3) // a tranche of 1 sold out of 3; the snapshot holds 2

	if e.exitPosition(context.Background(), "X", 2, 120, "stop-loss", true, false, e.pf.Snapshot()); b.orders != 1 {
		t.Errorf("full exit after a landed tranche sent %d orders, want 1", b.orders)
	}
}

// TestSizingRefusalIsAnnouncedOnce is the regression for EN2-7. act cleared
// the announce-once state as soon as the venue-price gate passed — which it
// always does with venue_price_check off — wiping the stored sizing refusal
// right before sizeBuy refused again. A symbol that could not be sized was
// recorded, and notified, every evaluation: 4 evaluations, 4 identical
// blocked trades.
func TestSizingRefusalIsAnnouncedOnce(t *testing.T) {
	e := ledgerEngine(t)
	e.blockedOnce = make(map[string]string)
	e.cfg.MinOrderSizeUSD = 1e6 // nothing is sizeable
	e.prices.Set("Y", 50)

	sig := strategy.Signal{Symbol: "Y", Action: strategy.Buy, Price: 50}
	for range 4 {
		e.act(context.Background(), sig, nil, true)
	}

	n := 0

	for _, tr := range e.Trades() {
		if tr.Status == "blocked" {
			n++
		}
	}

	if n != 1 {
		t.Errorf("4 evaluations of the same sizing refusal recorded %d blocked trades, want 1", n)
	}
}

// TestScaleInRefusalIsAnnouncedOnce is the regression for EN2-8: a refused
// scale-in add is a standing condition while the price sits under the rung,
// but it was recorded — and notified — on every evaluation. The VP-1
// announce-once fix had been applied to act only.
func TestScaleInRefusalIsAnnouncedOnce(t *testing.T) {
	e := ledgerEngine(t)
	e.blockedOnce = make(map[string]string)
	e.cfg.ScaleIn = config.ScaleInConfig{Enabled: true, MaxAdds: 2, StepPct: 0.05, SizePct: 0.5}
	e.cfg.VenuePriceCheck = true // no history source: the venue price cannot be confirmed
	e.prices.Set("X", 90)

	for range 4 {
		e.checkScaleIn(context.Background())
	}

	if n := len(e.Trades()); n != 1 {
		t.Errorf("4 evaluations of one scale-in refusal recorded %d blocked trades, want 1", n)
	}
}
