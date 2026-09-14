package engine

import (
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
)

// config.yaml's sectors: block is the explicit override layer — an
// auto-fetched sector must never overwrite one already present, whether it
// came from config or an earlier auto-fetch.
func TestSetAutoSectorNeverOverridesAKnownSector(t *testing.T) {
	t.Parallel()

	e := nameEngine(&nameStore{}, nil)
	e.sectors["SAP.DE"] = "tech" // e.g. from config.yaml

	e.setAutoSector("SAP.DE", "Financial Services")

	e.mu.RLock()
	got := e.sectors["SAP.DE"]
	e.mu.RUnlock()

	if got != "tech" {
		t.Errorf("sector = %q, want the original %q preserved", got, "tech")
	}
}

// A blank sector (funds/ETFs, or anything Yahoo can't classify) must not be
// recorded as a real (empty-string) answer — that would permanently mark the
// symbol "known" and stop it from ever being reconsidered.
func TestSetAutoSectorIgnoresABlankSector(t *testing.T) {
	t.Parallel()

	e := nameEngine(&nameStore{}, nil)

	e.setAutoSector("XMUS.DE", "")

	if e.hasSector("XMUS.DE") {
		t.Error("a blank sector was recorded as known")
	}
}

// The auto-learned cache survives a restart, same guarantee names already
// have — and config.yaml still wins over whatever was persisted, since the
// merge in restoreSectors always applies config.yaml's contribution last.
func TestSectorsSurviveARestartAndConfigStillWins(t *testing.T) {
	t.Parallel()

	st := &nameStore{}
	first := nameEngine(st, nil)
	first.portCfg = config.PortfolioConfig{Enabled: true}
	first.setAutoSector("MSFT", "Technology")

	if st.sectorWrites == 0 {
		t.Fatal("setAutoSector did not persist anything")
	}

	// A fresh engine, as if the process restarted, sharing only the store —
	// this time config.yaml already has an opinion on MSFT that disagrees
	// with what was auto-learned before the restart.
	second := nameEngine(st, nil)
	second.portCfg = config.PortfolioConfig{Enabled: true}
	second.sectors = map[string]string{"MSFT": "software"} // config.yaml's own entry

	second.restoreSectors()

	_, sectors, _ := second.PortfolioConfig()
	if sectors["MSFT"] != "software" {
		t.Errorf("MSFT = %q after restore, want config.yaml's %q to win over the persisted auto-fetch",
			sectors["MSFT"], "software")
	}
}

// The whole point: an auto-learned sector must actually change what the
// portfolio guard decides, not just sit in a map nobody reads. One
// max_positions_per_sector slot, one existing "technology" holding — a new
// candidate is unconstrained until it has a sector, then blocked once
// setAutoSector gives it the same one.
func TestSetAutoSectorRebuildsTheGuardsDecision(t *testing.T) {
	t.Parallel()

	e := nameEngine(&nameStore{}, nil)
	e.portCfg = config.PortfolioConfig{Enabled: true, MaxPositionsPerSector: 1}
	e.sectors = map[string]string{"AAPL": "technology"}
	e.SetPortfolioGuard(risk.NewPortfolioGuard(e.portCfg, e.sectors, e.assetClasses, nil))

	snap := portfolio.Snapshot{Holdings: map[string]portfolio.Holding{
		"AAPL": {Symbol: "AAPL", Qty: 1, MarketValue: 100},
	}}

	if ok, reason := e.portfolioGuard().AllowEntry("MSFT", snap, 100); !ok {
		t.Fatalf("MSFT should be unconstrained before it has a sector label: %s", reason)
	}

	e.setAutoSector("MSFT", "Technology") // normalizes to "technology", same bucket as AAPL

	ok, reason := e.portfolioGuard().AllowEntry("MSFT", snap, 100)
	if ok {
		t.Error("MSFT should now be blocked — one technology slot is already full")
	}

	if reason == "" {
		t.Error("a blocked entry should carry a reason")
	}
}
