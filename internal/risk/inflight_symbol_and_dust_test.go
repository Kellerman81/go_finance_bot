package risk

import (
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
)

// RK3-1: RK2-2 counted in-flight buys against the account-wide caps only. A
// resting limit buy of 500 left max_per_position 500 reporting 500 of room for
// the SAME symbol, so each evaluation until it resolved could approve another
// full clip — 2000 in one name under a 500 cap.
func TestAnInFlightBuyCountsAgainstItsOwnPositionCap(t *testing.T) {
	m := New(config.Limits{MaxPerPosition: 500}, config.CostsConfig{})
	snap := slotSnapshot(5000, nil)
	m.ReserveBuy("AAA", 5, 100) // 500 resting: the whole cap

	if _, d := m.Authorize(broker.Order{Symbol: "AAA", Side: broker.Buy, Qty: 5}, 100, snap); d.Approved {
		t.Errorf("max_per_position 500 approved %.2f more of AAA with 500 in flight", d.FinalQty*100)
	}

	if h := m.BuyHeadroom(broker.Order{Symbol: "AAA", Side: broker.Buy}, 100, snap); h > 0 {
		t.Errorf("BuyHeadroom reports %.2f for AAA with its whole cap in flight", h)
	}

	if !m.HasReservation("aaa") {
		t.Error("HasReservation missed the in-flight buy (symbols are matched normalised)")
	}
}

// RK3-1, the slot: min_positions 4 on 1000 is a 250 slot; with 250 in flight a
// second 250 of the same name was approved.
func TestAnInFlightBuyCountsAgainstItsSlot(t *testing.T) {
	m := New(config.Limits{}, config.CostsConfig{})
	m.SetMinPositions(4)
	snap := slotSnapshot(1000, nil)
	m.ReserveBuy("AAA", 2.5, 100)

	if _, d := m.Authorize(broker.Order{Symbol: "AAA", Side: broker.Buy, Qty: 2.5}, 100, snap); d.Approved {
		t.Errorf("a 250 slot approved %.2f more of AAA with 250 in flight", d.FinalQty*100)
	}
}

// RK3-2: PF2-4 made dust "not a position" for the counts and Authorize, but the
// guard's class, sector and correlation sums still tested Qty <= 0. A 5e-7
// broker remainder took a max_positions_per_sector slot, blocking other names
// in the sector and re-entry into the dust symbol itself.
func TestTheGuardDoesNotCountDustAsAPosition(t *testing.T) {
	g := NewPortfolioGuard(config.PortfolioConfig{Enabled: true, MaxPositionsPerSector: 1},
		map[string]string{"AAA": "tech", "BBB": "tech"}, nil, nil)
	snap := portfolio.Snapshot{Cash: 1000, Equity: 1000, Holdings: map[string]portfolio.Holding{
		"AAA": {Symbol: "AAA", Qty: 5e-7, MarketValue: 0.0001},
	}}

	if ok, reason := g.AllowEntry("BBB", snap, 100); !ok {
		t.Errorf("a dust AAA row blocked BBB: %s", reason)
	}

	if ok, reason := g.AllowEntry("AAA", snap, 100); !ok {
		t.Errorf("a dust AAA row blocked re-entering AAA: %s", reason)
	}
}

// SC3-5: a hand-written asset_classes label was only lower-cased, so
// {X: ETFs} founded a class "etfs" that no etf cap, profile or pool knew.
// Class labels are canonicalised the way the classifier's output is.
func TestAHandWrittenClassAliasBucketsWithTheClassifier(t *testing.T) {
	t.Parallel()

	got := NormalizeClassLabels(map[string]string{"iwda.as": "ETFs", "sap.de": "Equity"})
	if got["IWDA.AS"] != "etf" || got["SAP.DE"] != "stock" {
		t.Errorf("labels = %v, want IWDA.AS→etf and SAP.DE→stock", got)
	}
}

// EX3-4, entry side: with mult·ATR at or above the price the ATR stop cannot
// sit above zero, the percentage stop takes over, and the most volatile entry
// went out at FULL size behind the narrowest stop. It is refused.
func TestAnEntryWhoseATRStopCannotBePlacedIsRefused(t *testing.T) {
	t.Parallel()

	m := New(config.Limits{MaxOrderValue: 1000, MaxTotalInvested: 10000}, config.CostsConfig{})
	cfg := SizingConfig{Exits: config.ExitsConfig{Enabled: true, StopLossPct: 0.05, ATRStopMult: 3}}

	if got := m.SizeBuy("X", 10, portfolio.Snapshot{Cash: 10000, Equity: 10000}, cfg, 3.4); got.OK {
		t.Errorf("a buy at ATR 3.4 on a 10 price (3 ATR > price) was sized at %.2f", got.Notional)
	}
}
