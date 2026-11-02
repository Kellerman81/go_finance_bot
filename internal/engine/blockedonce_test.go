package engine

import (
	"strings"
	"testing"
)

// The 332-blocked-trades incident, generalised.
//
// That weekend produced 332 identical records because the venue-price refusal
// was written once per eval_interval for as long as the condition held. The
// suffix that caused it was mapped, but the SHAPE of the failure was not
// fixed — and it came back through a different door: three symbols the data
// provider only has daily candles for (UNPRF, PRAJL.XC, 2HP.F) produced
// 2,436 identical blocked trades between them, 1,566 from one ticker. Every
// one of those also fired a notification.
//
// "The provider has no intraday history for this symbol" is a standing
// property, not news each time it is re-checked.
func TestBlockedOnceRecordsAStandingConditionOnce(t *testing.T) {
	e, _ := forgetEngine()
	e.blockedOnce = make(map[string]string)

	const reason = "venue price is 24h0m0s-bar data, not a current price (requested 1m)"

	// The evaluation loop revisits the symbol every 30s; simulate an hour.
	for range 120 {
		e.noteBlockedOnce("PRAJL.XC", reason, 32.275, "")
	}

	if n := len(e.Trades()); n != 1 {
		t.Fatalf("an hour of a standing condition recorded %d blocked trades, want 1 — "+
			"each one is also a notification", n)
	}
}

// A CHANGED reason is new information and must be reported: the symbol moving
// from "no intraday data" to "no headroom" is a different problem with a
// different fix, and swallowing it would hide the second one entirely.
func TestBlockedOnceReportsAChangedReason(t *testing.T) {
	e, _ := forgetEngine()
	e.blockedOnce = make(map[string]string)

	e.noteBlockedOnce("X", "venue price is 24h0m0s-bar data", 10, "")
	e.noteBlockedOnce("X", "venue price is 24h0m0s-bar data", 10, "")
	e.noteBlockedOnce("X", "max_daily_spend leaves 99.02", 10, "")

	trades := e.Trades()
	if len(trades) != 2 {
		t.Fatalf("recorded %d blocked trades, want 2 (one per distinct reason)", len(trades))
	}

	var seen []string
	for _, tr := range trades {
		seen = append(seen, tr.Reason)
	}

	joined := strings.Join(seen, " | ")
	if !strings.Contains(joined, "24h0m0s") || !strings.Contains(joined, "max_daily_spend") {
		t.Errorf("both distinct reasons should appear, got: %s", joined)
	}
}

// Clearing on success means the NEXT occurrence is reported rather than
// swallowed as a repeat — a symbol that recovers and fails again is two
// separate events.
func TestBlockedOnceReportsAgainAfterRecovery(t *testing.T) {
	e, _ := forgetEngine()
	e.blockedOnce = make(map[string]string)

	const reason = "venue price is 24h0m0s-bar data"

	e.noteBlockedOnce("X", reason, 10, "")
	e.clearBlockedOnce("X") // the gate passed on some later evaluation
	e.noteBlockedOnce("X", reason, 10, "")

	if n := len(e.Trades()); n != 2 {
		t.Errorf("recorded %d, want 2 — a recurrence after recovery is a new event", n)
	}
}
