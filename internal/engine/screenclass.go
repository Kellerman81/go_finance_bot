package engine

import (
	"context"
	"strings"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/screener"
)

// The engine is the screener's Classifier and the owner of its optional Prober.
//
// The screener needs to know what each candidate IS before it can judge it by
// the rules written for that kind of instrument, and the engine already has the
// classification machinery: a persistent cache filled from the data provider's
// instrument search and the broker's own metadata, with config.yaml's
// asset_classes: block as the override layer (see names.go).
//
// Reusing it here rather than giving the screener its own classifier matters
// beyond saving code: a candidate screened as an ETF and then capped as an ETF
// by the portfolio allocation limits must be the same ETF to both, or the two
// systems disagree about what the book contains.

// AssetClass implements screener.Classifier. It answers from the cache when it
// can and falls back to a live lookup for a candidate the bot has never
// tracked — which is every symbol in a discovery universe on the first pass.
//
// A symbol that cannot be classified returns "", which the screener reads as
// "judge it against the base configuration". That is the honest answer: a wrong
// class applies the wrong resolution, the wrong filters and the wrong cap, all
// of which is worse than applying the general rules.
func (e *Engine) AssetClass(ctx context.Context, symbol string) string {
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	if sym == "" {
		return ""
	}

	e.mu.RLock()
	cached, ok := e.assetClasses[sym]
	e.mu.RUnlock()

	if ok {
		return cached
	}

	class := e.lookupAssetClass(ctx, sym)
	if class == "" {
		return ""
	}

	// Cache it: a screening pass over a few hundred candidates would otherwise
	// re-resolve every one of them on every pass, and a class is effectively
	// immutable. setAutoAssetClass persists and rebuilds the portfolio guard,
	// so the allocation caps learn the same classification at the same moment.
	e.setAutoAssetClass(sym, class)

	return class
}

// lookupAssetClass resolves a class the same way resolveNames does — the
// provider's instrument search first, the broker's own instrument list second —
// without touching the name or sector caches, which a screening candidate has
// no business populating before it is even adopted.
func (e *Engine) lookupAssetClass(ctx context.Context, symbol string) string {
	if lookup := e.instrumentLookup(); lookup != nil {
		if inst, ok := lookup(ctx, symbol); ok {
			if class := market.NormalizeAssetClass(inst.Type); class != "" {
				return class
			}
		}
	}

	if c, ok := e.brk.(broker.AssetClassifier); ok {
		return e.brokerAssetClass(ctx, c, symbol)
	}

	return ""
}

// Sector implements screener.Sectorizer. Like AssetClass it answers from the
// cache when it can and falls back to a live lookup for a candidate the bot has
// never tracked — which is every symbol in a discovery universe on the first
// pass.
//
// That fallback is the whole point. resolveNames only labels symbols ALREADY on
// the watchlist, and it runs detached after a symbol is adopted; a screener
// reading only that cache would see no sector for exactly the candidates it is
// trying to judge, and the diversification cap would silently never bind.
//
// A symbol with no sector returns "", which both the screener and the portfolio
// guard read as "exempt". Yahoo reports no sector for funds, so an ETF is
// normally unsectored — deliberately, since an ETF's industry exposure is a
// property of its holdings, not of the instrument.
func (e *Engine) Sector(ctx context.Context, symbol string) string {
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	if sym == "" {
		return ""
	}

	e.mu.RLock()
	cached, ok := e.sectors[sym]
	e.mu.RUnlock()

	if ok {
		return cached
	}

	lookup := e.instrumentLookup()
	if lookup == nil {
		return ""
	}

	inst, found := lookup(ctx, sym)
	if !found {
		return ""
	}

	sector := strings.ToLower(strings.TrimSpace(inst.Sector))
	if sector == "" {
		return ""
	}

	// Cache it the same way a tracked symbol's sector is cached, so a screening
	// pass over a few hundred candidates does not re-resolve every one of them
	// on every pass. setAutoSector persists and rebuilds the portfolio guard,
	// so the guard learns the same label at the same moment — which is what
	// keeps the screener's diversification and the guard's cap talking about
	// the same sectors.
	e.setAutoSector(sym, sector)

	return sector
}

// SetScreenProber installs the optional probe stage — the backtest/autotune
// scoring the screener runs over its shortlist. Passing nil disables it,
// whatever screener.probe says in the config.
//
// It is installed rather than constructed here because a probe needs the
// backtester and the optimizer, and internal/autotune already imports this
// package: the engine cannot reach back into it. cmd/bot wires the two
// together.
func (e *Engine) SetScreenProber(p screener.Prober) {
	e.mu.Lock()
	e.prober = p
	e.mu.Unlock()

	// Rebuild so a prober installed after the screener still reaches it.
	e.SetScreenerConfig(e.ScreenerConfig())
}

// screenProber returns the installed probe stage, which may be nil.
func (e *Engine) screenProber() screener.Prober {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.prober
}
