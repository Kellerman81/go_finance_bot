package risk

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// TestSizeBuySeesANewDayWithoutARoll is the regression for RK2-1. The buy
// headroom read the raw spentToday, and only Authorize rolls the day. Live and
// the backtester both size BEFORE authorising, so once yesterday had used up
// max_daily_spend every SizeBuy refused — Authorize was never reached, the day
// never rolled, and a headless bot with no exits stayed locked out of buys
// until a sell, a manual order or a UI poll happened to roll it.
func TestSizeBuySeesANewDayWithoutARoll(t *testing.T) {
	now := time.Date(2026, 10, 1, 15, 0, 0, 0, time.UTC)

	m := New(config.Limits{MaxDailySpend: 500}, config.CostsConfig{})
	m.SetClock(func() time.Time { return now })
	m.Restore("2026-10-01", 0)
	m.RecordBuy(500)

	now = now.Add(24 * time.Hour)

	if s := m.SizeBuy("AAA", 100, slotSnapshot(5000, nil), SizingConfig{MinOrderSizeUSD: 50}, 0); !s.OK {
		t.Errorf("the day after max_daily_spend was used up, SizeBuy still refused: %s", s.Reason)
	}
}

// TestInFlightBuysCountAgainstInvestedAndOpenPositions is the regression for
// RK2-2. A resting limit or unconfirmed buy is in neither the broker snapshot
// nor openPos, so pendingExposure cannot add it, and its reservation was only
// subtracted from the cash and daily-spend headroom. With max_total_invested
// 1000 and max_open_positions 1, a second 1000 buy of another symbol was
// approved while the first still rested: 2000 invested, 2 positions.
func TestInFlightBuysCountAgainstInvestedAndOpenPositions(t *testing.T) {
	m := New(config.Limits{MaxTotalInvested: 1000}, config.CostsConfig{})
	snap := slotSnapshot(5000, nil)

	m.ReserveBuy("AAA", 10, 100) // resting, worth the whole cap

	if _, d := m.Authorize(broker.Order{Symbol: "BBB", Side: broker.Buy, Qty: 10}, 100, snap); d.Approved {
		t.Errorf("max_total_invested 1000 approved %.2f more while 1000 was in flight", d.FinalQty*100)
	}

	m2 := New(config.Limits{MaxTotalInvested: 1e6, MaxOpenPositions: 1}, config.CostsConfig{})
	m2.ReserveBuy("AAA", 1, 100)

	_, d := m2.Authorize(broker.Order{Symbol: "BBB", Side: broker.Buy, Qty: 1}, 100, snap)
	if d.Approved || !strings.Contains(d.Reason, "max_open_positions") {
		t.Errorf("max_open_positions 1 approved a second symbol with one in flight: %+v", d)
	}

	// Adding to the in-flight symbol itself is not a new position.
	if _, d := m2.Authorize(broker.Order{Symbol: "AAA", Side: broker.Buy, Qty: 1}, 100, snap); !d.Approved {
		t.Errorf("a second clip of the in-flight symbol was counted as a new position: %s", d.Reason)
	}
}

// TestSlotsAloneBuildAGuard is the regression for RK2-3: with portfolio limits
// on and ONLY min_positions_per_asset_class set, NewPortfolioGuard returned
// nil ("nothing to enforce"), and a 1000 buy went through where the slot is
// 250.
func TestSlotsAloneBuildAGuard(t *testing.T) {
	cfg := config.PortfolioConfig{Enabled: true, MinPositionsPerAssetClass: map[string]int{"stock": 4}}

	g := NewPortfolioGuard(cfg, nil, map[string]string{"AAA": "stock"}, nil)
	if g == nil {
		t.Fatal("slots-only portfolio config built no guard")
	}

	m := New(config.Limits{}, config.CostsConfig{})
	m.SetPortfolioGuard(g)

	if _, d := m.Authorize(broker.Order{Symbol: "AAA", Side: broker.Buy, Qty: 10}, 100, slotSnapshot(1000, nil)); d.FinalQty > 2.5+1e-9 {
		t.Errorf("approved %.4f shares (%.2f), want at most one 250 slot", d.FinalQty, d.FinalQty*100)
	}
}

// TestSingleSlotOnABudgetlessClassBinds is the regression for RK2-4. A slot
// count of 1 on a class with no budget of its own caps a position at the
// leftover the named budgets did not claim — MaxPositionValue implements it —
// but normalisation dropped every 1 first, so bond had no ceiling at all.
func TestSingleSlotOnABudgetlessClassBinds(t *testing.T) {
	cfg := config.PortfolioConfig{
		Enabled:                   true,
		MaxPctPerAssetClass:       map[string]float64{"stock": 0.6},
		MinPositionsPerAssetClass: map[string]int{"stock": 3, "bond": 1},
	}

	g := NewPortfolioGuard(cfg, nil, map[string]string{"AAA": "stock", "BND": "bond"}, nil)

	if v, _, ok := g.MaxPositionValue("BND", 1000, 0); !ok || math.Abs(v-400) > 1e-9 {
		t.Errorf("bond's single slot = %v (binding %v), want the 400 left over by stock's 60%%", v, ok)
	}
}

// TestAuthorizeRefusesANonFinitePrice is the regression for RK2-5: "<= 0" is
// false for NaN, every cap's maxValue/NaN comparison is false too, so a NaN
// price was approved at the full requested quantity with every limit bypassed.
func TestAuthorizeRefusesANonFinitePrice(t *testing.T) {
	m := New(config.Limits{MaxOrderValue: 100, MaxTotalInvested: 500}, config.CostsConfig{})

	for _, px := range []float64{math.NaN(), math.Inf(1)} {
		if _, d := m.Authorize(broker.Order{Symbol: "AAA", Side: broker.Buy, Qty: 50}, px, slotSnapshot(1000, nil)); d.Approved {
			t.Errorf("price %v approved %v shares", px, d.FinalQty)
		}
	}
}

// TestAuthorizeFailsClosedOnUnusableCosts is the regression for RK2-6: with
// commission_pct at 1 the effective price the cash caps divide by is 0 or
// less, and the cash_reserve clamp silently stopped clamping.
func TestAuthorizeFailsClosedOnUnusableCosts(t *testing.T) {
	m := New(config.Limits{CashReserve: 900}, config.CostsConfig{CommissionPct: -1})

	if _, d := m.Authorize(broker.Order{Symbol: "AAA", Side: broker.Buy, Qty: 50}, 100, slotSnapshot(1000, nil)); d.Approved {
		t.Errorf("cost settings with no valid effective price approved %v shares past a 900 cash reserve", d.FinalQty)
	}
}

// TestPercentOnlyCeilingWithNoEquityAdmitsNothing is the regression for
// RK2-7. Resolve's comment promised that a non-positive equity never turns a
// percentage into "no ceiling", but a percentage-only ceiling resolved to 0 —
// which every caller reads as unlimited. Unreachable today (cash_reserve
// refuses first), but the comment is now the behaviour.
func TestPercentOnlyCeilingWithNoEquityAdmitsNothing(t *testing.T) {
	t.Parallel()

	r := config.Limits{MaxOrderValuePct: 0.1, MaxTotalInvestedPct: 0.5}.Resolve(0)

	if r.MaxOrderValue <= 0 || r.MaxTotalInvested <= 0 {
		t.Errorf("percentage-only ceilings at equity 0 resolved to %v / %v — read as unlimited",
			r.MaxOrderValue, r.MaxTotalInvested)
	}

	if r.MaxOrderValue > 1e-300 {
		t.Errorf("max_order_value resolved to %v, want a ceiling that admits nothing", r.MaxOrderValue)
	}
}
