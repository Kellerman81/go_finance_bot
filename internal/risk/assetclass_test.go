package risk

import (
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
)

// classSnapshot builds a snapshot with an explicit cash balance, so the
// equity denominator the allocation cap divides by is exactly known.
func classSnapshot(cash float64, holdings map[string]float64) portfolio.Snapshot {
	snap := snapshotOf(holdings)
	snap.Cash = cash
	snap.Equity = cash + snap.Invested

	return snap
}

// etfCapConfig is a 30%-of-equity cap on ETFs and nothing else, so a guard
// built from it exercises the allocation check in isolation.
func etfCapConfig() config.PortfolioConfig {
	return config.PortfolioConfig{
		Enabled:             true,
		MaxPctPerAssetClass: map[string]float64{"etf": 0.30},
	}
}

var classMap = map[string]string{
	"IWDA": "etf", "VWCE": "etf", "SAP.DE": "stock",
}

// A class cap alone must build a guard: it is the only limit an operator
// enabling just an allocation budget has configured.
func TestAssetClassCapAloneBuildsGuard(t *testing.T) {
	t.Parallel()

	if g := NewPortfolioGuard(etfCapConfig(), nil, classMap, nil); g == nil {
		t.Fatal("guard not built for an enabled asset-class cap")
	}

	// A cap of zero is "no cap", not "cap everything" — it must not build a
	// guard that pays for a check on every order and blocks nothing.
	zero := config.PortfolioConfig{
		Enabled:             true,
		MaxPctPerAssetClass: map[string]float64{"etf": 0},
	}
	if g := NewPortfolioGuard(zero, nil, classMap, nil); g != nil {
		t.Error("a zero cap built a guard")
	}
}

// The cap is measured against equity (cash + holdings), and an order that
// would take the class past its share is refused.
func TestAssetClassCapBlocksPastTheBudget(t *testing.T) {
	t.Parallel()

	g := NewPortfolioGuard(etfCapConfig(), nil, classMap, nil)

	// Equity 1000, ETFs already 250. 250 + 100 = 350 > 300 (30%).
	snap := classSnapshot(750, map[string]float64{"IWDA": 250})

	ok, reason := g.AllowEntry("VWCE", snap, 100)
	if ok {
		t.Fatal("an entry past the asset-class budget was allowed")
	}

	for _, want := range []string{"etf", "35.0%", "max_pct_per_asset_class"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q does not mention %q", reason, want)
		}
	}

	// 250 + 50 = 300, exactly the cap: allowed. A budget is a ceiling, not a
	// strict inequality, or no order could ever fill it exactly.
	if ok, reason := g.AllowEntry("VWCE", snap, 50); !ok {
		t.Errorf("an entry landing exactly on the cap was blocked: %s", reason)
	}
}

// Buying a class that is already over its cap is refused even when the order
// itself is tiny — the point of the limit is to stop adding, not to meter.
func TestAssetClassCapBlocksWhenAlreadyOver(t *testing.T) {
	t.Parallel()

	g := NewPortfolioGuard(etfCapConfig(), nil, classMap, nil)
	snap := classSnapshot(100, map[string]float64{"IWDA": 900})

	if ok, _ := g.AllowEntry("VWCE", snap, 1); ok {
		t.Error("a buy was allowed into a class already past its cap")
	}
}

// Topping up a position already held spends the same budget as a new name, so
// unlike the sector/correlation caps the allocation cap must still apply.
// This is the case the scale-in path hits.
func TestAssetClassCapAppliesToTopUps(t *testing.T) {
	t.Parallel()

	g := NewPortfolioGuard(etfCapConfig(), nil, classMap, nil)
	snap := classSnapshot(600, map[string]float64{"IWDA": 400})

	if ok, _ := g.AllowEntry("IWDA", snap, 100); ok {
		t.Error("topping up a held position was allowed past the asset-class cap")
	}

	// The sector cap still exempts a top-up — the two behave differently on
	// purpose, and changing one must not silently change the other.
	sector := NewPortfolioGuard(config.PortfolioConfig{
		Enabled: true, MaxPerSector: 100,
	}, map[string]string{"IWDA": "funds"}, classMap, nil)

	if ok, reason := sector.AllowEntry("IWDA", snap, 100); !ok {
		t.Errorf("the sector cap blocked a top-up: %s", reason)
	}
}

// A class under its cap, a class with no cap, and a symbol nothing could
// classify all pass — an allocation budget only ever constrains what it names.
func TestAssetClassCapLetsEverythingElseThrough(t *testing.T) {
	t.Parallel()

	g := NewPortfolioGuard(etfCapConfig(), nil, classMap, nil)
	snap := classSnapshot(750, map[string]float64{"IWDA": 250})

	if ok, reason := g.AllowEntry("VWCE", snap, 20); !ok {
		t.Errorf("an entry inside the budget was blocked: %s", reason)
	}

	// "stock" has no cap configured, so a 900 stock order on a 1000 book is
	// the operator's business, not the guard's.
	if ok, reason := g.AllowEntry("SAP.DE", snap, 900); !ok {
		t.Errorf("an uncapped class was blocked: %s", reason)
	}

	// An unclassified symbol counts against the unclassified bucket, which
	// this config does not name — so it passes, exactly like the uncapped
	// "stock" above. Guessing a class instead would charge it to a budget it
	// may not belong to.
	if ok, reason := g.AllowEntry("MYSTERY", snap, 900); !ok {
		t.Errorf("an unclassified symbol was blocked with no unclassified cap: %s", reason)
	}
}

// The unclassified bucket is cappable like any other class. Without this, the
// one position the bot understands least is the only one no allocation limit
// can bind, and capping it means noticing it in the UI and hand-writing an
// asset_classes: entry per symbol.
func TestUnclassifiedBucketCap(t *testing.T) {
	t.Parallel()

	g := NewPortfolioGuard(config.PortfolioConfig{
		Enabled: true,
		MaxPctPerAssetClass: map[string]float64{
			"etf": 0.30, config.UnclassifiedAssetClass: 0.10,
		},
	}, nil, classMap, nil)

	// Equity 1000, nothing unclassified held yet: 120 > 100 (10%).
	snap := classSnapshot(750, map[string]float64{"IWDA": 250})

	ok, reason := g.AllowEntry("MYSTERY", snap, 120)
	if ok {
		t.Fatal("an unclassified buy past its budget was allowed")
	}

	for _, want := range []string{config.UnclassifiedAssetClass, "12.0%", "max_pct_per_asset_class"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q does not mention %q", reason, want)
		}
	}

	// Inside the budget it is an ordinary buy.
	if ok, reason := g.AllowEntry("MYSTERY", snap, 80); !ok {
		t.Errorf("an unclassified buy inside its budget was blocked: %s", reason)
	}

	// A named class is judged against its own budget, not this one: 250 ETFs
	// held plus 40 is under 30%, even though 40 would breach unclassified.
	if ok, reason := g.AllowEntry("VWCE", snap, 40); !ok {
		t.Errorf("a classified symbol was charged to the unclassified bucket: %s", reason)
	}
}

// The bucket is the sum of every unclassified holding, not just the candidate's
// own line — a budget counted per symbol would let ten unknown positions each
// take the whole allowance.
func TestUnclassifiedBucketSumsEveryUnknownHolding(t *testing.T) {
	t.Parallel()

	g := NewPortfolioGuard(config.PortfolioConfig{
		Enabled:             true,
		MaxPctPerAssetClass: map[string]float64{config.UnclassifiedAssetClass: 0.20},
	}, nil, classMap, nil)

	// Equity 1000: 90 + 90 unclassified already held, 20 more breaches 200.
	snap := classSnapshot(620, map[string]float64{
		"NTA.F": 90, "MYSTERY": 90, "IWDA": 200,
	})

	if ok, _ := g.AllowEntry("OTHER", snap, 30); ok {
		t.Error("unclassified holdings were budgeted per symbol, not as one bucket")
	}

	// The classified holding is not swept into the bucket: 180 + 15 < 200.
	if ok, reason := g.AllowEntry("OTHER", snap, 15); !ok {
		t.Errorf("a classified holding counted against the unclassified bucket: %s", reason)
	}
}

// A cap on the unclassified bucket alone must build a guard, and must bind even
// when nothing in the book is classified at all — that is the account the
// setting exists for.
func TestUnclassifiedCapAloneBuildsGuard(t *testing.T) {
	t.Parallel()

	g := NewPortfolioGuard(config.PortfolioConfig{
		Enabled:             true,
		MaxPctPerAssetClass: map[string]float64{config.UnclassifiedAssetClass: 0.25},
	}, nil, nil, nil)

	if g == nil {
		t.Fatal("guard not built for an unclassified-only cap")
	}

	snap := classSnapshot(700, map[string]float64{"NTA.F": 300})

	if ok, _ := g.AllowEntry("NTA.F", snap, 10); ok {
		t.Error("an empty class map left the unclassified cap unenforced")
	}
}

// Config files are written by hand and providers disagree on case: a cap on
// "ETF" must bind the symbols classified as "etf".
func TestAssetClassCapNormalizesCaseAndWhitespace(t *testing.T) {
	t.Parallel()

	g := NewPortfolioGuard(config.PortfolioConfig{
		Enabled:             true,
		MaxPctPerAssetClass: map[string]float64{"  ETF ": 0.30},
	}, nil, map[string]string{"iwda": " ETF ", "VWCE": "Etf"}, nil)

	snap := classSnapshot(750, map[string]float64{"IWDA": 250})

	if ok, _ := g.AllowEntry("vwce", snap, 100); ok {
		t.Error("a differently-cased class escaped its cap")
	}
}

// Equity is the denominator, and a snapshot that only filled Cash/Invested
// must still be capped — leaving it unenforced while the book is being built
// would skip the cap exactly when the allocation is being decided.
func TestAssetClassCapReconstructsEquity(t *testing.T) {
	t.Parallel()

	g := NewPortfolioGuard(etfCapConfig(), nil, classMap, nil)

	snap := snapshotOf(map[string]float64{"IWDA": 250}) // Equity left at 0
	snap.Cash = 750

	if ok, _ := g.AllowEntry("VWCE", snap, 100); ok {
		t.Error("a snapshot with no derived Equity skipped the asset-class cap")
	}

	// Nothing at all to take a percentage of: allow rather than refuse every
	// order on an empty account.
	empty := portfolio.Snapshot{Holdings: map[string]portfolio.Holding{}}
	if ok, reason := g.AllowEntry("VWCE", empty, 100); !ok {
		t.Errorf("an empty account blocked an entry: %s", reason)
	}
}

// A closed position (Qty 0) still carries a MarketValue in some broker
// snapshots; counting it would charge the budget for exposure that no longer
// exists and refuse a legitimate re-entry.
func TestAssetClassCapIgnoresClosedPositions(t *testing.T) {
	t.Parallel()

	g := NewPortfolioGuard(etfCapConfig(), nil, classMap, nil)

	snap := classSnapshot(1000, nil)
	snap.Holdings["IWDA"] = portfolio.Holding{Symbol: "IWDA", Qty: 0, MarketValue: 900}

	if ok, reason := g.AllowEntry("VWCE", snap, 250); !ok {
		t.Errorf("a flat position counted against the budget: %s", reason)
	}
}

// AssetClass exposes what the guard believes, including through a nil guard,
// so the UI can explain a block without a special case.
func TestGuardAssetClassLookup(t *testing.T) {
	t.Parallel()

	g := NewPortfolioGuard(etfCapConfig(), nil, classMap, nil)

	if got := g.AssetClass(" iwda "); got != "etf" {
		t.Errorf("AssetClass = %q, want %q", got, "etf")
	}

	if got := g.AssetClass("MYSTERY"); got != "" {
		t.Errorf("AssetClass of an unclassified symbol = %q, want empty", got)
	}

	var nilGuard *PortfolioGuard
	if got := nilGuard.AssetClass("IWDA"); got != "" {
		t.Errorf("nil guard AssetClass = %q, want empty", got)
	}
}

// The question this answers: is a tech ETF the same unit of "tech exposure" as
// a tech stock? Off (the default) says yes — they fall together. On says no —
// a sector ETF holds dozens of companies, so one unit of it is not one unit of
// single-name risk, and MaxPctPerAssetClass already bounds each wrapper.
func TestSectorCapsPerAssetClass(t *testing.T) {
	t.Parallel()

	sectors := map[string]string{"AAPL": "tech", "MSFT": "tech", "SEC0.DE": "tech"}
	classes := map[string]string{"AAPL": "stock", "MSFT": "stock", "SEC0.DE": "etf"}

	// One tech stock and one tech ETF already held.
	snap := classSnapshot(600, map[string]float64{"AAPL": 200, "SEC0.DE": 200})

	shared := NewPortfolioGuard(config.PortfolioConfig{
		Enabled: true, MaxPositionsPerSector: 2,
	}, sectors, classes, nil)

	// Counted across the book, tech is full at two.
	if ok, reason := shared.AllowEntry("MSFT", snap, 100); ok {
		t.Error("a third tech position was allowed under a shared cap of 2")
	} else if !strings.Contains(reason, "tech") {
		t.Errorf("reason %q does not name the sector", reason)
	}

	perClass := NewPortfolioGuard(config.PortfolioConfig{
		Enabled: true, MaxPositionsPerSector: 2, SectorCapsPerAssetClass: true,
	}, sectors, classes, nil)

	// Counted per class, "stock tech" holds only AAPL, so there is room.
	if ok, reason := perClass.AllowEntry("MSFT", snap, 100); !ok {
		t.Errorf("a second tech STOCK was refused with per-class counting: %s", reason)
	}
}

// Per-class counting must still bind within a class — it separates the buckets,
// it does not remove the cap.
func TestSectorCapsPerAssetClassStillBindWithinAClass(t *testing.T) {
	t.Parallel()

	sectors := map[string]string{"AAPL": "tech", "MSFT": "tech", "NVDA": "tech"}
	classes := map[string]string{"AAPL": "stock", "MSFT": "stock", "NVDA": "stock"}

	g := NewPortfolioGuard(config.PortfolioConfig{
		Enabled: true, MaxPositionsPerSector: 2, SectorCapsPerAssetClass: true,
	}, sectors, classes, nil)

	snap := classSnapshot(600, map[string]float64{"AAPL": 200, "MSFT": 200})

	ok, reason := g.AllowEntry("NVDA", snap, 100)
	if ok {
		t.Fatal("a third tech STOCK was allowed under a per-class cap of 2")
	}

	// The refusal names the bucket it was counted in, so the operator can tell
	// which of the two filled up.
	if !strings.Contains(reason, "stock tech") {
		t.Errorf("reason %q does not name the class+sector bucket", reason)
	}
}

// The value cap follows the same rule, not just the position count.
func TestSectorValueCapAlsoSplitsPerAssetClass(t *testing.T) {
	t.Parallel()

	sectors := map[string]string{"AAPL": "tech", "SEC0.DE": "tech"}
	classes := map[string]string{"AAPL": "stock", "SEC0.DE": "etf"}

	snap := classSnapshot(600, map[string]float64{"SEC0.DE": 400})

	shared := NewPortfolioGuard(config.PortfolioConfig{
		Enabled: true, MaxPerSector: 500,
	}, sectors, classes, nil)

	if ok, _ := shared.AllowEntry("AAPL", snap, 200); ok {
		t.Error("400 + 200 was allowed past a shared max_per_sector of 500")
	}

	perClass := NewPortfolioGuard(config.PortfolioConfig{
		Enabled: true, MaxPerSector: 500, SectorCapsPerAssetClass: true,
	}, sectors, classes, nil)

	if ok, reason := perClass.AllowEntry("AAPL", snap, 200); !ok {
		t.Errorf("the ETF's value was counted against the stock bucket: %s", reason)
	}
}

// An unclassified candidate has no class bucket to sit in; it is counted
// against other unclassified holdings in the sector and says so.
func TestSectorCapsPerAssetClassWithAnUnclassifiedSymbol(t *testing.T) {
	t.Parallel()

	g := NewPortfolioGuard(config.PortfolioConfig{
		Enabled: true, MaxPositionsPerSector: 1, SectorCapsPerAssetClass: true,
	}, map[string]string{"A": "tech", "B": "tech"}, map[string]string{"A": "stock"}, nil)

	// B is unclassified; A is a stock, so B's bucket is empty and it passes.
	snap := classSnapshot(600, map[string]float64{"A": 200})
	if ok, reason := g.AllowEntry("B", snap, 100); !ok {
		t.Errorf("an unclassified candidate was blocked by a stock's slot: %s", reason)
	}

	// Two unclassified tech names DO compete with each other.
	g2 := NewPortfolioGuard(config.PortfolioConfig{
		Enabled: true, MaxPositionsPerSector: 1, SectorCapsPerAssetClass: true,
	}, map[string]string{"A": "tech", "B": "tech"}, nil, nil)

	ok, reason := g2.AllowEntry("B", classSnapshot(600, map[string]float64{"A": 200}), 100)
	if ok {
		t.Error("two unclassified tech names ignored the cap")
	}

	if !strings.Contains(reason, "unclassified") {
		t.Errorf("reason %q does not flag the unclassified bucket", reason)
	}
}
