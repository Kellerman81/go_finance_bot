package config

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// TestPatchWritesAChangedListInEitherStyle is the regression for CF2-1. The
// list settings had no ",flow" tag, so a changed list marshalled as a block
// ("- adx\n- trend") and was spliced over the file's list as if it were a
// scalar: `detectors: - adx` on one line plus the old items. verify refused
// the result, so every save of a changed detector set, screener universe or
// filter chain, notify event list or scale-out ladder came back "applied, but
// not saved" — whether the file wrote the list inline or one item per line —
// and the next restart reverted it.
func TestPatchWritesAChangedListInEitherStyle(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"inline": "strategy:\n  detectors: [ema_cross, rsi]   # the buy set\n  fast_ma: 12\n  slow_ma: 26\n",
		"block":  "strategy:\n  detectors:\n    - ema_cross   # trend\n    - rsi\n  fast_ma: 12\n  slow_ma: 26\n",
	} {
		path := configFile(t, body)

		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("%s: load: %v", name, err)
		}

		cfg.Strategy.Detectors = []string{"adx", "trend", "macd"}

		if err := Patch(path, "strategy", cfg.Strategy); err != nil {
			t.Fatalf("%s: saving a changed detector list failed: %v", name, err)
		}

		reloaded, err := Load(path)
		if err != nil {
			raw, _ := os.ReadFile(path)
			t.Fatalf("%s: reload: %v\n%s", name, err, raw)
		}

		if !slices.Equal(reloaded.Strategy.Detectors, []string{"adx", "trend", "macd"}) {
			t.Errorf("%s: detectors after a save = %v", name, reloaded.Strategy.Detectors)
		}

		if reloaded.Strategy.FastMA != 12 || reloaded.Strategy.SlowMA != 26 {
			t.Errorf("%s: the neighbouring keys were disturbed: %+v", name, reloaded.Strategy)
		}
	}
}

// TestPatchWritesAChangedScaleOutLadder covers the list of structs the same
// defect hit: exits.partial_take_profit, written one rung per block.
func TestPatchWritesAChangedScaleOutLadder(t *testing.T) {
	t.Parallel()

	path := configFile(t, "exits:\n  enabled: true\n  partial_take_profit:\n    - profit_pct: 0.05\n      sell_pct: 0.5\n  stop_loss_pct: 0.05\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	cfg.Exits.PartialTakeProfit = []PartialExit{{ProfitPct: 0.04, SellPct: 0.3}, {ProfitPct: 0.08, SellPct: 0.3}}

	if err := Patch(path, "exits", cfg.Exits); err != nil {
		t.Fatalf("saving a changed ladder failed: %v", err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if len(reloaded.Exits.PartialTakeProfit) != 2 || reloaded.Exits.StopLossPct != 0.05 {
		t.Errorf("after a save: ladder %+v, stop %v", reloaded.Exits.PartialTakeProfit, reloaded.Exits.StopLossPct)
	}
}

// TestPatchRemovesADeletedMapKey is the regression for CF2-5. Patch only ever
// overwrote or inserted leaves, and verify checked only the leaves the value
// HAS — so deleting the ROI rung "30m" changed the live engine, the file
// kept it, the UI said saved, and the rung came back on the next restart. The
// same held for strategy weights and the per-class caps and slots.
func TestPatchRemovesADeletedMapKey(t *testing.T) {
	t.Parallel()

	path := configFile(t, `exits:
  enabled: true
  # the time-decaying profit ladder
  roi:
    "0s": 0.10
    "30m": 0.05   # this rung goes
    "2h": 0.02
  stop_loss_pct: 0.05
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	delete(cfg.Exits.ROI, "30m")

	if err := Patch(path, "exits", cfg.Exits); err != nil {
		t.Fatalf("saving a removed rung failed: %v", err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, kept := reloaded.Exits.ROI["30m"]; kept || len(reloaded.Exits.ROI) != 2 {
		t.Errorf("roi after removing 30m = %v", reloaded.Exits.ROI)
	}

	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "# the time-decaying profit ladder") {
		t.Errorf("the save dropped an unrelated comment:\n%s", raw)
	}
}

// TestPatchRefusesAFileThatWouldNotLoad is the regression for AP2-4. Each
// settings handler validated its own section, but whether the bot STARTS
// depends on rules that span sections: with trading_enabled: true in the
// file and the kill switch off at runtime, PUT /api/limits accepted zeroed
// money ceilings (ValidateForTrading only runs while trading is on), saved
// them, and the next restart refused to load — no bot, so no exits.
func TestPatchRefusesAFileThatWouldNotLoad(t *testing.T) {
	t.Parallel()

	path := configFile(t, `engine:
  trading_enabled: true
limits:
  max_order_value: 500
  max_per_position: 1000
  max_total_invested: 5000
  max_daily_spend: 2000
  max_open_positions: 5
`)

	if _, err := Load(path); err != nil {
		t.Fatalf("the starting file must load: %v", err)
	}

	before, _ := os.ReadFile(path)

	if err := Patch(path, "limits", Limits{}); err == nil {
		t.Error("zeroing every money ceiling under trading_enabled: true was written")
	}

	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Error("the file was changed by a refused save")
	}
}
