package engine

import (
	"context"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
)

// The weekend that produced 332 identical blocked trades.
//
// PRAJL.XC's .XC suffix was not in the exchange table, ExchangeFor reported the
// venue as unknown, and OrderingAllowed fails open for an unknown venue — so the
// market-hours gate waved a Saturday entry through, the venue-price check then
// correctly refused it (a venue that is shut has no current price), and the
// refusal was recorded. Once per eval_interval, for the whole weekend.
//
// The suffix is mapped now, but the general case is what these tests pin: an
// entry on a venue nobody can name must not be attempted at the weekend.

func TestWeekendBlocksAnEntryOnAnUnknownVenue(t *testing.T) {
	e, _ := hoursEngine(t, "FOO.ZZ", false, false) // broker cannot answer either

	saturday := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

	atTime(t, saturday, func() {
		if ok, why := e.entryAllowed("FOO.ZZ"); ok {
			t.Error("a Saturday entry on an unknown venue was allowed")
		} else if why == "" {
			t.Error("the refusal carried no reason")
		}
	})

	// Midweek the uncertainty resolves the old way: an unmodelled venue is not
	// silently untradeable, it just has no session to gate.
	atTime(t, time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC), func() {
		if ok, why := e.entryAllowed("FOO.ZZ"); !ok {
			t.Errorf("a Wednesday entry on an unknown venue was blocked: %s", why)
		}
	})
}

// The same weekend, through the scale-in ladder: this path ran the shared entry
// gates LAST, after the venue-price check, so a closed venue produced a recorded
// "cannot confirm a venue price" block instead of the silent skip act() does.
func TestScaleInOnAClosedVenueRecordsNothing(t *testing.T) {
	e, brk := hoursEngine(t, "SAP.DE", false, false)
	e.cfg = config.EngineConfig{
		MarketHoursCheck: true, VenuePriceCheck: true,
		ScaleIn: config.ScaleInConfig{
			Enabled: true, MaxAdds: 3, StepPct: 0.02, SizePct: 0.5,
		},
	}

	before := len(e.Trades())

	// Saturday: Xetra is shut, and there is no history source to price from —
	// which is exactly the combination that used to produce a block record.
	atTime(t, time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC), func() {
		e.addToPosition(context.Background(), "SAP.DE", 90, portfolio.Snapshot{
			Cash: 1000, Equity: 1000, Holdings: map[string]portfolio.Holding{},
		})
	})

	if got := len(e.Trades()) - before; got != 0 {
		t.Errorf("recorded %d trade(s) for a scale-in into a closed venue, want 0", got)
	}

	if brk.orders != 0 {
		t.Errorf("submitted %d order(s) into a closed venue, want 0", brk.orders)
	}
}
