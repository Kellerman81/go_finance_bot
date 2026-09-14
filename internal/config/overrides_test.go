package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes a config file and loads it.
func writeConfig(t *testing.T, body string) (Config, error) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return Load(path)
}

// TestApplyStrategyOverrideMatchesTheFileLoadedEquivalent reproduces the
// need behind ApplyStrategyOverride: the scheduled auto-optimizer computes
// a SymbolOverrideFields and needs to hand a running Engine the resulting
// StrategyConfig immediately, without a restart — that resolution must be
// identical to what loading the SAME fields from a hand-written
// strategy_overrides: block via config.Load would produce, or a
// live-applied override and a restarted one would silently disagree.
func TestApplyStrategyOverrideMatchesTheFileLoadedEquivalent(t *testing.T) {
	t.Parallel()

	fromFile, err := writeConfig(t, `
strategy:
  detectors: ["ema_cross", "adx", "supertrend"]
  combine: "weighted"
  min_strength: 0.30
  rsi_period: 14
  fast_ma: 14
  slow_ma: 26
  trend_gate: true
strategy_overrides:
  MSFT:
    detectors: ["ema_cross", "rsi", "macd"]
    combine: "unanimous"
    min_strength: 0.20
`)
	if err != nil {
		t.Fatal(err)
	}

	ms := 0.20
	live, err := ApplyStrategyOverride(fromFile.Strategy, SymbolOverrideFields{
		Detectors: []string{"ema_cross", "rsi", "macd"}, Combine: "unanimous", MinStrength: &ms,
	})
	if err != nil {
		t.Fatalf("ApplyStrategyOverride: %v", err)
	}

	fileMSFT := fromFile.PerSymbolStrategy("MSFT")

	if live.Combine != fileMSFT.Combine || live.MinStrength != fileMSFT.MinStrength ||
		len(live.Detectors) != len(fileMSFT.Detectors) {
		t.Errorf("live-applied = %+v, want it to match the file-loaded equivalent %+v", live, fileMSFT)
	}

	// Inherited, not just the named fields — the whole point of layering
	// onto a copy of base rather than replacing it outright.
	if live.RSIPeriod != fromFile.Strategy.RSIPeriod || live.TrendGate != fromFile.Strategy.TrendGate {
		t.Errorf("unnamed fields were not inherited from base: %+v", live)
	}
}

// TestApplyStrategyOverrideCanExplicitlyRevertSellSideToBuy reproduces the
// AT-4 finding: a search can decide a symbol's sell side should go back to
// "same as buy" — internal/backtest/refine.go's sellSide axis offers nil
// DetectorsSell / "" CombineSell as an explicit candidate labelled "same as
// buy". Before DetectorsSell/CombineSell became pointers, SymbolOverrideFields
// had no way to represent "explicitly set to empty" versus "not decided at
// all": a plain []string/string with `omitempty` drops a nil/"" value
// exactly like an absent field, so applying it silently kept whatever
// sell-side override the symbol already had instead of clearing it.
func TestApplyStrategyOverrideCanExplicitlyRevertSellSideToBuy(t *testing.T) {
	t.Parallel()

	base := StrategyConfig{
		Detectors: []string{"ema_cross"}, Combine: "majority", MinStrength: 0.2,
		DetectorsSell: []string{"rsi", "macd"}, CombineSell: "weighted",
	}

	emptyDS := []string{}
	emptyCS := ""

	live, err := ApplyStrategyOverride(base, SymbolOverrideFields{
		Detectors: []string{"ema_cross"}, Combine: "majority",
		DetectorsSell: &emptyDS, CombineSell: &emptyCS,
	})
	if err != nil {
		t.Fatalf("ApplyStrategyOverride: %v", err)
	}

	if len(live.DetectorsSell) != 0 {
		t.Errorf("DetectorsSell = %v, want cleared (same-as-buy) — the explicit revert was dropped", live.DetectorsSell)
	}

	if live.CombineSell != "" {
		t.Errorf("CombineSell = %q, want cleared (same-as-buy) — the explicit revert was dropped", live.CombineSell)
	}

	// A genuinely unset field (nil pointer) must still inherit base, so the
	// pointer change didn't also break the ordinary "field not named" case.
	live2, err := ApplyStrategyOverride(base, SymbolOverrideFields{
		Detectors: []string{"ema_cross"}, Combine: "majority",
	})
	if err != nil {
		t.Fatalf("ApplyStrategyOverride: %v", err)
	}

	if !strings.Contains(strings.Join(live2.DetectorsSell, ","), "rsi") || live2.CombineSell != "weighted" {
		t.Errorf("unnamed DetectorsSell/CombineSell = %+v, want inherited from base", live2)
	}
}

// TestOverrideInheritsUnnamedFields is the property that makes overrides usable:
// naming one setting must not zero every other one. Without inheritance, an
// override of `min_strength` alone would leave that symbol with rsi_period 0, no
// detectors and a broken MACD — trading on a config nobody wrote.
func TestOverrideInheritsUnnamedFields(t *testing.T) {
	t.Parallel()

	cfg, err := writeConfig(t, `
strategy:
  detectors: ["rsi", "macd"]
  min_strength: 0.25
  rsi_period: 14
  fast_ma: 12
  slow_ma: 26
strategy_overrides:
  NVDA:
    min_strength: 0.45
`)
	if err != nil {
		t.Fatal(err)
	}

	nvda := cfg.PerSymbolStrategy("NVDA")

	if nvda.MinStrength != 0.45 {
		t.Errorf("min_strength = %v, want the override's 0.45", nvda.MinStrength)
	}

	if nvda.RSIPeriod != 14 {
		t.Errorf("rsi_period = %d, want the inherited 14 — an unnamed field was zeroed",
			nvda.RSIPeriod)
	}

	if len(nvda.Detectors) != 2 {
		t.Errorf("detectors = %v, want the inherited [rsi macd]", nvda.Detectors)
	}

	if nvda.FastMA != 12 || nvda.SlowMA != 26 {
		t.Errorf("MAs = %d/%d, want the inherited 12/26", nvda.FastMA, nvda.SlowMA)
	}

	// The base is untouched, and a symbol without an override gets it.
	if cfg.Strategy.MinStrength != 0.25 {
		t.Errorf("base min_strength = %v, want 0.25 — the override leaked into the base",
			cfg.Strategy.MinStrength)
	}

	if got := cfg.PerSymbolStrategy("AAPL").MinStrength; got != 0.25 {
		t.Errorf("AAPL min_strength = %v, want the base 0.25", got)
	}
}

// TestOverrideCanReplaceDetectors checks a list field is replaced wholesale
// rather than merged — the useful semantics for "trade this symbol differently".
func TestOverrideCanReplaceDetectors(t *testing.T) {
	t.Parallel()

	cfg, err := writeConfig(t, `
strategy:
  detectors: ["rsi", "macd", "vwap"]
  fast_ma: 12
  slow_ma: 26
strategy_overrides:
  IBCI.DE:
    detectors: ["trend"]
    min_strength: 0.5
`)
	if err != nil {
		t.Fatal(err)
	}

	ibci := cfg.PerSymbolStrategy("IBCI.DE")
	if len(ibci.Detectors) != 1 || ibci.Detectors[0] != "trend" {
		t.Errorf("detectors = %v, want exactly [trend]", ibci.Detectors)
	}
}

// TestOverrideSymbolsAreCaseInsensitive checks lookups match however the config
// and the watchlist were capitalised.
func TestOverrideSymbolsAreCaseInsensitive(t *testing.T) {
	t.Parallel()

	cfg, err := writeConfig(t, `
strategy:
  fast_ma: 12
  slow_ma: 26
strategy_overrides:
  nvda:
    min_strength: 0.4
`)
	if err != nil {
		t.Fatal(err)
	}

	for _, sym := range []string{"NVDA", "nvda", " Nvda "} {
		if got := cfg.PerSymbolStrategy(sym).MinStrength; got != 0.4 {
			t.Errorf("PerSymbolStrategy(%q).MinStrength = %v, want 0.4", sym, got)
		}
	}

	if !cfg.HasOverride("NVDA") || cfg.HasOverride("AAPL") {
		t.Error("HasOverride disagrees with PerSymbolStrategy")
	}
}

// TestInvalidOverrideIsRejected checks a bad override fails the load naming the
// symbol, rather than leaving one symbol quietly broken.
func TestInvalidOverrideIsRejected(t *testing.T) {
	t.Parallel()

	_, err := writeConfig(t, `
strategy:
  fast_ma: 12
  slow_ma: 26
strategy_overrides:
  NVDA:
    fast_ma: 50
`)
	if err == nil {
		t.Fatal("an override with fast_ma >= slow_ma was accepted")
	}

	if !strings.Contains(err.Error(), "NVDA") {
		t.Errorf("error = %v, want it to name the offending symbol", err)
	}

	if _, err := writeConfig(t, `
strategy:
  fast_ma: 12
  slow_ma: 26
strategy_overrides:
  NVDA:
    min_strength: 5
`); err == nil {
		t.Error("an out-of-range min_strength was accepted")
	}
}

// TestNoOverridesIsNil checks the common case costs nothing.
func TestNoOverridesIsNil(t *testing.T) {
	t.Parallel()

	cfg, err := writeConfig(t, "strategy:\n  fast_ma: 12\n  slow_ma: 26\n")
	if err != nil {
		t.Fatal(err)
	}

	if cfg.SymbolStrategy != nil {
		t.Errorf("SymbolStrategy = %v, want nil with no overrides", cfg.SymbolStrategy)
	}

	if len(cfg.OverriddenSymbols()) != 0 {
		t.Error("OverriddenSymbols is not empty")
	}

	// And the accessor still works, returning the base.
	if got := cfg.PerSymbolStrategy("AAPL"); got.FastMA != 12 {
		t.Errorf("PerSymbolStrategy fell back wrongly: %+v", got)
	}
}

// TestOverridesLayerOnTopOfAProfile checks the resolution order end to end: a
// profile seeds the base, the file's strategy block overrides that, and a
// per-symbol override overrides both.
func TestOverridesLayerOnTopOfAProfile(t *testing.T) {
	t.Parallel()

	cfg, err := writeConfig(t, `
profile: conservative
strategy:
  min_strength: 0.20
strategy_overrides:
  NVDA:
    min_strength: 0.60
`)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Strategy.MinStrength != 0.20 {
		t.Errorf("base min_strength = %v, want the file's 0.20 over the profile's",
			cfg.Strategy.MinStrength)
	}

	if got := cfg.PerSymbolStrategy("NVDA").MinStrength; got != 0.60 {
		t.Errorf("NVDA min_strength = %v, want the override's 0.60", got)
	}

	// NVDA still inherits the profile's confirm-heavy behaviour elsewhere.
	if cfg.Engine.ConfirmBuy != 3 {
		t.Errorf("confirm_buy = %d, want the conservative profile's 3", cfg.Engine.ConfirmBuy)
	}
}
