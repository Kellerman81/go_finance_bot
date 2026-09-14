package api

import (
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/engine"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
)

// snapOf builds a broker snapshot from symbol→quantity.
func snapOf(held map[string]float64) portfolio.Snapshot {
	out := portfolio.Snapshot{Holdings: map[string]portfolio.Holding{}}
	for sym, qty := range held {
		out.Holdings[sym] = portfolio.Holding{Symbol: sym, Qty: qty}
	}

	return out
}

// trackedOf builds the bot's own records from symbol→quantity.
func trackedOf(pairs map[string]float64) []engine.OpenPosition {
	out := make([]engine.OpenPosition, 0, len(pairs))
	for sym, qty := range pairs {
		out = append(out, engine.OpenPosition{Symbol: sym, Qty: qty})
	}

	return out
}

// A broker that sells fractional shares rounds the filled quantity, so a
// position recorded as 3.0926 comes back as 3.0920. That is settlement, not a
// tracking failure, and flagging it every time teaches the operator to ignore
// the one panel where a genuinely stranded position would show up.
func TestReconcileIgnoresBrokerRounding(t *testing.T) {
	t.Parallel()

	rows := reconcile(
		snapOf(map[string]float64{"PRAJL.XC": 3.0920}),
		trackedOf(map[string]float64{"PRAJL.XC": 3.0926}),
	)

	if len(rows) != 0 {
		t.Errorf("a 0.02%% quantity difference was reported as drift: %+v", rows)
	}
}

// A position the bot tracks but the broker does not have is the dangerous case:
// exits stay armed and a slot stays occupied for stock that is gone.
func TestReconcileFlagsATrackedPositionTheBrokerLacks(t *testing.T) {
	t.Parallel()

	rows := reconcile(snapOf(nil), trackedOf(map[string]float64{"GONE": 3}))

	if len(rows) != 1 || rows[0].Symbol != "GONE" || rows[0].Broker != 0 {
		t.Fatalf("got %+v, want one row for GONE with broker 0", rows)
	}
}

// The mirror case matters just as much: a holding the bot has no record of is
// one it is not managing — no stop, no ladder, no time barrier.
func TestReconcileFlagsAHoldingTheBotDoesNotTrack(t *testing.T) {
	t.Parallel()

	rows := reconcile(snapOf(map[string]float64{"PR1JD": 3.09}), trackedOf(nil))

	if len(rows) != 1 || rows[0].Symbol != "PR1JD" || rows[0].Bot != 0 {
		t.Fatalf("got %+v, want one row for PR1JD with bot 0", rows)
	}
}

// A difference well past rounding is still drift.
func TestReconcileFlagsARealDifference(t *testing.T) {
	t.Parallel()

	rows := reconcile(
		snapOf(map[string]float64{"AAPL": 2}),
		trackedOf(map[string]float64{"AAPL": 3}),
	)

	if len(rows) != 1 {
		t.Fatalf("a 33%% difference was not reported: %+v", rows)
	}
}

// Agreement produces nothing.
func TestReconcileQuietWhenBothAgree(t *testing.T) {
	t.Parallel()

	rows := reconcile(
		snapOf(map[string]float64{"AAPL": 1.5, "MSFT": 2}),
		trackedOf(map[string]float64{"AAPL": 1.5, "MSFT": 2}),
	)

	if len(rows) != 0 {
		t.Errorf("matching books reported drift: %+v", rows)
	}
}

// The tolerance is relative, so it holds at any position size.
func TestDriftToleranceIsRelative(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		bot, brk float64
		wantFlag bool
	}{
		// 0.0031 vs 0.0030 is a 3.2% difference — past rounding, not an example
		// of it. Rounding at this size is the last digit or two.
		{"tiny position, rounding", 0.003100, 0.003098, false},
		{"tiny position, real gap", 0.0031, 0.0020, true},
		{"large position, rounding", 10000, 9999.5, false},
		{"large position, real gap", 10000, 9000, true},
	} {
		if got := disagrees(tc.bot, tc.brk); got != tc.wantFlag {
			t.Errorf("%s: disagrees(%v, %v) = %v, want %v",
				tc.name, tc.bot, tc.brk, got, tc.wantFlag)
		}
	}
}
