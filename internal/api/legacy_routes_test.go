package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// AP3-7: the legacy PUT /api/exits decoded its body into an empty struct, so
// a partial body — {"stop_loss_pct":0.04} — switched exits off and zeroed the
// trail, take-profit, ladder and max_hold, live and in config.yaml. A key the
// body omits keeps its value, as on the settings section.
func TestAPartialExitsPutKeepsWhatItDoesNotSend(t *testing.T) {
	s := newTestServer(t)
	s.eng.SetExitsConfig(config.ExitsConfig{Enabled: true, StopLossPct: 0.05, TrailingStopPct: 0.08})

	if code, body := do(t, s, http.MethodPut, "/api/exits", `{"stop_loss_pct":0.04}`); code != http.StatusOK {
		t.Fatalf("PUT /api/exits = %d: %s", code, body)
	}

	x := s.eng.ExitsConfig()
	if !x.Enabled || x.TrailingStopPct != 0.08 || x.StopLossPct != 0.04 {
		t.Errorf("after a partial PUT exits = enabled %v, trail %v, stop %v; want true, 0.08, 0.04",
			x.Enabled, x.TrailingStopPct, x.StopLossPct)
	}
}

// AP3-3: the portfolio form edits the sector and asset-class maps, which
// nothing wrote to config.yaml — a correction applied live, the form said
// "saved", a restart reverted it. Symbols carry dots (SAP.DE), which the
// dotted-path patcher could not address. The hand-written maps reach the file.
func TestAPortfolioSaveWritesTheLabelMaps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	seed := "# The comments are half the value of this file.\nportfolio:\n  enabled: false\n"

	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t)
	s.SetConfigPath(path)

	body := `{"enabled": false, "sectors": {"SAP.DE": "technology"}, "asset_classes": {"SPY": "bond"}}`
	if code, out := do(t, s, http.MethodPut, "/api/settings/portfolio", body); code != http.StatusOK {
		t.Fatalf("PUT portfolio = %d: %s", code, out)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Sectors["SAP.DE"] != "technology" || cfg.AssetClasses["SPY"] != "bond" {
		t.Errorf("after a restart sectors=%v asset_classes=%v; the form's edits did not survive",
			cfg.Sectors, cfg.AssetClasses)
	}

	if !strings.Contains(readFile(t, path), "The comments are half the value") {
		t.Error("rewriting the label maps lost the rest of the file's comments")
	}
}

// AP3-5: the cross-section slot rule ran only when the file was written, so a
// portfolio save claiming more per-class slots than engine.min_positions —
// or a sizing save lowering min_positions under them — was applied LIVE and
// answered "applied, but not saved": the engine ran a split the loader calls
// impossible, and a restart undid it. Both are refused before applying.
func TestASlotSplitTheLoaderRefusesIsNotAppliedLive(t *testing.T) {
	s := newTestServer(t)
	s.eng.SetSizing(4, 0, 0)

	body := `{"enabled": true, "min_positions_per_asset_class": {"etf": 3, "stock": 3}}`
	if code, out := do(t, s, http.MethodPut, "/api/settings/portfolio", body); code == http.StatusOK {
		t.Errorf("a 6-slot split under min_positions 4 was accepted: %s", out)
	}

	if port, _, _ := s.eng.PortfolioConfig(); len(port.MinPositionsPerAssetClass) != 0 {
		t.Errorf("the refused split is live: %v", port.MinPositionsPerAssetClass)
	}

	s.eng.SetPortfolioConfig(config.PortfolioConfig{Enabled: true,
		MinPositionsPerAssetClass: map[string]int{"etf": 2, "stock": 2}}, nil, nil)

	if code, out := do(t, s, http.MethodPut, "/api/sizing", `{"min_positions": 2}`); code == http.StatusOK {
		t.Errorf("min_positions 2 under a 4-slot split was accepted: %s", out)
	}

	if minPos, _, _ := s.eng.Sizing(); minPos != 4 {
		t.Errorf("the refused min_positions is live: %d", minPos)
	}
}

// AP3-5, the limits half: the ceilings were checked against the LIVE trading
// switch only. With trading toggled off live but trading_enabled: true in the
// file, zeroed ceilings were applied and only the write refused — the file's
// switch is the one the loader checks.
func TestZeroedCeilingsAreRefusedWhileTheFileTrades(t *testing.T) {
	cfg := config.Default()
	cfg.Engine.TradingEnabled = true
	cfg.Limits.MaxOrderValue, cfg.Limits.MaxTotalInvested = 1000, 5000

	s := newTestServerFrom(t, nil, cfg)
	s.eng.SetTradingEnabled(false) // toggled off live; the file still trades

	if code, out := do(t, s, http.MethodPut, "/api/limits", `{"max_order_value": 0, "max_total_invested": 0}`); code == http.StatusOK {
		t.Errorf("zeroed ceilings were accepted while the configured switch trades: %s", out)
	}
}
