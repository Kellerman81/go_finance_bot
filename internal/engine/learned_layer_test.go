package engine

import (
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
)

// SG2-2: the "auto-learned" store used to persist the live map, i.e. config.yaml
// ∪ learned. An operator who wrote `asset_classes: {FOO: bond}` by mistake and
// then deleted the line found FOO still bucketed as a bond after the next
// restart — the hand-written entry had been saved as a learned one, and
// because a known symbol is never re-classified, nothing would ever correct it.
// Only what the bot learned may reach the store.
func TestRemovedConfigEntryDoesNotReturnFromTheStore(t *testing.T) {
	t.Parallel()

	st := &nameStore{}
	first := nameEngine(st, nil)
	first.SetPortfolioConfig(config.PortfolioConfig{Enabled: true},
		map[string]string{"FOO": "bonds"}, map[string]string{"FOO": "bond"})
	// Any learn triggers a persist of the cache.
	first.setAutoAssetClass("IWDA.AS", "etf")
	first.setAutoSector("SAP.DE", "technology")

	// Restart with the FOO lines deleted from config.yaml.
	second := nameEngine(st, nil)
	second.SetPortfolioConfig(config.PortfolioConfig{Enabled: true}, nil, nil)
	second.restoreAssetClasses()
	second.restoreSectors()

	_, sectors, classes := second.PortfolioConfig()
	if got, ok := classes["FOO"]; ok {
		t.Errorf("FOO came back as %q from the learned cache after its config entry was removed", got)
	}

	if got, ok := sectors["FOO"]; ok {
		t.Errorf("FOO's sector came back as %q after its config entry was removed", got)
	}

	if classes["IWDA.AS"] != "etf" || sectors["SAP.DE"] != "technology" {
		t.Errorf("genuinely learned entries were lost across the restart: classes=%v sectors=%v",
			classes, sectors)
	}
}

// The other half of SG2-2: a portfolio settings save replaces the hand-written
// maps wholesale. It used to replace the LIVE maps, so every learned entry
// vanished from memory and the next persist erased them from the store too —
// every symbol unbucketed until the classifier re-learned it.
func TestPortfolioSaveKeepsLearnedEntries(t *testing.T) {
	t.Parallel()

	e := nameEngine(&nameStore{}, nil)
	e.SetPortfolioConfig(config.PortfolioConfig{Enabled: true}, nil, nil)
	e.setAutoAssetClass("IWDA.AS", "etf")
	e.setAutoSector("SAP.DE", "technology")

	// What the API's portfolio PUT sends: the operator's own maps only.
	e.SetPortfolioConfig(config.PortfolioConfig{Enabled: true},
		map[string]string{"BAYN.DE": "health"}, map[string]string{"BRK-B": "stock"})

	_, sectors, classes := e.PortfolioConfig()
	if classes["IWDA.AS"] != "etf" || sectors["SAP.DE"] != "technology" {
		t.Errorf("a portfolio save dropped the learned layer: classes=%v sectors=%v", classes, sectors)
	}

	if classes["BRK-B"] != "stock" || sectors["BAYN.DE"] != "health" {
		t.Errorf("the saved hand-written entries did not apply: classes=%v sectors=%v", classes, sectors)
	}

	hs, hc := e.PortfolioOverrides()
	if _, ok := hc["IWDA.AS"]; ok {
		t.Error("PortfolioOverrides reports a learned class as hand-written; the form would write it into config.yaml")
	}

	if _, ok := hs["SAP.DE"]; ok {
		t.Error("PortfolioOverrides reports a learned sector as hand-written")
	}
}

// EN3-6: a hand-written key in another case ("sap.de") was stored as typed, so
// hasSector's upper-cased lookup missed it, a learned "SAP.DE" label landed
// beside it, and NormalizeLabels — used by the guard and the panel — kept
// whichever came last in map order: the hand-written label lost about one
// rebuild in nine. Hand-written must always win.
func TestAHandLabelInAnotherCaseAlwaysWins(t *testing.T) {
	e, _ := forgetEngine()
	e.SetPortfolioConfig(config.PortfolioConfig{}, map[string]string{"sap.de": "tech"}, nil)

	if !e.hasSector("SAP.DE") {
		t.Error("the hand-written sap.de entry is invisible to the SAP.DE lookup")
	}

	e.setAutoSector("SAP.DE", "Industrials")

	for range 200 {
		e.mu.RLock()
		got := risk.NormalizeLabels(e.sectors)["SAP.DE"]
		e.mu.RUnlock()

		if got != "tech" {
			t.Fatalf("SAP.DE resolved to %q, want the hand-written %q", got, "tech")
		}
	}
}

// ML3-4: a strategy binds the ML predictor when it is built, and an ML
// settings save rebuilt only the shared strategy — every overridden symbol
// kept the old predictor, and voted with it after ML was switched off.
// RebuildStrategies rebuilds the overrides too.
func TestRebuildStrategiesRebuildsTheOverrides(t *testing.T) {
	e, _ := forgetEngine()
	e.SetSymbolStrategies(map[string]config.StrategyConfig{"AAPL": config.Default().Strategy})

	before := e.strategyFor("AAPL")

	e.RebuildStrategies()

	if e.strategyFor("AAPL") == before {
		t.Error("the override strategy was not rebuilt; it keeps the predictor it was built with")
	}
}
