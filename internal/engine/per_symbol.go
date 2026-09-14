package engine

import (
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// SetSymbolStrategies installs per-symbol parameter overrides. Each entry is a
// fully-resolved StrategyConfig (config.Load merges overrides onto the base), so
// this only has to build a Strategy per symbol.
//
// A symbol with no override uses the shared strategy, which is the common case;
// the map is expected to be small.
func (e *Engine) SetSymbolStrategies(overrides map[string]config.StrategyConfig) {
	built := make(map[string]strategy.Strategy, len(overrides))
	cfgs := make(map[string]config.StrategyConfig, len(overrides))

	for sym, sc := range overrides {
		built[sym] = strategy.New(sc)
		cfgs[sym] = sc
	}

	e.mu.Lock()

	e.symStrategies = built
	e.symStratCfg = cfgs
	e.mu.Unlock()
}

// strategyFor returns the strategy to evaluate symbol with: its override if it
// has one, otherwise the shared strategy.
func (e *Engine) strategyFor(symbol string) strategy.Strategy {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if s, ok := e.symStrategies[symbol]; ok {
		return s
	}

	return e.start
}

// StrategyConfigFor returns the parameters symbol is evaluated with — its
// override if it has one, otherwise the shared config. The detector views and
// the indicator snapshot on each trade use this, so the UI shows what actually
// drove a decision rather than the base config the symbol may not be using.
func (e *Engine) StrategyConfigFor(symbol string) config.StrategyConfig {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if sc, ok := e.symStratCfg[symbol]; ok {
		return sc
	}

	return e.stratCfg
}

// OverriddenSymbols lists the symbols evaluated with their own parameters.
func (e *Engine) OverriddenSymbols() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	out := make([]string, 0, len(e.symStrategies))
	for sym := range e.symStrategies {
		out = append(out, sym)
	}

	return out
}
