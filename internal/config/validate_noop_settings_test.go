package config

import (
	"strings"
	"testing"
	"time"
)

// TestValidateRejectsSettingsThatCannotWork collects the LR2 findings where a
// setting loaded cleanly and then did nothing — the "silent no-op is a
// defect" rule. Each case names the finding it guards and the failure it
// produced; every one is checked only while its block is active, so
// config.example.yaml's staged-off values still load.
func TestValidateRejectsSettingsThatCannotWork(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{
			// EX2-6: only "0" parsed, so a 10% target never decayed.
			"roi minute keys", func(c *Config) { c.Exits.ROI = map[string]float64{"0": 0.10, "30": 0.05} },
			"exits.roi key",
		},
		{
			// PR2-5: 20 meant as 20% could never halt.
			"drawdown as percent", func(c *Config) { c.Protections.Enabled = true; c.Protections.MaxDrawdownPct = 20 },
			"max_drawdown_pct",
		},
		{
			"realized drawdown as percent", func(c *Config) {
				c.Protections.Enabled = true
				c.Protections.MaxRealizedDrawdownPct = 15
			},
			"max_realized_drawdown_pct",
		},
		{
			// PR2-4: trips, locks until "now", blocks nothing.
			"low-profit guard with no lock", func(c *Config) {
				c.Protections.Enabled = true
				c.Protections.LowProfitTrades = 4
				c.Protections.LowProfitLookback = 24 * time.Hour
				c.Protections.LowProfitLock = 0
			},
			"low_profit_trades",
		},
		{
			"stop guard with no lock", func(c *Config) {
				c.Protections.Enabled = true
				c.Protections.StopGuardTrades = 3
				c.Protections.StopGuardLock = 0
			},
			"stop_guard_trades",
		},
		{
			// NT2-7: a typo subscribed to nothing.
			"notify event typo", func(c *Config) { c.Notify.Enabled = true; c.Notify.Events = []string{"trades", "halts"} },
			"notify.events",
		},
		{
			// AI2-3: 15 meant as percent turned every verdict into HOLD.
			"ai confidence as percent", func(c *Config) {
				c.AI.Enabled, c.AI.Model, c.AI.BaseURL = true, "m", "http://x"
				c.AI.MinConfidence = 15
			},
			"ai.min_confidence",
		},
		{
			// AI2-4: a negative cap was silently replaced by full strength.
			"ai negative strength", func(c *Config) {
				c.AI.Enabled, c.AI.Model, c.AI.BaseURL = true, "m", "http://x"
				c.AI.MaxStrength = -1
			},
			"ai.max_strength",
		},
		{
			// ML2-4: 0.35/0 runs as 0.35/0.42 — crossed.
			"ml crossed after defaults", func(c *Config) { c.ML.Enabled = true; c.ML.BuyProb, c.ML.SellProb = 0.35, 0 },
			"ml.sell_prob",
		},
		{
			// CF2-8: 20 meant as 20% never binds.
			"screener spread as percent", func(c *Config) { c.Screener.MaxSpreadPct = 20 },
			"max_spread_pct",
		},
		{
			// RK2-6: a negative cost inflates every headroom figure.
			"negative commission", func(c *Config) { c.Costs.CommissionPct = -0.01 },
			"costs.commission_pct",
		},
		{
			"commission as percent", func(c *Config) { c.Costs.CommissionPct = 1.5 },
			"costs.commission_pct",
		},
	}

	for _, tc := range cases {
		cfg := Default()
		tc.edit(&cfg)

		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate = %v, want an error naming %q", tc.name, err, tc.want)
		}
	}
}

// TestValidateAcceptsMLDefaultsAsTheyRun is ML2-4's other half: buy 0 / sell
// 0.5 runs as a valid 0.58/0.5 and was wrongly refused.
func TestValidateAcceptsMLDefaultsAsTheyRun(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.ML.Enabled = true
	cfg.ML.BuyProb, cfg.ML.SellProb = 0, 0.5

	if err := cfg.Validate(); err != nil {
		t.Errorf("buy_prob 0 (= 0.58) with sell_prob 0.5 refused: %v", err)
	}
}
