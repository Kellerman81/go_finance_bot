package engine

import (
	"github.com/Kellerman81/go_finance_bot/internal/risk"
)

// SetPortfolioGuard installs the concentration caps (sector exposure, correlated
// positions). Passing nil disables them. Call before Run.
//
// The same guard goes to the risk manager, which reads one thing from it: the
// per-class position slot that sizes a buy down (min_positions_per_asset_class).
// Both installs happen here so the two can never hold different generations of
// a guard that is rebuilt on every portfolio settings change.
func (e *Engine) SetPortfolioGuard(g *risk.PortfolioGuard) {
	e.mu.Lock()

	e.guard = g
	e.mu.Unlock()

	e.rm.SetPortfolioGuard(g)
}

// portfolioGuard returns the installed guard, which may be nil (safe to use).
func (e *Engine) portfolioGuard() *risk.PortfolioGuard {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.guard
}

// Returns supplies recent per-bar returns for symbol from the engine's own
// candle history, so correlations are measured on exactly the bars the strategy
// trades on rather than a separately-fetched series that might disagree.
//
// It implements risk.Returns.
func (e *Engine) Returns(symbol string, n int) ([]float64, bool) {
	candles := e.seriesFor(symbol)
	if len(candles) < 2 {
		return nil, false
	}

	// n returns need n+1 closes.
	if want := n + 1; len(candles) > want {
		candles = candles[len(candles)-want:]
	}

	closes := make([]float64, len(candles))
	for i, c := range candles {
		closes[i] = c.Close
	}

	rets := risk.BarReturns(closes)

	return rets, len(rets) > 0
}
