package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestProfilesAreCoherent checks each profile's settings move together in the
// direction its name promises. A "conservative" profile that trades more freely
// than the aggressive one is worse than no profile at all.
func TestProfilesAreCoherent(t *testing.T) {
	t.Parallel()

	cons, err := ApplyProfile(Default(), "conservative")
	if err != nil {
		t.Fatal(err)
	}

	aggr, err := ApplyProfile(Default(), "aggressive")
	if err != nil {
		t.Fatal(err)
	}

	if cons.Strategy.MinStrength <= aggr.Strategy.MinStrength {
		t.Errorf(
			"conservative min_strength %.2f <= aggressive %.2f; it should demand more conviction",
			cons.Strategy.MinStrength,
			aggr.Strategy.MinStrength,
		)
	}

	if cons.Engine.ConfirmBuy <= aggr.Engine.ConfirmBuy {
		t.Errorf("conservative confirm_buy %d <= aggressive %d",
			cons.Engine.ConfirmBuy, aggr.Engine.ConfirmBuy)
	}

	// min_positions is the conservative/aggressive sizing axis now that risk
	// scaling is unconditional: more positions means a smaller share of the
	// book in each.
	if cons.Engine.MinPositions <= aggr.Engine.MinPositions {
		t.Errorf("conservative spreads over %d positions, aggressive %d; that is backwards",
			cons.Engine.MinPositions, aggr.Engine.MinPositions)
	}

	if cons.Protections.MaxDrawdownPct >= aggr.Protections.MaxDrawdownPct {
		t.Errorf("conservative tolerates %.2f drawdown, aggressive %.2f; that is backwards",
			cons.Protections.MaxDrawdownPct, aggr.Protections.MaxDrawdownPct)
	}

	if cons.Exits.StopLossPct >= aggr.Exits.StopLossPct {
		t.Errorf("conservative stop %.3f is not tighter than aggressive %.3f",
			cons.Exits.StopLossPct, aggr.Exits.StopLossPct)
	}

	// Every profile must leave the protections on: they are the backstop, not a
	// style choice.
	for name, c := range map[string]Config{"conservative": cons, "aggressive": aggr} {
		if !c.Protections.Enabled {
			t.Errorf("%s profile disabled protections", name)
		}

		if !c.Exits.Enabled {
			t.Errorf("%s profile disabled protective exits", name)
		}
	}
}

// TestUnknownProfileIsRejected checks a typo fails loudly instead of silently
// trading someone else's settings.
func TestUnknownProfileIsRejected(t *testing.T) {
	t.Parallel()

	if _, err := ApplyProfile(Default(), "agressive"); err == nil {
		t.Error("a misspelled profile was accepted")
	}

	if _, err := ApplyProfile(Default(), ""); err != nil {
		t.Errorf("an empty profile should be a no-op, got %v", err)
	}

	if !KnownProfile("") || !KnownProfile("Balanced") || KnownProfile("nope") {
		t.Error("KnownProfile disagrees with ApplyProfile")
	}
}

// TestExplicitSettingsBeatTheProfile checks the layering order: a profile seeds
// the config, and anything written in the file overrides it. Getting this
// backwards would silently ignore the user's own settings.
func TestExplicitSettingsBeatTheProfile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	// The conservative profile sets min_strength 0.45 and confirm_buy 3; this
	// file overrides one of them and leaves the other alone.
	body := `
profile: conservative
strategy:
  min_strength: 0.05
engine:
  eval_interval: 30s
`

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Strategy.MinStrength != 0.05 {
		t.Errorf("min_strength = %v, want the file's explicit 0.05, not the profile's",
			cfg.Strategy.MinStrength)
	}

	if cfg.Engine.ConfirmBuy != 3 {
		t.Errorf("confirm_buy = %d, want the profile's 3 (the file did not set it)",
			cfg.Engine.ConfirmBuy)
	}

	if cfg.Protections.CooldownAfterLoss != 2*time.Hour {
		t.Errorf("cooldown = %v, want the conservative profile's 2h",
			cfg.Protections.CooldownAfterLoss)
	}
}

// TestBadProfileInFileIsAnError checks a bad profile name in the config file
// stops the load rather than being ignored.
func TestBadProfileInFileIsAnError(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("profile: yolo\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); err == nil {
		t.Error("Load accepted an unknown profile")
	}
}

// TestFillPriceIsSymmetric checks the cost model documented on CostsConfig.
func TestFillPriceIsSymmetric(t *testing.T) {
	t.Parallel()

	c := CostsConfig{SpreadPct: 0.002, SlippagePct: 0.001} // 20 bps adverse

	buy, sell := c.FillPrice(true, 100), c.FillPrice(false, 100)
	if buy <= 100 || sell >= 100 {
		t.Errorf("buy %v / sell %v, want the buy above and the sell below 100", buy, sell)
	}

	if diff := (buy - 100) - (100 - sell); diff > 1e-9 || diff < -1e-9 {
		t.Errorf("adverse move is asymmetric: buy +%v, sell -%v", buy-100, 100-sell)
	}
}

// TestAIValidateOnlyWhenEnabled checks a half-filled but disabled AI block does
// not stop the bot booting.
func TestAIValidateOnlyWhenEnabled(t *testing.T) {
	t.Parallel()

	if err := (AIConfig{Enabled: false, Provider: "nonsense"}).Validate(); err != nil {
		t.Errorf("a disabled AI block was validated: %v", err)
	}

	if err := (AIConfig{Enabled: true, BaseURL: "http://x"}).Validate(); err == nil {
		t.Error("an enabled AI block with no model passed validation")
	}
}
