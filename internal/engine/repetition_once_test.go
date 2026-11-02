package engine

import (
	"context"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// blockedFor counts the blocked or dry-run records naming symbol.
func blockedFor(e *Engine, symbol, status string) int {
	n := 0

	for _, tr := range e.Trades() {
		if tr.Symbol == symbol && tr.Status == status {
			n++
		}
	}

	return n
}

// RK3-3: sizing knows nothing of max_open_positions, so a full book is refused
// later, by Authorize — and that refusal was recorded (and notified) on every
// evaluation: with ten symbols signalling BUY, ~1,200 records an hour. A
// standing condition is announced once.
func TestAFullBookRefusesAStandingBuyOnce(t *testing.T) {
	e := ledgerEngine(t) // holds X
	e.rm = risk.New(config.Limits{
		MaxOrderValue: 1e9, MaxPerPosition: 1e9, MaxTotalInvested: 1e9,
		MaxDailySpend: 1e9, MaxOpenPositions: 1,
	}, config.CostsConfig{})
	e.prices.Set("Y", 50)

	for range 3 {
		e.act(context.Background(), strategy.Signal{Symbol: "Y", Action: strategy.Buy, Price: 50}, nil, true)
	}

	if n := blockedFor(e, "Y", "blocked"); n != 1 {
		t.Errorf("a full book refused a standing BUY with %d blocked records over 3 evaluations, want 1", n)
	}
}

// EX3-3: with trading off (or a halt) and bypass_trading_halt false, a
// position through its stop recorded a dry run on every checkExits — 2,880
// rows per symbol per day for as long as the halt lasted.
func TestAHeldBreachedExitIsRecordedOnce(t *testing.T) {
	e := ledgerEngine(t)
	e.brk = &heldAsyncBroker{held: 2}
	e.tradingEnabled = false
	e.exits = config.ExitsConfig{Enabled: true, StopLossPct: 0.05}
	e.prices.Set("X", 90)

	for range 5 {
		e.checkExits(context.Background())
	}

	if n := blockedFor(e, "X", "dry-run"); n != 1 {
		t.Errorf("5 evaluations of one breached stop with trading off recorded %d dry runs, want 1", n)
	}
}

// addRejectingBroker confirms every order as rejected, counting them.
type addRejectingBroker struct{ confirmingBroker }

func (b *addRejectingBroker) SubmitOrder(_ context.Context, o broker.Order) (broker.OrderResult, error) {
	b.buys++

	return broker.OrderResult{ID: "r", Symbol: o.Symbol, Side: o.Side, Qty: o.Qty, Status: "rejected", Terminal: true}, nil
}

// EN3-7: a scale-in add the broker refused left Adds unchanged, so the rung
// stayed due and the add went out, was refused and was recorded every
// evaluation.
func TestARefusedScaleInIsNotResentEveryEvaluation(t *testing.T) {
	e := ledgerEngine(t)
	b := &addRejectingBroker{confirmingBroker{qty: map[string]float64{"X": 2}, cash: 1000}}
	e.brk = b
	e.cfg.ScaleIn = config.ScaleInConfig{Enabled: true, MaxAdds: 2, StepPct: 0.05, SizePct: 0.5}
	e.prices.Set("X", 90)

	for range 4 {
		e.checkScaleIn(context.Background())
	}

	if b.buys != 1 {
		t.Errorf("4 evaluations sent %d refused adds, want 1", b.buys)
	}

	if n := blockedFor(e, "X", "blocked"); n != 1 {
		t.Errorf("%d records of the refusal, want 1", n)
	}
}

// RK3-3, the guard's half: a full asset-class budget refused a standing BUY on
// every evaluation, and its reason carries figures that move with the price
// ("... 19.4% of equity ...") — so even an exact-reason dedupe announced it
// afresh each time. The same cap refusing the same symbol is one condition.
func TestAFullClassBudgetRefusesAStandingBuyOnce(t *testing.T) {
	e := ledgerEngine(t) // holds 2 X at 120
	e.SetPortfolioGuard(risk.NewPortfolioGuard(
		config.PortfolioConfig{Enabled: true, MaxPctPerAssetClass: map[string]float64{"stock": 0.10}},
		nil, map[string]string{"X": "stock", "Y": "stock"}, nil))

	for i, px := range []float64{50, 51, 49.5} {
		e.prices.Set("Y", px)
		e.pf.MarkPrice("X", 120+float64(i)*7) // the class's share of equity moves
		e.act(context.Background(), strategy.Signal{Symbol: "Y", Action: strategy.Buy, Price: px}, nil, true)
	}

	if n := blockedFor(e, "Y", "blocked"); n != 1 {
		t.Errorf("a full class budget refused a standing BUY with %d records over 3 evaluations, want 1", n)
	}
}
