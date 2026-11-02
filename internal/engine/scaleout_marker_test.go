package engine

import (
	"context"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// heldAsyncBroker never confirms an order (asyncBroker) but reports a holding
// the test moves by hand, as the venue would once an unknown order fills.
type heldAsyncBroker struct {
	asyncBroker

	held   float64
	orders []float64
}

func (b *heldAsyncBroker) SubmitOrder(ctx context.Context, o broker.Order) (broker.OrderResult, error) {
	b.orders = append(b.orders, o.Qty)

	return b.asyncBroker.SubmitOrder(ctx, o)
}

func (b *heldAsyncBroker) GetPositions(context.Context) ([]broker.Position, error) {
	if b.held <= 0 {
		return nil, nil
	}

	return []broker.Position{{Symbol: "X", Qty: b.held, AvgPrice: 100, Current: 120}}, nil
}

// EX3-1: an unconfirmed scale-out rung never advanced PartialsTaken. When the
// unknown order then filled, the holding dropped below the pending-sell
// marker, the guard opened, and the same rung fired again — sized against the
// original quantity, it sold the whole remainder. A 50% scale-out became a
// full exit.
func TestAnUnconfirmedRungIsNotSoldAgainOnceItFills(t *testing.T) {
	e := ledgerEngine(t) // 2 of X @100, price 120
	b := &heldAsyncBroker{held: 2}
	e.brk = b
	e.exits = config.ExitsConfig{
		Enabled: true, PartialTakeProfit: []config.PartialExit{{ProfitPct: 0.10, SellPct: 0.5}},
	}

	e.checkExits(context.Background()) // the rung sells 1; outcome unknown

	if len(b.orders) != 1 {
		t.Fatalf("first pass sent %v, want one tranche", b.orders)
	}

	b.held = 1 // the unknown order filled at the venue
	e.syncPortfolio(context.Background())
	e.prices.Set("X", 120)

	e.checkExits(context.Background())

	if len(b.orders) != 1 {
		t.Errorf("a one-rung 50%% ladder sent %v: the rung fired twice and closed the position", b.orders)
	}
}

// confirmingBroker confirms every market order in full and tracks holdings,
// with no OrderLister — like Alpaca and Sim, so reconcile never clears a
// pending-sell marker.
type confirmingBroker struct {
	qty   map[string]float64
	cash  float64
	buys  int
	sells int
}

func (*confirmingBroker) Name() string { return "confirming" }

func (b *confirmingBroker) SubmitOrder(_ context.Context, o broker.Order) (broker.OrderResult, error) {
	if o.Side == broker.Buy {
		b.buys++
		b.qty[o.Symbol] += o.Qty
	} else {
		b.sells++
		b.qty[o.Symbol] -= o.Qty
	}

	return broker.OrderResult{
		ID: "x", Symbol: o.Symbol, Side: o.Side, Qty: o.Qty,
		FilledQty: o.Qty, FilledPx: 100, Status: "filled", Terminal: true,
	}, nil
}

func (b *confirmingBroker) GetAccount(context.Context) (broker.Account, error) {
	return broker.Account{Cash: b.cash, Equity: b.cash}, nil
}

func (b *confirmingBroker) GetPositions(context.Context) ([]broker.Position, error) {
	var out []broker.Position

	for s, q := range b.qty {
		if q > 0 {
			out = append(out, broker.Position{Symbol: s, Qty: q, AvgPrice: 100, Current: 100})
		}
	}

	return out, nil
}

// EN3-2: a CONFIRMED scale-out rung leaves its pending-sell marker until
// reconcile clears it, which needs an order-listing broker. Scale-in skipped
// any symbol with a marker, so on Alpaca or Sim it never fired again for that
// position. A settled sale — holding below the marker — is not in flight.
func TestAConfirmedScaleOutDoesNotDisableScaleIn(t *testing.T) {
	e := ledgerEngine(t) // holds 2 of X @100
	b := &confirmingBroker{qty: map[string]float64{"X": 2}, cash: 1000}
	e.brk = b
	e.cfg.ScaleIn = config.ScaleInConfig{Enabled: true, MaxAdds: 2, StepPct: 0.05, SizePct: 0.5}

	if !e.exitPosition(context.Background(), "X", 1, 120, "partial take-profit rung 1", true, false, e.pf.Snapshot()) {
		t.Fatal("setup: the rung did not fill")
	}

	e.prices.Set("X", 90)

	for range 3 {
		e.checkScaleIn(context.Background())
	}

	if b.buys == 0 {
		t.Error("scale-in never fired after a confirmed scale-out rung: the stale pending-sell marker blocks it")
	}
}
