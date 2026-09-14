package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A config written for the previous sizing model must still load. order_size_usd
// and risk_per_trade_pct were removed; the operator's live file still carries
// them, and refusing to start over a key that no longer exists would take the
// bot down on upgrade rather than on any real misconfiguration.
func TestStaleSizingKeysDoNotBlockLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	body := "engine:\n  order_size_usd: 0\n  risk_per_trade_pct: 0.001\n  min_positions: 0\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := Load(path); err != nil {
		t.Fatalf("a config carrying the removed sizing keys failed to load: %v", err)
	}
}
