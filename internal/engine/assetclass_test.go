package engine

import (
	"context"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
)

// The provider's instrument type is what makes the allocation caps work with
// no hand-maintained map, and it must arrive in the canonical spelling the
// caps are keyed by — "ETF" from Yahoo has to land as "etf".
func TestResolveNamesLearnsAssetClassFromSearch(t *testing.T) {
	t.Parallel()

	search := &fakeSearcher{
		table: map[string]string{"IWDA.AS": "iShares Core MSCI World"},
		types: map[string]string{"IWDA.AS": "ETF"},
	}
	e := nameEngine(&nameStore{}, search)
	e.portCfg = config.PortfolioConfig{Enabled: true}

	e.resolveNames(context.Background(), []string{"IWDA.AS"})

	if got := e.AssetClasses()["IWDA.AS"]; got != "etf" {
		t.Errorf("asset class = %q, want %q (learned and normalised)", got, "etf")
	}
}

// config.yaml's asset_classes: block is the explicit override layer — an
// auto-classification must never overwrite one already present, whether it
// came from config or an earlier pass.
func TestSetAutoAssetClassNeverOverridesAKnownClass(t *testing.T) {
	t.Parallel()

	e := nameEngine(&nameStore{}, nil)
	e.assetClasses["BRK-B"] = "stock" // e.g. from config.yaml

	e.setAutoAssetClass("BRK-B", "fund")

	if got := e.AssetClasses()["BRK-B"]; got != "stock" {
		t.Errorf("asset class = %q, want the original %q preserved", got, "stock")
	}
}

// A symbol nothing could classify must not be recorded as an empty-string
// answer: that would permanently mark it "known" and stop it from ever being
// reconsidered, while leaving it outside every allocation cap forever.
func TestSetAutoAssetClassIgnoresABlankClass(t *testing.T) {
	t.Parallel()

	e := nameEngine(&nameStore{}, nil)

	e.setAutoAssetClass("PRAJL.XC", "")

	if e.hasAssetClass("PRAJL.XC") {
		t.Error("a blank asset class was recorded as known")
	}
}

// The auto-learned cache survives a restart, and config.yaml still wins over
// whatever was persisted — the same two-layer guarantee sectors have.
func TestAssetClassesSurviveARestartAndConfigStillWins(t *testing.T) {
	t.Parallel()

	st := &nameStore{}
	first := nameEngine(st, nil)
	first.portCfg = config.PortfolioConfig{Enabled: true}
	first.setAutoAssetClass("XEON.DE", "fund")

	if st.classWrites == 0 {
		t.Fatal("setAutoAssetClass did not persist anything")
	}

	// A fresh engine, as if the process restarted, sharing only the store —
	// config.yaml disagrees with what was classified before the restart.
	second := nameEngine(st, nil)
	// config.yaml's own entry, applied the way startup applies it — the
	// hand-written layer is held apart from the learned one (SG2-2).
	second.SetPortfolioConfig(config.PortfolioConfig{Enabled: true}, nil, map[string]string{"XEON.DE": "etf"})

	second.restoreAssetClasses()

	_, _, classes := second.PortfolioConfig()
	if classes["XEON.DE"] != "etf" {
		t.Errorf("XEON.DE = %q after restore, want config.yaml's %q to win over the "+
			"persisted classification", classes["XEON.DE"], "etf")
	}
}

// The whole point: a learned class must actually change what the portfolio
// guard decides, not just sit in a map nobody reads.
func TestAutoAssetClassReachesTheGuard(t *testing.T) {
	t.Parallel()

	e := nameEngine(&nameStore{}, nil)
	e.portCfg = config.PortfolioConfig{
		Enabled:             true,
		MaxPctPerAssetClass: map[string]float64{"etf": 0.30},
	}
	e.SetPortfolioGuard(risk.NewPortfolioGuard(e.portCfg, e.sectors, e.assetClasses, nil))

	// Equity 1000, 250 of it already in a (so far unclassified) ETF.
	snap := portfolio.Snapshot{
		Cash: 750, Equity: 1000, Invested: 250,
		Holdings: map[string]portfolio.Holding{
			"IWDA.AS": {Symbol: "IWDA.AS", Qty: 1, MarketValue: 250},
		},
	}

	// Nothing is classified yet, so nothing counts against the ETF budget.
	if ok, reason := e.portfolioGuard().AllowEntry("VWCE.DE", snap, 200); !ok {
		t.Fatalf("an unclassified book was already blocked: %s", reason)
	}

	e.setAutoAssetClass("IWDA.AS", "etf")
	e.setAutoAssetClass("VWCE.DE", "etf")

	// 250 + 200 = 450 > 300 (30% of 1000).
	if ok, _ := e.portfolioGuard().AllowEntry("VWCE.DE", snap, 200); ok {
		t.Error("the rebuilt guard ignored the newly learned asset classes")
	}
}
