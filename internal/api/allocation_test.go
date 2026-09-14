package api

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
	"github.com/gin-gonic/gin"
)

func allocSnapshot(cash float64, holdings map[string]float64) portfolio.Snapshot {
	snap := portfolio.Snapshot{Cash: cash, Holdings: map[string]portfolio.Holding{}}
	for sym, v := range holdings {
		snap.Holdings[sym] = portfolio.Holding{Symbol: sym, Qty: 1, MarketValue: v}
		snap.Invested += v
	}

	snap.Equity = snap.Cash + snap.Invested

	return snap
}

func rowFor(t *testing.T, rows []allocationRow, class string) allocationRow {
	t.Helper()

	for _, r := range rows {
		if r.Class == class {
			return r
		}
	}

	t.Fatalf("no row for class %q in %+v", class, rows)

	return allocationRow{}
}

// The panel must agree with the limit it displays, or it is worse than no
// panel: same equity denominator, same arithmetic as
// risk.PortfolioGuard.checkAssetClass.
func TestAllocationMatchesTheGuardsArithmetic(t *testing.T) {
	snap := allocSnapshot(600, map[string]float64{"IWDA": 250, "VWCE": 50, "SAP.DE": 100})
	classes := map[string]string{"IWDA": "etf", "VWCE": "etf", "SAP.DE": "stock"}
	caps := map[string]float64{"etf": 0.30, "stock": 0.70}

	rows := allocation(snap, classes, caps)

	etf := rowFor(t, rows, "etf")
	if etf.Positions != 2 || etf.Value != 300 {
		t.Fatalf("etf row = %+v, want 2 positions worth 300", etf)
	}

	// 300 of 1000 equity = 30%, exactly at a 30% cap.
	if etf.PctEquity != 30 || etf.CapPct != 30 {
		t.Errorf("etf share/cap = %v/%v, want 30/30", etf.PctEquity, etf.CapPct)
	}

	if !etf.AtCap {
		t.Error("etf sitting exactly on its cap did not report at-cap")
	}

	if etf.HeadroomPct != 0 {
		t.Errorf("etf headroom = %v, want 0", etf.HeadroomPct)
	}

	stock := rowFor(t, rows, "stock")
	if stock.PctEquity != 10 || stock.HeadroomPct != 60 {
		t.Errorf("stock = %v%% with %v%% headroom, want 10/60", stock.PctEquity, stock.HeadroomPct)
	}

	if stock.AtCap {
		t.Error("stock at 10% of a 70% cap reported at-cap")
	}
}

// A configured but empty class must still be listed: "0 of 30% used" is exactly
// what someone deciding what to buy next needs.
func TestAllocationListsEmptyCappedClasses(t *testing.T) {
	rows := allocation(allocSnapshot(1000, nil), nil, map[string]float64{"etf": 0.3})

	etf := rowFor(t, rows, "etf")
	if etf.Positions != 0 || !etf.Capped || etf.HeadroomPct != 30 {
		t.Errorf("empty capped class = %+v, want 0 positions and full headroom", etf)
	}
}

// With no cap named for it, an unclassified holding counts against nothing —
// worth seeing, since it is exposure the limits cannot constrain.
func TestAllocationSurfacesUnclassifiedHoldings(t *testing.T) {
	snap := allocSnapshot(500, map[string]float64{"MYSTERY": 500})

	rows := allocation(snap, nil, map[string]float64{"etf": 0.3})

	unclassified := rowFor(t, rows, "")
	if unclassified.Positions != 1 || unclassified.PctEquity != 50 {
		t.Errorf("unclassified row = %+v, want 1 position at 50%%", unclassified)
	}

	if unclassified.Capped {
		t.Error("an unclassified holding reported as capped with no cap configured")
	}
}

// The guard budgets unclassified holdings under a reserved key; the panel must
// look the cap up the same way, or it reports "no limit" on the one bucket that
// has one — and the panel is where an operator checks before a buy is refused.
func TestAllocationShowsTheUnclassifiedCap(t *testing.T) {
	snap := allocSnapshot(500, map[string]float64{"MYSTERY": 250, "IWDA": 250})

	rows := allocation(snap, map[string]string{"IWDA": "etf"}, map[string]float64{
		"etf": 0.3, config.UnclassifiedAssetClass: 0.4,
	})

	// One row, keyed on "" so the view can name it in the reader's language.
	if got := rowsWithClass(rows, config.UnclassifiedAssetClass); got != 0 {
		t.Errorf("%d row(s) keyed on the sentinel; the view would print it raw", got)
	}

	unclassified := rowFor(t, rows, "")
	if !unclassified.Capped || unclassified.CapPct != 40 {
		t.Fatalf("unclassified row = %+v, want a 40%% cap", unclassified)
	}

	// 250 of 1000 = 25%, so 15 points of headroom and not yet at the cap.
	if unclassified.HeadroomPct != 15 || unclassified.AtCap {
		t.Errorf("unclassified headroom = %v, at-cap = %v, want 15 and false",
			unclassified.HeadroomPct, unclassified.AtCap)
	}
}

// A cap on a bucket holding nothing is still listed, on the same single row its
// holdings would land on.
func TestAllocationListsAnEmptyUnclassifiedCap(t *testing.T) {
	rows := allocation(allocSnapshot(1000, nil), nil,
		map[string]float64{config.UnclassifiedAssetClass: 0.1})

	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want exactly one", rows)
	}

	if row := rowFor(t, rows, ""); !row.Capped || row.HeadroomPct != 10 {
		t.Errorf("empty unclassified budget = %+v, want full 10%% headroom", row)
	}
}

// rowsWithClass counts the rows carrying a given class label.
func rowsWithClass(rows []allocationRow, class string) int {
	n := 0

	for _, r := range rows {
		if r.Class == class {
			n++
		}
	}

	return n
}

// A flat row is not a position, matching the guard — counting it would inflate
// a class and refuse a legitimate re-entry.
func TestAllocationIgnoresClosedPositions(t *testing.T) {
	snap := allocSnapshot(1000, nil)
	snap.Holdings["IWDA"] = portfolio.Holding{Symbol: "IWDA", Qty: 0, MarketValue: 900}

	etf := rowFor(t, allocation(snap, map[string]string{"IWDA": "etf"},
		map[string]float64{"etf": 0.3}), "etf")

	if etf.Positions != 0 || etf.Value != 0 {
		t.Errorf("flat position counted: %+v", etf)
	}
}

// Capped classes sort first — they are the ones that can block a buy.
func TestAllocationSortsCappedClassesFirst(t *testing.T) {
	snap := allocSnapshot(100, map[string]float64{"A": 500, "B": 100})
	classes := map[string]string{"A": "crypto", "B": "etf"}

	rows := allocation(snap, classes, map[string]float64{"etf": 0.3})

	if rows[0].Class != "etf" {
		t.Errorf("first row = %q, want the capped class first despite lower value", rows[0].Class)
	}
}

// An empty account must not divide by zero or invent a share.
func TestAllocationHandlesAnEmptyAccount(t *testing.T) {
	rows := allocation(portfolio.Snapshot{Holdings: map[string]portfolio.Holding{}},
		nil, map[string]float64{"etf": 0.3})

	etf := rowFor(t, rows, "etf")
	if etf.PctEquity != 0 {
		t.Errorf("share on an empty account = %v, want 0", etf.PctEquity)
	}
}

// The panel reaches the page with its values, and an at-cap class says so.
func TestAllocationPanelRenders(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/v2/f/positions", nil)

	snap := allocSnapshot(600, map[string]float64{"IWDA": 300, "SAP.DE": 100})

	render(c, "positions", map[string]any{
		"Holdings": nil, "Tracked": nil, "Drift": nil,
		"Allocation": allocation(snap,
			map[string]string{"IWDA": "etf", "SAP.DE": "stock"},
			map[string]float64{"etf": 0.30, "stock": 0.70}),
	})

	body := w.Body.String()

	for _, want := range []string{"etf", "stock", "30.00 %", "10.00 %", "at cap", "60.00 %"} {
		if !strings.Contains(body, want) {
			t.Errorf("allocation panel is missing %q", want)
		}
	}

	if strings.Contains(body, "{") && strings.Contains(body, "}") &&
		strings.Contains(body, "{of}") {
		t.Error("page contains an unrendered placeholder")
	}
}

// TestAllocationBucketsAHandWrittenClassMapLikeTheGuard is the regression for
// the panel and the guard disagreeing about what a symbol's class IS, rather
// than about the arithmetic applied afterwards.
//
// config.yaml's asset_classes: block is hand-written, so it arrives however
// the operator typed it. risk.PortfolioGuard canonicalises it at construction
// (symbols upper-cased, labels lower-cased) and the caps are keyed lower-cased
// too; the panel used the raw map with a bare strings.ToUpper on the symbol
// and the label verbatim. So "spy: ETF" landed in an "ETF" row — a key no cap
// is written under — and the panel reported "no limit" beside a capped "etf"
// row reading 0% used, while the guard was refusing buys against exactly that
// cap. The panel is only worth having if it says what the limit says.
func TestAllocationBucketsAHandWrittenClassMapLikeTheGuard(t *testing.T) {
	snap := allocSnapshot(700, map[string]float64{"SPY": 200, "QQQ": 100})

	// Spelled the way a person writes YAML, not the way the bot stores it:
	// lower-case symbol, capitalised label, a stray space.
	classes := map[string]string{"spy": "ETF", " QQQ ": "Etf"}
	caps := config.PortfolioConfig{
		MaxPctPerAssetClass: map[string]float64{"ETF": 0.30},
	}.NormalizedAssetClassCaps()

	rows := allocation(snap, classes, caps)

	if n := rowsWithClass(rows, "ETF"); n != 0 {
		t.Errorf("panel produced %d uncanonical \"ETF\" row(s); the caps are keyed \"etf\"", n)
	}

	etf := rowFor(t, rows, "etf")
	if etf.Positions != 2 || etf.Value != 300 {
		t.Fatalf("etf row = %+v, want both holdings (2 positions worth 300)", etf)
	}

	if !etf.Capped || etf.CapPct != 30 {
		t.Errorf("etf row = %+v, want it to show the configured 30%% cap", etf)
	}

	// 300 of 1000 equity is exactly the 30% cap, so the guard refuses the next
	// buy. The panel must be saying so at the same moment.
	if !etf.AtCap {
		t.Error("panel reports headroom in a class the guard has already closed")
	}

	guard := risk.NewPortfolioGuard(
		config.PortfolioConfig{
			Enabled:             true,
			MaxPctPerAssetClass: map[string]float64{"ETF": 0.30},
		},
		nil, classes, nil,
	)

	if ok, _ := guard.AllowEntry("spy", snap, 1); ok {
		t.Fatal("guard allowed a buy at the cap; the fixture no longer pins the disagreement")
	}
}
