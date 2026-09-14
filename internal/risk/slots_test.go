package risk

import (
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
)

// min_positions_per_asset_class splits a class budget into slots so one
// position cannot take the lot. These tests exercise it through Authorize,
// which is where it binds: unlike the other portfolio caps it sizes the order
// down rather than refusing it, so the interesting assertion is the approved
// quantity, not a boolean.

// slotGuard is a guard with a 50% stock budget split into two positions —
// 25% of equity per stock position — plus a 10% unclassified budget split the
// same way, so the sentinel bucket is exercised alongside a named class.
func slotGuard() *PortfolioGuard {
	return NewPortfolioGuard(config.PortfolioConfig{
		Enabled: true,
		MaxPctPerAssetClass: map[string]float64{
			"stock": 0.50, config.UnclassifiedAssetClass: 0.10,
		},
		MinPositionsPerAssetClass: map[string]int{
			"stock": 2, config.UnclassifiedAssetClass: 2,
		},
	}, nil, map[string]string{"AAPL": "stock", "MSFT": "stock"}, nil)
}

// slotManager is a manager whose own money limits are far away, so only the
// per-class slot can bind.
func slotManager(g *PortfolioGuard) *Manager {
	m := New(config.Limits{
		MaxOrderValue: 1e6, MaxPerPosition: 1e6, MaxTotalInvested: 1e6,
	}, config.CostsConfig{})
	m.SetPortfolioGuard(g)

	return m
}

// slotSnapshot is a book of known equity, so the slot (a fraction of equity)
// is an exact figure.
func slotSnapshot(cash float64, holdings map[string]float64) portfolio.Snapshot {
	snap := portfolio.Snapshot{Cash: cash, Holdings: map[string]portfolio.Holding{}}
	for sym, v := range holdings {
		snap.Holdings[sym] = portfolio.Holding{Symbol: sym, Qty: 1, MarketValue: v}
		snap.Invested += v
		snap.OpenPositions++
	}

	snap.Equity = snap.Cash + snap.Invested

	return snap
}

// The headline case: a 50% stock budget split in two clamps a buy to 25% of
// equity, which is what leaves room for the second position the operator asked
// the class to hold.
func TestSlotClampsABuyToItsShareOfTheClassBudget(t *testing.T) {
	t.Parallel()

	m := slotManager(slotGuard())
	snap := slotSnapshot(1000, nil) // equity 1000, slot = 1000 * 0.5 / 2 = 250

	// Asking for 400 of a symbol whose slot is 250.
	order := broker.Order{Symbol: "AAPL", Side: broker.Buy, Qty: 4, Type: broker.Market}

	authOrder, d := m.Authorize(order, 100, snap)
	if !d.Approved {
		t.Fatalf("a buy inside the class budget was refused: %s", d.Reason)
	}

	if authOrder.Qty != 2.5 {
		t.Errorf("qty = %v, want 2.5 (250 of a 250 slot at 100)", authOrder.Qty)
	}

	if !strings.Contains(d.Reason, "min_positions_per_asset_class") {
		t.Errorf("reason %q does not name the cap that clamped it", d.Reason)
	}
}

// The slot is a ceiling on the POSITION: a scale-in add gets what is left of
// it, so a ladder cannot walk one name up to the whole class budget.
func TestSlotSubtractsWhatIsAlreadyHeld(t *testing.T) {
	t.Parallel()

	m := slotManager(slotGuard())

	// Equity 1000 (800 cash + 200 held), slot 250, 200 of it already used.
	snap := slotSnapshot(800, map[string]float64{"AAPL": 200})

	authOrder, d := m.Authorize(
		broker.Order{Symbol: "AAPL", Side: broker.Buy, Qty: 4, Type: broker.Market}, 100, snap)
	if !d.Approved {
		t.Fatalf("an add inside the remaining slot was refused: %s", d.Reason)
	}

	if authOrder.Qty != 0.5 {
		t.Errorf("qty = %v, want 0.5 (the 50 left of a 250 slot at 100)", authOrder.Qty)
	}

	// A different name in the same class gets its own full slot — the point of
	// the setting is more positions, not less exposure per class.
	other, d := m.Authorize(
		broker.Order{Symbol: "MSFT", Side: broker.Buy, Qty: 4, Type: broker.Market}, 100, snap)
	if !d.Approved {
		t.Fatalf("a second name in the class was refused: %s", d.Reason)
	}

	if other.Qty != 2.5 {
		t.Errorf("second name qty = %v, want its own 250 slot at 100", other.Qty)
	}
}

// A position already worth its whole slot takes nothing more. It fails closed
// with the cap named, like every other exhausted money limit.
func TestSlotBlocksAnAddOnceFull(t *testing.T) {
	t.Parallel()

	m := slotManager(slotGuard())
	snap := slotSnapshot(750, map[string]float64{"AAPL": 250}) // equity 1000, slot 250

	if _, d := m.Authorize(
		broker.Order{Symbol: "AAPL", Side: broker.Buy, Qty: 1, Type: broker.Market}, 100, snap,
	); d.Approved {
		t.Error("a buy was allowed into a position already filling its slot")
	} else if !strings.Contains(d.Reason, "min_positions_per_asset_class") {
		t.Errorf("reason %q does not name the cap", d.Reason)
	}
}

// The unclassified bucket splits like any other class, so the holdings nobody
// could name cannot pile into one position either.
func TestSlotAppliesToTheUnclassifiedBucket(t *testing.T) {
	t.Parallel()

	m := slotManager(slotGuard())
	snap := slotSnapshot(1000, nil) // slot = 1000 * 0.10 / 2 = 50

	authOrder, d := m.Authorize(
		broker.Order{Symbol: "NTA.F", Side: broker.Buy, Qty: 10, Type: broker.Market}, 10, snap)
	if !d.Approved {
		t.Fatalf("an unclassified buy was refused: %s", d.Reason)
	}

	if authOrder.Qty != 5 {
		t.Errorf("qty = %v, want 5 (50 of a 50 slot at 10)", authOrder.Qty)
	}
}

// Nothing configured, nothing changed: no slots, a class with no budget to
// divide, and no guard at all must all leave Authorize exactly as it was.
func TestSlotIsInertWhenNotConfigured(t *testing.T) {
	t.Parallel()

	snap := slotSnapshot(1000, nil)
	order := broker.Order{Symbol: "AAPL", Side: broker.Buy, Qty: 4, Type: broker.Market}

	classes := map[string]string{"AAPL": "stock"}

	guards := map[string]*PortfolioGuard{
		"no guard": nil,
		"caps but no slots": NewPortfolioGuard(config.PortfolioConfig{
			Enabled:             true,
			MaxPctPerAssetClass: map[string]float64{"stock": 0.50},
		}, nil, classes, nil),
		// A slot count for a class with no budget: config.Validate refuses
		// this pairing at load, and the guard must not invent a ceiling for
		// one built in code either.
		"slots but no cap": NewPortfolioGuard(config.PortfolioConfig{
			Enabled:                   true,
			MaxPerSector:              1e6,
			MinPositionsPerAssetClass: map[string]int{"stock": 2},
		}, nil, classes, nil),
		// One slot is the whole budget — the same thing as not splitting.
		"one slot": NewPortfolioGuard(config.PortfolioConfig{
			Enabled:                   true,
			MaxPctPerAssetClass:       map[string]float64{"stock": 0.50},
			MinPositionsPerAssetClass: map[string]int{"stock": 1},
		}, nil, classes, nil),
	}

	for name, g := range guards {
		m := slotManager(g)

		authOrder, d := m.Authorize(order, 100, snap)
		if !d.Approved {
			t.Errorf("%s: buy refused: %s", name, d.Reason)

			continue
		}

		if authOrder.Qty != 4 {
			t.Errorf("%s: qty = %v, want the unclamped 4", name, authOrder.Qty)
		}
	}
}

// BuyHeadroom is what engine-side sizing aims at. If it did not know about the
// slot, sizing would aim past a ceiling Authorize then clamps, and every buy in
// a split class would report as clamped.
func TestBuyHeadroomAccountsForTheSlot(t *testing.T) {
	t.Parallel()

	m := slotManager(slotGuard())
	snap := slotSnapshot(1000, nil)

	got := m.BuyHeadroom(broker.Order{Symbol: "AAPL", Side: broker.Buy}, 100, snap)
	if got != 250 {
		t.Errorf("BuyHeadroom = %v, want the 250 slot", got)
	}
}

// The slot is a fraction of equity, so it moves with the book — the same
// denominator max_pct_per_asset_class uses, including its fallback for a
// snapshot that filled Cash/Invested without deriving Equity.
func TestSlotFollowsEquity(t *testing.T) {
	t.Parallel()

	g := slotGuard()

	grown := slotSnapshot(2000, nil)
	if v, _, ok := g.MaxPositionValue("AAPL", equityOf(grown), 0); !ok || v != 500 {
		t.Errorf("slot on a 2000 book = %v (%v), want 500", v, ok)
	}

	derived := slotSnapshot(1000, nil)
	derived.Equity = 0 // only Cash/Invested filled, as in the backtester's early bars

	if v, _, ok := g.MaxPositionValue("AAPL", equityOf(derived), 0); !ok || v != 250 {
		t.Errorf("slot with no derived equity = %v (%v), want 250", v, ok)
	}

	empty := portfolio.Snapshot{Holdings: map[string]portfolio.Holding{}}
	if _, _, ok := g.MaxPositionValue("AAPL", equityOf(empty), 0); ok {
		t.Error("an empty account produced a slot; there is no book to take a share of")
	}
}
