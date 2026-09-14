package engine

import (
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
)

// TestPendingExposureStopsOverlayingAStaleDrift reproduces the root cause of
// the "forgetting a drifted position trips a false max-drawdown halt"
// finding at the pendingExposure level: an openPos record the broker
// snapshot never confirms is overlaid onto equity indefinitely — with no
// distinction between a fill still settling (should overlay, briefly) and a
// position closed outside the bot (must not be counted as real equity at
// all, or peak/drawdown math gets corrupted). pendingFillGrace bounds this:
// fresh drift is still overlaid, drift older than the grace window is not.
func TestPendingExposureStopsOverlayingAStaleDrift(t *testing.T) {
	e, _ := forgetEngine()
	e.pf.SetCash(1000)
	e.pf.SyncPositions(nil) // broker holds nothing — OLD was closed outside the bot

	e.openPos["OLD"] = &OpenPosition{Symbol: "OLD", Qty: 5, EntryPrice: 20}

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	var fresh, stale portfolio.Snapshot

	atTime(t, base, func() {
		fresh = e.pendingExposure(e.pf.Snapshot())
	})

	atTime(t, base.Add(pendingFillGrace+time.Second), func() {
		stale = e.pendingExposure(e.pf.Snapshot())
	})

	if fresh.Invested != 100 {
		t.Fatalf("a fresh drift was not overlaid: Invested = %v, want 100 (5 * 20)", fresh.Invested)
	}

	if stale.Invested != 0 {
		t.Errorf("a drift older than pendingFillGrace was still overlaid: "+
			"Invested = %v, want 0", stale.Invested)
	}
}

// TestForgetOpenPositionDoesNotFalseTripMaxDrawdown is the full reproduction
// of the reported bug: an operator uses the Reconciliation panel's "forget"
// action on a position the bot's own record disagreed with the broker
// about. Before this fix, checkDrawdown's pendingExposure overlay had been
// silently counting that phantom position as real equity, letting it set a
// peak; forgetting the record then dropped equity back to its true value
// and read as a drawdown against that phantom-inflated peak — even though
// nothing was sold and no money was lost. A remove must never, by itself,
// trip a drawdown halt.
func TestForgetOpenPositionDoesNotFalseTripMaxDrawdown(t *testing.T) {
	e, _ := forgetEngine()
	e.SetProtectionsConfig(config.ProtectionsConfig{Enabled: true, MaxDrawdownPct: 0.15})

	e.pf.SetCash(1000)
	e.pf.SyncPositions(nil) // the broker's real holdings — DRIFTED is not among them

	// The bot's own record still shows a position the broker does not, worth
	// as much as the real cash — without a fix, forgetting it reads as a 33%
	// drawdown (1500 -> 1000), comfortably past the 15% limit below.
	e.openPos["DRIFTED"] = &OpenPosition{
		Symbol: "DRIFTED", Qty: 10, EntryPrice: 50, EntryTime: time.Now(),
	}

	e.checkDrawdown() // first observation: sets the (phantom-inflated) peak at 1500

	if halted, reason := e.Protections().Halted(); halted {
		t.Fatalf("halted on the very first observation: %s", reason)
	}

	if !e.ForgetOpenPosition("DRIFTED") {
		t.Fatal("ForgetOpenPosition reported no record for a symbol that has one")
	}

	e.checkDrawdown() // equity correctly drops now that the phantom is gone

	if halted, reason := e.Protections().Halted(); halted {
		t.Errorf("forgetting a drifted position tripped a false drawdown halt: %q", reason)
	}
}

// TestForgetOpenPositionStillAllowsARealDrawdownToTrip checks that the
// LowerPeak correction in ForgetOpenPosition only cancels the phantom
// amount it introduced, not more — a genuine loss that happens to coincide
// with a forget must still trip normally.
func TestForgetOpenPositionStillAllowsARealDrawdownToTrip(t *testing.T) {
	e, _ := forgetEngine()
	e.SetProtectionsConfig(config.ProtectionsConfig{Enabled: true, MaxDrawdownPct: 0.15})

	e.pf.SetCash(1000)
	e.pf.SyncPositions(nil)

	e.openPos["DRIFTED"] = &OpenPosition{
		Symbol: "DRIFTED", Qty: 10, EntryPrice: 50, EntryTime: time.Now(),
	}

	e.checkDrawdown() // peak = 1000 (cash) + 500 (phantom) = 1500

	// A real loss elsewhere in the account, unrelated to DRIFTED.
	e.pf.SetCash(700)

	e.ForgetOpenPosition("DRIFTED") // un-does exactly the 500 phantom: peak -> 1000

	e.checkDrawdown() // equity now 700; (1000-700)/1000 = 30% >= 15%

	if halted, _ := e.Protections().Halted(); !halted {
		t.Error("a genuine 20% drawdown did not trip after a coincident forget — " +
			"LowerPeak must only cancel the phantom amount, not mask a real loss")
	}
}
