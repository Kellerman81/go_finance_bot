package engine

import (
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// sellTranche books one realised exit slice the way every fill site does:
// record() first, recordClose() after.
func sellTranche(e *Engine, sym string, qty, pnl float64, reason string) {
	e.record(TradeRecord{
		Time: time.Now(), Symbol: sym, Side: broker.Sell, Qty: qty, Price: 100,
		Status: "filled", Reason: reason, PnL: &pnl,
	})
	e.recordClose(sym, qty)
}

// PR2-6 (c): a trade that banked +500 on a scale-out rung and closed its
// remainder at -1 is a winner. Judged per tranche, the -1 slice started a loss
// cooldown and locked the symbol out of a re-entry.
func TestCooldownJudgesTheRoundTripNotItsLastTranche(t *testing.T) {
	e, _ := forgetEngine()
	e.SetProtectionsConfig(config.ProtectionsConfig{Enabled: true, CooldownAfterLoss: time.Hour})
	e.openPos["AAPL"] = &OpenPosition{Symbol: "AAPL", Qty: 10, EntryPrice: 100}

	sellTranche(e, "AAPL", 5, 500, "partial take-profit rung 1")
	sellTranche(e, "AAPL", 5, -1, "strategy sell")

	if ok, reason := e.Protections().AllowEntry("AAPL"); !ok {
		t.Errorf("a +499 round trip started a loss cooldown: %s", reason)
	}
}

// PR2-6 (b): a stop-loss the broker filled in two slices is one stop event.
// Counted per tranche it was two, and stop_guard_trades 2 locked the symbol
// after a single stop.
func TestAPartiallyFilledStopCountsOnceTowardTheStopGuard(t *testing.T) {
	e, _ := forgetEngine()
	e.SetProtectionsConfig(config.ProtectionsConfig{
		Enabled: true, StopGuardTrades: 2, StopGuardLookback: time.Hour, StopGuardLock: time.Hour,
	})
	e.openPos["AAPL"] = &OpenPosition{Symbol: "AAPL", Qty: 10, EntryPrice: 100}

	sellTranche(e, "AAPL", 6, -30, "stop-loss")
	sellTranche(e, "AAPL", 4, -20, "stop-loss")

	if ok, reason := e.Protections().AllowEntry("AAPL"); !ok {
		t.Errorf("one partially filled stop tripped a 2-stop guard: %s", reason)
	}
}

// PR3-1: the round trip closed only below 1e-9, while everything else treats
// up to portfolio.DustQty (1e-6) as nothing — and sells nothing smaller. A
// fractional broker's booked buy and sold quantity differing in the 7th
// decimal left the record open for good: a -100 stop-out started no cooldown.
func TestASubDustResidualClosesTheRoundTrip(t *testing.T) {
	e, _ := forgetEngine()
	e.SetProtectionsConfig(config.ProtectionsConfig{Enabled: true, CooldownAfterLoss: time.Hour})
	e.openPos["AAPL"] = &OpenPosition{Symbol: "AAPL", Qty: 10.0000005, EntryPrice: 100}

	sellTranche(e, "AAPL", 10, -100, "stop-loss")

	if ok, _ := e.Protections().AllowEntry("AAPL"); ok {
		t.Error("a -100 stop-out started no loss cooldown: a sub-dust residual kept the trip open")
	}

	if _, open := e.openPos["AAPL"]; open {
		t.Error("a sub-dust residual record survived the exit")
	}
}

// PR3-1's second half: the stale residual's loss was folded into the next,
// winning trade, which then started a loss cooldown.
func TestASubDustResidualDoesNotLeakIntoTheNextTrade(t *testing.T) {
	e, _ := forgetEngine()
	e.SetProtectionsConfig(config.ProtectionsConfig{Enabled: true, CooldownAfterLoss: time.Hour})
	e.openPos["AAPL"] = &OpenPosition{Symbol: "AAPL", Qty: 10.0000005, EntryPrice: 100}
	sellTranche(e, "AAPL", 10, -100, "stop-loss")

	e.Protections().Unlock("AAPL")
	e.recordOpen("AAPL", 5, 100, "")
	sellTranche(e, "AAPL", 5.0000005, 50, "take-profit") // the broker's holding, residual included

	if ok, reason := e.Protections().AllowEntry("AAPL"); !ok {
		t.Errorf("a +50 winner was judged a loser: %s", reason)
	}
}
