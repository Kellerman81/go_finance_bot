package config

import (
	"fmt"
	"strings"

	"github.com/goccy/go-yaml"
)

// PerSymbolStrategy returns the strategy configuration for symbol: the override
// if one exists, otherwise the shared base.
//
// One set of parameters for a volatile small cap and a short-duration bond ETF is
// a compromise that suits neither. An RSI period tuned on NVDA says nothing
// useful about IBCI.DE, and a 14-bar ATR breakout on an instrument that moves 5
// bps a day fires on noise.
func (c Config) PerSymbolStrategy(symbol string) StrategyConfig {
	if sc, ok := c.SymbolStrategy[strings.ToUpper(strings.TrimSpace(symbol))]; ok {
		return sc
	}

	return c.Strategy
}

// HasOverride reports whether symbol has its own strategy parameters.
func (c Config) HasOverride(symbol string) bool {
	_, ok := c.SymbolStrategy[strings.ToUpper(strings.TrimSpace(symbol))]

	return ok
}

// OverriddenSymbols lists the symbols with their own parameters.
func (c Config) OverriddenSymbols() []string {
	out := make([]string, 0, len(c.SymbolStrategy))
	for sym := range c.SymbolStrategy {
		out = append(out, sym)
	}

	return out
}

// resolveSymbolStrategies expands the `strategy_overrides` block into a full
// StrategyConfig per symbol.
//
// The subtlety is inheritance. An override that names only `min_strength` must
// keep every other parameter from the base — otherwise writing one field would
// silently zero the RSI period, the MACD lengths and the detector list, and the
// symbol would trade on a config nobody wrote. That is achieved by unmarshalling
// each override *over a copy of the resolved base*: YAML only assigns the fields
// present in the document, so everything unmentioned is inherited.
func resolveSymbolStrategies(data []byte, base StrategyConfig) (map[string]StrategyConfig, error) {
	var doc struct {
		Overrides map[string]map[string]any `yaml:"strategy_overrides"`
	}

	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse strategy_overrides: %w", err)
	}

	if len(doc.Overrides) == 0 {
		return nil, nil
	}

	out := make(map[string]StrategyConfig, len(doc.Overrides))
	// Which raw key produced each normalized symbol, so a second key naming
	// the same symbol can be reported with both spellings.
	seen := make(map[string]string, len(doc.Overrides))

	for symbol, fields := range doc.Overrides {
		sym := strings.ToUpper(strings.TrimSpace(symbol))
		if sym == "" || len(fields) == 0 {
			continue
		}

		// Two keys that differ only in case/whitespace collapse to one symbol
		// here, and a map has no order — so silently keeping one meant the
		// config a symbol traded under was decided by map iteration and could
		// differ between restarts of the same binary on the same file
		// (measured: 171/29 across 200 loads). Refuse instead: it is a
		// genuinely ambiguous file, and an operator can only fix it by
		// deciding which block they meant. See CODE_REVIEW_FINDINGS.md's
		// EN-2 finding, and PatchSymbolOverride's existingOverrideKey, which
		// stops this package from ever writing the second key itself.
		if prior, dup := seen[sym]; dup {
			return nil, fmt.Errorf(
				"strategy_overrides has two entries for %s (%q and %q); "+
					"symbol keys are matched case-insensitively, so keep only one",
				sym, prior, symbol,
			)
		}

		seen[sym] = symbol

		merged, err := layerOntoBase(base, fields)
		if err != nil {
			return nil, fmt.Errorf("strategy_overrides[%s]: %w", symbol, err)
		}

		if err := validateStrategy(merged, symbol); err != nil {
			return nil, err
		}

		out[sym] = merged
	}

	return out, nil
}

// layerOntoBase marshals fields (any value whose yaml tags name
// StrategyConfig fields — a map[string]any fragment from a parsed file, or
// a SymbolOverrideFields struct) and unmarshals the result over a COPY of
// base, so only the fields fields actually names are changed and everything
// else is inherited. Shared by resolveSymbolStrategies (load time, from
// file bytes) and ApplyStrategyOverride (runtime, from a just-computed
// SymbolOverrideFields) so both produce the identical resolved config for
// the same fields — a scheduler-applied override behaves exactly like a
// hand-written one, live and after a restart alike.
func layerOntoBase(base StrategyConfig, fields any) (StrategyConfig, error) {
	body, err := yaml.Marshal(fields)
	if err != nil {
		return base, err
	}

	merged := base
	if err := yaml.Unmarshal(body, &merged); err != nil {
		return base, err
	}

	return merged, nil
}

// ApplyStrategyOverride layers fields onto a copy of base — the runtime
// equivalent of what resolveSymbolStrategies does when config.Load parses a
// strategy_overrides entry from a file, for a caller (the scheduled
// auto-optimizer) that has just computed SymbolOverrideFields and needs the
// resulting StrategyConfig to hand a running Engine immediately, without
// waiting for a restart to re-read the file it also just wrote via
// PatchSymbolOverride.
func ApplyStrategyOverride(base StrategyConfig, fields SymbolOverrideFields) (StrategyConfig, error) {
	return layerOntoBase(base, fields)
}

// validateStrategy applies the per-strategy invariants to an override, so a bad
// one is reported at load time naming the symbol rather than producing a
// silently broken strategy for that symbol alone.
func validateStrategy(sc StrategyConfig, symbol string) error {
	if sc.FastMA >= sc.SlowMA {
		return fmt.Errorf("strategy_overrides[%s]: fast_ma (%d) must be < slow_ma (%d)",
			symbol, sc.FastMA, sc.SlowMA)
	}

	if sc.MinStrength < 0 || sc.MinStrength > 1 {
		return fmt.Errorf("strategy_overrides[%s]: min_strength %.2f must be in 0..1",
			symbol, sc.MinStrength)
	}

	return nil
}
