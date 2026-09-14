package engine

import (
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
)

// TestClosePnLIsNetOfTheRoundTrip reproduces the EN-1 finding: closePnL used to
// book (exit-entry)*qty gross, while internal/backtest books every round trip
// net of commission (RoundTrip.PnL, "net of round-trip commission"). Since
// engine.SymbolStats.ExpectancyPct is AVG(pnl_pct) over exactly these values,
// and internal/autotune compares that directly against a candidate's
// backtested ExpectancyPct/WinRatePct, the two sides of that gate were
// measuring different quantities and the live side was flattered on both.
func TestClosePnLIsNetOfTheRoundTrip(t *testing.T) {
	e, _ := forgetEngine()
	// data/config.example.yaml ships commission_pct: 0.001.
	e.rm = risk.New(config.Limits{}, config.CostsConfig{CommissionPct: 0.001})

	e.recordOpen("X", 10, 100, "")

	if got := e.openPos["X"].EntryCommission; got != 1.0 {
		t.Fatalf("EntryCommission = %v, want 1.00 (0.1%% of a 1000 buy)", got)
	}

	r, ok := e.closePnL("X", 101, 10)
	if !ok {
		t.Fatal("no pnl")
	}

	// gross 10.00, minus 1.00 entry + 1.01 exit commission.
	if want := 7.99; r.PnL < want-1e-9 || r.PnL > want+1e-9 {
		t.Errorf("PnL = %.4f, want %.2f (net of the round trip, not the 10.00 gross)", r.PnL, want)
	}

	if want := 0.799; r.PnLPct < want-1e-9 || r.PnLPct > want+1e-9 {
		t.Errorf("PnLPct = %.4f, want %.3f%% — the basis is the position cost, "+
			"matching backtest.RoundTrip.PnLPct", r.PnLPct, want)
	}
}

// A trade that is a win gross and a loss net must be booked as the loss it is,
// or WinRatePct is inflated relative to every backtested candidate.
func TestMarginalTradeIsALossOnceCommissionIsCounted(t *testing.T) {
	e, _ := forgetEngine()
	e.rm = risk.New(config.Limits{}, config.CostsConfig{CommissionPct: 0.001})

	e.recordOpen("Y", 10, 100, "")

	r, ok := e.closePnL("Y", 100.1, 10) // +1.00 gross, ~2.00 of commission
	if !ok {
		t.Fatal("no pnl")
	}

	if r.PnL >= 0 {
		t.Errorf("PnL = %.4f; a +1.00 gross move that cost ~2.00 to trade is a loss, "+
			"and winFromPnL would otherwise book it as a win", r.PnL)
	}
}

// A scale-out rung must carry only its share of the entry commission, and the
// remainder must stay with the position — otherwise the entry cost is charged
// twice across a laddered exit.
func TestEntryCommissionIsApportionedAcrossPartialExits(t *testing.T) {
	e, _ := forgetEngine()
	e.rm = risk.New(config.Limits{}, config.CostsConfig{CommissionPct: 0.001})

	e.recordOpen("Z", 10, 100, "") // EntryCommission 1.00

	first, ok := e.closePnL("Z", 100, 4) // sell 40%
	if !ok {
		t.Fatal("no pnl")
	}

	// 40% of the 1.00 entry fee, plus 0.001*400 exit.
	if want := -(0.4 + 0.4); first.PnL < want-1e-9 || first.PnL > want+1e-9 {
		t.Errorf("first rung PnL = %.4f, want %.2f (0.40 entry share + 0.40 exit)", first.PnL, want)
	}

	e.recordClose("Z", 4)

	if got := e.openPos["Z"].EntryCommission; got < 0.6-1e-9 || got > 0.6+1e-9 {
		t.Errorf("remaining EntryCommission = %.4f, want 0.60 — the sold share must be "+
			"drawn down so a later rung is not charged for it again", got)
	}

	second, _ := e.closePnL("Z", 100, 6) // the rest
	if want := -(0.6 + 0.6); second.PnL < want-1e-9 || second.PnL > want+1e-9 {
		t.Errorf("second rung PnL = %.4f, want %.2f", second.PnL, want)
	}
}

// A position restored from a row written before entry_commission existed has
// no recorded entry fee; it must book gross rather than inventing one.
func TestPositionWithoutRecordedEntryCommissionBooksGross(t *testing.T) {
	e, _ := forgetEngine()
	e.rm = risk.New(config.Limits{}, config.CostsConfig{CommissionPct: 0.001})

	e.openPos["OLD"] = &OpenPosition{Symbol: "OLD", Qty: 10, OrigQty: 10, EntryPrice: 100}

	r, ok := e.closePnL("OLD", 101, 10)
	if !ok {
		t.Fatal("no pnl")
	}

	// Only the exit side is known: 10.00 gross - 1.01 exit commission.
	if want := 8.99; r.PnL < want-1e-9 || r.PnL > want+1e-9 {
		t.Errorf("PnL = %.4f, want %.2f", r.PnL, want)
	}
}
