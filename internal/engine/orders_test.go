package engine

import (
	"context"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
)

// modifyingBroker accepts any modification unconditionally and records what
// it received, so a test can tell whether the broker was ever reached.
type modifyingBroker struct{ broker.Broker }

func (modifyingBroker) Name() string { return "modifying" }

func (modifyingBroker) GetAccount(context.Context) (broker.Account, error) {
	return broker.Account{Cash: 100000}, nil
}

func (modifyingBroker) GetPositions(context.Context) ([]broker.Position, error) { return nil, nil }

func (b *recordingModifier) ModifyOrder(_ context.Context, m broker.OrderModification) (broker.OrderResult, error) {
	b.got = &m

	return broker.OrderResult{ID: m.OrderID, Symbol: m.Symbol, Side: m.Side, Qty: m.Qty, FilledQty: 0, Terminal: false}, nil
}

type recordingModifier struct {
	modifyingBroker

	got *broker.OrderModification
}

// TestModifyOrderIsAuthorized reproduces the PATCH-skips-authorization
// finding: ModifyOrder used to call the broker directly with no money-limit
// check at all — the one order-placement path where rm.Authorize never ran.
// A modification requesting far more than max_order_value allows must be
// blocked before it ever reaches the broker.
func TestModifyOrderIsAuthorized(t *testing.T) {
	e, _ := forgetEngine()

	b := &recordingModifier{}
	e.brk = b
	// A cash_reserve above available cash leaves zero headroom for ANY
	// quantity, unlike max_order_value alone (which Authorize clamps down
	// to fit rather than rejecting outright) — this exercises the genuine
	// "blocked, no headroom" path.
	e.rm = risk.New(config.Limits{
		MaxOrderValue: 1e9, MaxPerPosition: 1e9, MaxTotalInvested: 1e9,
		MaxDailySpend: 1e9, MaxOpenPositions: 10, CashReserve: 200000,
	}, config.CostsConfig{})
	e.pf = portfolio.New()
	e.pf.SetCash(100000) // less than CashReserve => no headroom at all
	e.prices = NewPriceCache()
	e.prices.Set("AAPL", 100)

	_, err := e.ModifyOrder(context.Background(), broker.OrderModification{
		OrderID: "1", Symbol: "AAPL", Side: broker.Buy, Type: broker.Limit,
		Qty: 10, Price: 100,
	})
	if err == nil {
		t.Fatal("want an error: cash_reserve leaves no headroom for any quantity")
	}

	if b.got != nil {
		t.Errorf("broker.ModifyOrder was called (%+v) — the block must happen before reaching the broker", b.got)
	}
}

// TestModifyOrderClampsToAuthorizedQty checks a modification that's within
// budget but whose exact quantity needs clamping is sent to the broker at
// the clamped amount, not the originally requested one — mirroring how
// PlaceBracket already clamps b.Qty to authOrder.Qty.
func TestModifyOrderClampsToAuthorizedQty(t *testing.T) {
	e, _ := forgetEngine()

	b := &recordingModifier{}
	e.brk = b
	e.rm = risk.New(config.Limits{
		MaxOrderValue: 1000, MaxPerPosition: 1e9, MaxTotalInvested: 1e9,
		MaxDailySpend: 1e9, MaxOpenPositions: 10,
	}, config.CostsConfig{})
	e.pf = portfolio.New()
	e.pf.SetCash(100000)
	e.prices = NewPriceCache()
	e.prices.Set("AAPL", 100)

	// 20 shares @ $100 = $2,000 requested, but max_order_value=1000 caps it
	// at 10 shares.
	res, err := e.ModifyOrder(context.Background(), broker.OrderModification{
		OrderID: "1", Symbol: "AAPL", Side: broker.Buy, Type: broker.Limit,
		Qty: 20, Price: 100,
	})
	if err != nil {
		t.Fatalf("ModifyOrder: %v", err)
	}

	if res.Qty != 10 {
		t.Errorf("result Qty = %v, want 10 (clamped)", res.Qty)
	}

	if b.got == nil {
		t.Fatal("broker.ModifyOrder was never called")
	}

	if b.got.Qty != 10 {
		t.Errorf("broker received Qty = %v, want 10 (clamped, not the requested 20)", b.got.Qty)
	}
}
