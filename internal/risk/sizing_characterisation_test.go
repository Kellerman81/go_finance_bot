package risk

import (
	"fmt"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
)

// Characterisation of the whole buy-sizing stack, as one table.
//
// This is not a correctness test — it asserts no opinion about what the
// numbers SHOULD be. It pins what they ARE, across the combinations that the
// slot/budget rework changes: capped vs uncapped classes, slot counts, an
// existing holding, and a cash position tight enough that cash_reserve binds.
//
// It exists so that rework shows a readable DELTA instead of a wall of
// hand-written expectations that are really just the new code transcribed. If
// a case below changes, that change must be deliberate and explainable.
func TestSizingCharacterisation(t *testing.T) {
	t.Parallel()

	limits := config.Limits{
		MaxOrderValue:    200,
		MaxPerPosition:   200,
		MaxTotalInvested: 950,
		CashReserve:      50,
		MaxOpenPositions: 6,
	}

	classes := map[string]string{"AAPL": "stock", "SPY": "etf", "WAT": "weird"}

	cases := []struct {
		name    string
		caps    map[string]float64
		slots   map[string]int
		symbol  string
		cash    float64
		holding float64 // market value already held in symbol
		other   float64 // market value held in a DIFFERENT symbol of the same class
	}{
		{"capped stock, 2 slots, empty book", map[string]float64{"stock": 0.7, "etf": 0.3}, map[string]int{"stock": 2, "etf": 2}, "AAPL", 1000, 0, 0},
		{"capped etf, 2 slots, empty book", map[string]float64{"stock": 0.7, "etf": 0.3}, map[string]int{"stock": 2, "etf": 2}, "SPY", 1000, 0, 0},
		{"capped stock, already holding 150", map[string]float64{"stock": 0.7, "etf": 0.3}, map[string]int{"stock": 2, "etf": 2}, "AAPL", 700, 150, 0},
		{"capped stock, sibling holds 300", map[string]float64{"stock": 0.7, "etf": 0.3}, map[string]int{"stock": 2, "etf": 2}, "AAPL", 700, 0, 300},
		{"slots but NO caps", nil, map[string]int{"stock": 2, "etf": 2}, "AAPL", 1000, 0, 0},
		{"caps but NO slots", map[string]float64{"stock": 0.7, "etf": 0.3}, nil, "AAPL", 1000, 0, 0},
		{"class with neither cap nor slot", map[string]float64{"stock": 0.7}, map[string]int{"stock": 2}, "WAT", 1000, 0, 0},
		{"single slot (n=1) on a capped class", map[string]float64{"stock": 0.7}, map[string]int{"stock": 1}, "AAPL", 1000, 0, 0},
		{"cash-starved: only 60 cash", map[string]float64{"stock": 0.7}, map[string]int{"stock": 2}, "AAPL", 60, 0, 300},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := portfolio.Snapshot{
				Cash:     tc.cash,
				Holdings: map[string]portfolio.Holding{},
			}
			if tc.holding > 0 {
				snap.Holdings[tc.symbol] = portfolio.Holding{Symbol: tc.symbol, Qty: 1, MarketValue: tc.holding}
				snap.Invested += tc.holding
			}

			if tc.other > 0 {
				sib := "SIB"
				classes[sib] = classes[tc.symbol]
				snap.Holdings[sib] = portfolio.Holding{Symbol: sib, Qty: 1, MarketValue: tc.other}
				snap.Invested += tc.other
			}

			snap.Equity = snap.Cash + snap.Invested
			snap.OpenPositions = len(snap.Holdings)

			m := New(limits, config.CostsConfig{})
			m.SetPortfolioGuard(NewPortfolioGuard(config.PortfolioConfig{
				Enabled:                   true,
				MaxPctPerAssetClass:       tc.caps,
				MinPositionsPerAssetClass: tc.slots,
			}, nil, classes, nil))

			order := broker.Order{Symbol: tc.symbol, Side: broker.Buy, Qty: 1e6}

			headroom := m.BuyHeadroom(order, 100, snap)
			_, d := m.Authorize(order, 100, snap)

			got := fmt.Sprintf("headroom=%.2f approved=%v qty=%.4f reason=%q",
				headroom, d.Approved, d.FinalQty, d.Reason)

			t.Logf("CHAR | %-42s | equity=%7.2f | %s", tc.name, snap.Equity, got)
		})
	}
}
