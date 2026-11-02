package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// AT3-2 / CF3-3: an overridden symbol's live config is a resolved snapshot.
// A shared-strategy save used to replace only the shared strategy, so AAPL
// kept trading on the OLD rsi_period while the file resolved it onto the
// new one — autotune backtested the stale base and a restart applied a
// config no gate ever saw. The save must re-layer every override.
func TestStrategySaveReLayersTheOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "strategy:\n  fast_ma: 12\n  slow_ma: 26\n  rsi_period: 14\n" +
		"strategy_overrides:\n  AAPL:\n    combine: unanimous\n"

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t)
	s.SetConfigPath(path)
	s.eng.SetStrategyConfig(cfg.Strategy)
	s.eng.SetSymbolStrategies(cfg.SymbolStrategy)

	if code, out := do(t, s, "PUT", "/api/strategy", `{"rsi_period": 10}`); code != 200 {
		t.Fatalf("PUT /api/strategy = %d: %s", code, out)
	}

	live := s.eng.StrategyConfigFor("AAPL")
	if live.RSIPeriod != 10 || live.Combine != "unanimous" {
		t.Errorf("AAPL live = rsi %d / %q, want rsi 10 (the new base) / unanimous (its override)",
			live.RSIPeriod, live.Combine)
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if got := reloaded.PerSymbolStrategy("AAPL").RSIPeriod; got != live.RSIPeriod {
		t.Errorf("after a restart AAPL trades rsi %d, live trades %d: file and process disagree",
			got, live.RSIPeriod)
	}
}

// The other side of the re-layer: an override the new base would make
// invalid (fast_ma 30 over a base slow_ma of 20) is refused at save, rather
// than written into a file the next restart cannot load.
func TestStrategySaveRefusedWhenAnOverrideWouldBreak(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "strategy:\n  fast_ma: 12\n  slow_ma: 40\n" +
		"strategy_overrides:\n  AAPL:\n    fast_ma: 30\n"

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t)
	s.SetConfigPath(path)
	s.eng.SetStrategyConfig(cfg.Strategy)
	s.eng.SetSymbolStrategies(cfg.SymbolStrategy)

	if code, out := do(t, s, "PUT", "/api/strategy", `{"slow_ma": 20}`); code != 400 {
		t.Errorf("PUT slow_ma 20 under an AAPL fast_ma 30 override = %d (%s), want 400", code, out)
	}

	if got := s.eng.StrategyConfig().SlowMA; got != 40 {
		t.Errorf("the refused save still applied: slow_ma = %d", got)
	}
}
