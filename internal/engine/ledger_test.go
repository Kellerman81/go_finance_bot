package engine

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// asyncBroker models a real venue: it accepts the order but has not confirmed
// the fill by the time SubmitOrder returns, so FilledQty is 0. Alpaca, Saxo and
// Trading 212 all do this when their fill-wait window expires.
type asyncBroker struct{ broker.Broker }

func (asyncBroker) Name() string { return "async" }

func (asyncBroker) SubmitOrder(_ context.Context, o broker.Order) (broker.OrderResult, error) {
	return broker.OrderResult{
		ID: "1", Symbol: o.Symbol, Side: o.Side, Qty: o.Qty,
		Status: "submitted", SubmittedAt: time.Now(),
		// FilledQty and FilledPx deliberately zero: not confirmed yet.
	}, nil
}

func (asyncBroker) GetAccount(context.Context) (broker.Account, error) {
	return broker.Account{Cash: 1000}, nil
}

func (asyncBroker) GetPositions(context.Context) ([]broker.Position, error) { return nil, nil }

// ledgerEngine builds an engine over the async broker with one open position.
func ledgerEngine(t *testing.T) *Engine {
	t.Helper()

	e, _ := forgetEngine()
	e.brk = asyncBroker{}
	e.rm = risk.New(config.Limits{
		MaxOrderValue: 1e9, MaxPerPosition: 1e9, MaxTotalInvested: 1e9,
		MaxDailySpend: 1e9, MaxOpenPositions: 10,
	}, config.CostsConfig{})
	e.pf = portfolio.New()
	e.prices = NewPriceCache()
	e.barsStore = newBarStore(maxBars, time.Minute)
	e.signals = make(map[string]strategy.Signal)
	e.spreads = make(map[string]float64)
	e.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	e.tradingEnabled = true

	e.pf.SetCash(1000)
	e.pf.SyncPositions([]portfolio.Holding{
		{Symbol: "X", Qty: 2, AvgPrice: 100, LastPrice: 120},
	})
	e.prices.Set("X", 120)

	e.openPos["X"] = &OpenPosition{
		Symbol: "X", Qty: 2, OrigQty: 2, EntryPrice: 100,
		EntryTime: time.Now().Add(-time.Hour),
	}

	return e
}

// A closing sale whose fill the broker never confirmed must NOT reach the
// win/loss ledger and must NOT close the position record — that would be
// booking a guess (see CODE_REVIEW_FINDINGS.md CRIT-1/3/4). Instead it must
// be flagged as unconfirmed so an operator reconciles it by hand, and the
// pendingSell dedupe guard must still be set so the same exit isn't
// resubmitted next cycle.
func TestClosingSaleWithoutAConfirmedFillIsFlaggedNotBooked(t *testing.T) {
	e := ledgerEngine(t)

	ok := e.exitPosition(context.Background(), "X", 2, 120, "test exit", true, false, e.pf.Snapshot())
	if ok {
		t.Error("exitPosition reported a real fill for an unconfirmed broker result")
	}

	trades := e.Trades()
	if len(trades) != 1 {
		t.Fatalf("recorded %d trades, want 1", len(trades))
	}

	tr := trades[0]

	if tr.Status != "unknown" {
		t.Errorf("Status = %q, want %q", tr.Status, "unknown")
	}

	if tr.Win != nil || tr.PnL != nil {
		t.Errorf("an unconfirmed sale must not book a win/loss verdict: %+v", tr)
	}

	if p, still := e.openPos["X"]; !still || p.Qty != 2 {
		t.Errorf("the position must survive an unconfirmed exit untouched, got %+v", p)
	}

	if len(e.Unconfirmed()) != 1 {
		t.Fatalf("want 1 unconfirmed order flagged, got %d", len(e.Unconfirmed()))
	}

	if _, ok := e.pendingSellQty("X"); !ok {
		t.Error("the pendingSell dedupe guard must be set even on an unconfirmed sell")
	}
}

// The same must hold for a sell placed by hand from the web interface. It runs
// through ManualOrder, which calls the broker directly rather than through
// submitPlan, so it needs the same handling in its own right.
func TestManualSaleWithoutAConfirmedFillIsFlaggedNotBooked(t *testing.T) {
	e := ledgerEngine(t)

	rec, err := e.ManualOrder(context.Background(), "X", broker.Sell, 2)
	if err != nil {
		t.Fatalf("ManualOrder: %v", err)
	}

	if rec.Status != "unknown" {
		t.Errorf("Status = %q, want %q", rec.Status, "unknown")
	}

	if rec.Win != nil || rec.PnL != nil {
		t.Errorf("an unconfirmed manual sale must not book a win/loss verdict: %+v", rec)
	}

	if p, still := e.openPos["X"]; !still || p.Qty != 2 {
		t.Errorf("the position must survive an unconfirmed manual exit untouched, got %+v", p)
	}
}

// A scale-in add whose fill the broker never confirmed must not grow the
// position — booking a guessed quantity leaves OrigQty and the average entry
// price measured against a base that might not exist, mis-sizing every
// scale-out rung for the rest of the position. It must be flagged instead.
func TestScaleInAddWithoutAConfirmedFillIsFlaggedNotBooked(t *testing.T) {
	qty, confirmed := settledQty(broker.OrderResult{Status: "submitted"})
	if confirmed || qty != 0 {
		t.Fatalf("settledQty(unconfirmed) = %v, %v — want 0, false", qty, confirmed)
	}

	// addToPosition (scalein.go) must gate its recordOpen call on settledQty
	// reporting confirmed, exactly like the buy path in act() and ManualOrder
	// — see TestNoOrderPathBooksARawFilledQty for the structural check that
	// no call site bypasses settledQty with a raw FilledQty read.
}

// Every quantity that reaches the books must come from settledQty, and only
// when it reports confirmed. Bypassing it with a raw res.FilledQty silently
// reintroduces the booking bugs settledQty exists to prevent.
func TestNoOrderPathBooksARawFilledQty(t *testing.T) {
	t.Parallel()

	// execute.go is excluded: it defines settledQty, which is where the one
	// legitimate read of a raw filled quantity lives.
	for _, file := range []string{"engine.go", "exits.go", "scalein.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}

		for i, line := range strings.Split(string(src), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}

			if strings.Contains(line, "res.FilledQty") || strings.Contains(line, "fallback.FilledQty") {
				t.Errorf("%s:%d books a raw filled quantity:\n\t%s\n"+
					"An async broker reports 0 filled when it stops waiting for "+
					"confirmation, or Terminal:false when it can't confirm the outcome "+
					"at all. Use settledQty(res) and only book when it reports "+
					"confirmed.", file, i+1, strings.TrimSpace(line))
			}
		}
	}
}
