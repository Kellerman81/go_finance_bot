package risk

import (
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
)

// TestBuyHeadroomMatchesTheBindingCap confirms BuyHeadroom reports the exact
// same dollar figure as whichever cap Authorize itself would find binding —
// with zero costs (so estPrice == effPrice, the price every cap's headroom
// converts through), headroom/estPrice must equal the qty Authorize clamps
// to when that cap is the tightest one. This is the core contract: an
// engine-side sizing estimate built from BuyHeadroom must never aim higher
// than what Authorize will actually approve.
func TestBuyHeadroomMatchesTheBindingCap(t *testing.T) {
	snap := portfolio.Snapshot{
		Cash:     10000,
		Invested: 2000,
		Holdings: map[string]portfolio.Holding{
			"AAPL": {Qty: 10, MarketValue: 1000},
		},
	}
	order := broker.Order{Symbol: "AAPL", Side: broker.Buy, Qty: 1000}
	const estPrice = 100.0

	tests := []struct {
		name       string
		limits     config.Limits
		wantCap    string
		wantHeaded float64
	}{
		{
			"max_order_value binds",
			config.Limits{MaxOrderValue: 300, MaxPerPosition: 1e6, MaxTotalInvested: 1e6},
			"max_order_value", 300,
		},
		{
			"max_per_position binds (net of the AAPL holding already worth 1000)",
			config.Limits{MaxOrderValue: 1e6, MaxPerPosition: 1500, MaxTotalInvested: 1e6},
			"max_per_position", 500,
		},
		{
			"max_total_invested binds (net of 2000 already invested)",
			config.Limits{MaxOrderValue: 1e6, MaxPerPosition: 1e6, MaxTotalInvested: 2400},
			"max_total_invested", 400,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := New(tc.limits, config.CostsConfig{})

			headroom := m.BuyHeadroom(order, estPrice, snap)
			if headroom != tc.wantHeaded {
				t.Fatalf("BuyHeadroom = %v, want %v (%s)", headroom, tc.wantHeaded, tc.wantCap)
			}

			_, d := m.Authorize(order, estPrice, snap)
			if !d.Approved {
				t.Fatalf("Authorize refused: %s", d.Reason)
			}

			if got, want := d.FinalQty, headroom/estPrice; got != want {
				t.Errorf("Authorize clamped to qty %v, want %v (BuyHeadroom/estPrice) — "+
					"BuyHeadroom must agree with what Authorize actually approves", got, want)
			}
		})
	}
}

// TestBuyHeadroomAccountsForCashReserveAndDailySpend checks the two caps
// whose headroom is computed against an effPrice (spread/slippage/commission
// adjusted), not the bare estPrice — BuyHeadroom must apply the identical
// formula Authorize does, not a naive estPrice-only figure.
//
// It also pins the unit conversion those two caps need, which is what
// TestBuyHeadroomMatchesTheBindingCap cannot see: that test runs with zero
// costs, where estPrice == effPrice and the two are indistinguishable. A
// cash cap's headroom is denominated in CASH COST, so 500 of max_daily_spend
// does not buy 500 of stock — it buys 500/effPrice shares, worth 500·
// estPrice/effPrice. Reporting the raw 500 as though it were notional
// overstated the ceiling by the whole cost factor and broke the contract the
// test above states in prose: an estimate built from BuyHeadroom must never
// aim higher than what Authorize will actually approve.
func TestBuyHeadroomAccountsForCashReserveAndDailySpend(t *testing.T) {
	costs := config.CostsConfig{CommissionPct: 0.01, SpreadPct: 0.001, SlippagePct: 0.0005}
	// effPrice = estPrice·(1 + 0.01 + 0.001/2 + 0.0005) = estPrice·1.011.
	const (
		estPrice = 100.0
		effMult  = 1.011
	)

	snap := portfolio.Snapshot{Cash: 1000, Holdings: map[string]portfolio.Holding{}}
	order := broker.Order{Symbol: "AAPL", Side: broker.Buy, Qty: 1000}

	tests := []struct {
		name     string
		limits   config.Limits
		cashCap  float64 // the binding cap's remaining CASH headroom
		bindsVia string
	}{
		{
			// max_daily_spend (500) is tighter than cash_reserve here, which
			// leaves 1000-100=900 (no fixed fee: CommissionFlat/Min are 0).
			// Both must be present and correctly computed for the min to be right.
			"max_daily_spend binds",
			config.Limits{
				MaxOrderValue: 1e6, MaxPerPosition: 1e6, MaxTotalInvested: 1e6,
				MaxDailySpend: 500, CashReserve: 100,
			},
			500, "max_daily_spend",
		},
		{
			"cash_reserve binds",
			config.Limits{
				MaxOrderValue: 1e6, MaxPerPosition: 1e6, MaxTotalInvested: 1e6,
				MaxDailySpend: 1e6, CashReserve: 700,
			},
			300, "cash_reserve", // 1000-700
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := New(tc.limits, costs)

			got := m.BuyHeadroom(order, estPrice, snap)

			want := tc.cashCap / effMult // cash headroom converted to notional
			if diff := got - want; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("BuyHeadroom = %v, want %v (%s: %v of cash buys %v of stock)",
					got, want, tc.bindsVia, tc.cashCap, want)
			}

			if got >= tc.cashCap {
				t.Errorf("BuyHeadroom = %v, which is not below the %v of CASH the cap "+
					"leaves — the cost of buying has been dropped", got, tc.cashCap)
			}

			// The contract itself: sizing built on this must not aim above
			// what Authorize approves. Authorize floors qty to 4 decimals, so
			// it lands at or just under BuyHeadroom/estPrice, never above.
			_, d := m.Authorize(order, estPrice, snap)
			if !d.Approved {
				t.Fatalf("Authorize refused: %s", d.Reason)
			}

			ceiling := got / estPrice
			if d.FinalQty > ceiling || ceiling-d.FinalQty > 1e-4 {
				t.Errorf("Authorize approved qty %v against a BuyHeadroom ceiling of %v — "+
					"they must agree to within one 4-decimal rounding step",
					d.FinalQty, ceiling)
			}
		})
	}
}

// TestBuyHeadroomAccountsForReservations confirms in-flight reservations
// (Reserve) reduce headroom exactly like they reduce what Authorize would
// approve — a second candidate buy evaluated before the first one's order
// resolves must not see room already committed to the first.
func TestBuyHeadroomAccountsForReservations(t *testing.T) {
	m := New(config.Limits{MaxOrderValue: 1e6, MaxPerPosition: 1e6, MaxTotalInvested: 1e6, CashReserve: 0},
		config.CostsConfig{})

	snap := portfolio.Snapshot{Cash: 1000, Holdings: map[string]portfolio.Holding{}}
	order := broker.Order{Symbol: "AAPL", Side: broker.Buy, Qty: 1000}

	before := m.BuyHeadroom(order, 100, snap)
	if before != 1000 {
		t.Fatalf("BuyHeadroom before any reservation = %v, want 1000", before)
	}

	id := m.ReserveBuy("", 3, 100) // reserves ~300 (+ commission, here 0)

	after := m.BuyHeadroom(order, 100, snap)
	if after != 700 {
		t.Errorf("BuyHeadroom after reserving 300 = %v, want 700", after)
	}

	m.Release(id)

	if got := m.BuyHeadroom(order, 100, snap); got != before {
		t.Errorf("BuyHeadroom after Release = %v, want back to %v", got, before)
	}
}

// TestBuyHeadroomNeverNegative confirms an already-over-limit position (e.g.
// a manual order pushed max_per_position over) reports 0 headroom, not a
// negative number a caller might mistakenly treat as "room to spare."
func TestBuyHeadroomNeverNegative(t *testing.T) {
	m := New(config.Limits{MaxOrderValue: 1e6, MaxPerPosition: 500, MaxTotalInvested: 1e6},
		config.CostsConfig{})

	snap := portfolio.Snapshot{
		Cash: 1e6,
		Holdings: map[string]portfolio.Holding{
			"AAPL": {Qty: 10, MarketValue: 900}, // already over the 500 cap
		},
	}
	order := broker.Order{Symbol: "AAPL", Side: broker.Buy, Qty: 1000}

	if got := m.BuyHeadroom(order, 100, snap); got != 0 {
		t.Errorf("BuyHeadroom = %v, want 0 (already over max_per_position)", got)
	}
}

// TestBuyHeadroomIgnoresDisabledCaps confirms a cap left at its disabling
// value (0, except cash_reserve which has no disabled state) does not
// constrain headroom, matching Authorize's own `enabled` gating.
func TestBuyHeadroomIgnoresDisabledCaps(t *testing.T) {
	m := New(config.Limits{
		MaxOrderValue: 0, MaxPerPosition: 0, MaxTotalInvested: 0, MaxDailySpend: 0,
		CashReserve: 0,
	}, config.CostsConfig{})

	snap := portfolio.Snapshot{Cash: 42, Holdings: map[string]portfolio.Holding{}}
	order := broker.Order{Symbol: "AAPL", Side: broker.Buy, Qty: 1000}

	if got := m.BuyHeadroom(order, 100, snap); got != 42 {
		t.Errorf("BuyHeadroom = %v, want 42 (only the always-on cash_reserve cap binds)", got)
	}
}

// TestBuyHeadroomIsReadOnly confirms calling BuyHeadroom does not itself
// consume budget or roll the trading day — it must be safe to call
// repeatedly for sizing without side effects on later Authorize calls.
func TestBuyHeadroomIsReadOnly(t *testing.T) {
	m := New(config.Limits{MaxOrderValue: 1e6, MaxPerPosition: 1e6, MaxTotalInvested: 1e6, MaxDailySpend: 1000},
		config.CostsConfig{})

	snap := portfolio.Snapshot{Cash: 1e6, Holdings: map[string]portfolio.Holding{}}
	order := broker.Order{Symbol: "AAPL", Side: broker.Buy, Qty: 1000}

	before := m.SpentToday()

	for range 5 {
		m.BuyHeadroom(order, 100, snap)
	}

	if got := m.SpentToday(); got != before {
		t.Errorf("SpentToday changed from %v to %v after calling BuyHeadroom — it must be read-only", before, got)
	}

	// A full max_daily_spend budget must still be available afterward.
	if got := m.BuyHeadroom(order, 100, snap); got != 1000 {
		t.Errorf("BuyHeadroom after repeated calls = %v, want 1000 (unchanged)", got)
	}
}

// A buy squeezed under the $1 minimum must say WHICH limit did the squeezing.
// Without it the operator sees only "clamped below tradable minimum" and cannot
// tell whether they are out of cash, at max_total_invested, or up against the
// daily spend budget — three problems with three different answers.
func TestClampedBelowMinimumNamesTheBindingCap(t *testing.T) {
	t.Parallel()

	// Plenty of every other allowance, but cash sits essentially on the
	// reserve: the exact state a fully-invested account reaches.
	m := New(config.Limits{
		MaxOrderValue:    300,
		MaxPerPosition:   300,
		MaxTotalInvested: 950,
		MaxDailySpend:    500,
		CashReserve:      50,
	}, config.CostsConfig{})

	snap := portfolio.Snapshot{
		Cash: 50.5, Equity: 500, Invested: 449.5,
		Holdings: map[string]portfolio.Holding{},
	}

	_, d := m.Authorize(
		broker.Order{Symbol: "MRVL", Side: broker.Buy, Qty: 1}, 201.81, snap)

	if d.Approved {
		t.Fatal("a sub-$1 order was approved")
	}

	if !strings.Contains(d.Reason, "cash_reserve") {
		t.Errorf("reason %q does not name cash_reserve as the binding cap", d.Reason)
	}

	if !strings.Contains(d.Reason, "clamped below tradable minimum") {
		t.Errorf("reason %q lost the original explanation", d.Reason)
	}
}

// The same squeeze from a different cap names that one instead, so the message
// actually discriminates rather than always blaming cash.
func TestClampedBelowMinimumNamesMaxTotalInvested(t *testing.T) {
	t.Parallel()

	m := New(config.Limits{
		MaxOrderValue:    300,
		MaxPerPosition:   300,
		MaxTotalInvested: 950,
		MaxDailySpend:    500,
		CashReserve:      10,
	}, config.CostsConfig{})

	// Cash is ample; it is the invested total that has no room left.
	snap := portfolio.Snapshot{
		Cash: 5000, Equity: 5950, Invested: 949.9,
		Holdings: map[string]portfolio.Holding{},
	}

	_, d := m.Authorize(
		broker.Order{Symbol: "MRVL", Side: broker.Buy, Qty: 1}, 201.81, snap)

	if d.Approved {
		t.Fatal("a sub-$1 order was approved")
	}

	if !strings.Contains(d.Reason, "max_total_invested") {
		t.Errorf("reason %q does not name max_total_invested", d.Reason)
	}
}
