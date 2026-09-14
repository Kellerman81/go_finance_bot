package engine

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// Symbols are not readable. A watchlist of LXS.DE, XD5E.DE, PRAJL.XC and IBCI.DE
// tells an operator nothing about what the bot is holding, and the one place it
// matters most is the moment before you click sell.
//
// So the engine keeps a symbol → company name cache. It is persisted rather than
// re-fetched: a name is effectively immutable, and paying a network round trip
// per symbol on every start to re-learn that BAYN.DE is Bayer would be absurd.

// errNoSearcher reports that no configured provider can search instruments, so
// the picker is unavailable. Distinct from an empty result: "we cannot look"
// and "we looked and found nothing" need different messages in the UI.
var errNoSearcher = errors.New("instrument search is not available with this data provider")

// nameLookupLimit bounds how many unknown symbols one pass will resolve, so a
// large watchlist arriving at once cannot turn into a burst against the search
// endpoint. The rest are picked up on the next pass.
const nameLookupLimit = 8

// SymbolName returns the company name for symbol, or "" when it is not known.
// Callers render the symbol alone in that case rather than blocking on a lookup.
func (e *Engine) SymbolName(symbol string) string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.names[symbol]
}

// SymbolNames returns a copy of the whole cache, for views that render a table.
func (e *Engine) SymbolNames() map[string]string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	out := make(map[string]string, len(e.names))
	for sym, name := range e.names {
		out[sym] = name
	}

	return out
}

// SetSymbolName records a name learned without a lookup — a search result the
// operator picked already carries one, so adding from the picker costs nothing.
func (e *Engine) SetSymbolName(symbol, name string) {
	name = strings.TrimSpace(name)
	if symbol == "" || name == "" {
		return
	}

	e.mu.Lock()

	if e.names[symbol] == name {
		e.mu.Unlock()

		return // unchanged; don't rewrite the store
	}

	e.names[symbol] = name
	e.mu.Unlock()

	e.persistNames()
}

// searcher returns the instrument search provider, or nil when neither the
// history source nor the live feed can search.
func (e *Engine) searcher() market.Searcher {
	if s := searcherOf(e.historySource()); s != nil {
		return s
	}

	return searcherOf(e.provider)
}

// searcherOf finds a Searcher in v, unwrapping decorators on the way. The
// history source is wrapped in a cache before the engine ever sees it, so a
// direct type assertion finds the wrapper and concludes search is unavailable.
func searcherOf(v any) market.Searcher {
	for range maxUnwrap {
		if s, ok := v.(market.Searcher); ok {
			return s
		}

		u, ok := v.(interface{ Unwrap() market.HistoryFetcher })
		if !ok {
			return nil
		}

		v = u.Unwrap()
	}

	return nil
}

// maxUnwrap bounds decorator unwrapping, so a source that wrapped itself could
// not hang the caller.
const maxUnwrap = 8

// SearchInstruments finds instruments by name or ticker for the symbol picker.
// It returns nil when no provider can search, which the UI reports as "search
// unavailable" rather than "nothing found" — those mean very different things to
// someone typing a company name.
func (e *Engine) SearchInstruments(
	ctx context.Context,
	query string,
	limit int,
) ([]market.Instrument, error) {
	s := e.searcher()
	if s == nil {
		return nil, errNoSearcher
	}

	return s.Search(ctx, query, limit)
}

// CanSearch reports whether instrument search is available.
func (e *Engine) CanSearch() bool { return e.searcher() != nil }

// brokerResolveTimeout bounds the tradeability probe. It reads a cached
// instrument list, so it is normally instant; the bound exists so that the very
// first call — which loads that list over the network — cannot hang a search.
const brokerResolveTimeout = 5 * time.Second

// TradeableAtBroker reports whether the broker can trade symbol, and whether it
// could answer at all.
//
// known=false means "do not claim either way": a broker with no instrument list
// (the simulator) or one whose metadata has not loaded yet must not have its
// silence rendered as "your broker cannot trade this".
//
// This is what makes the symbol picker useful rather than merely informative.
// Searching for Lanxess returns its XETRA, Milan, Frankfurt, Vienna, Dusseldorf,
// Stuttgart and Munich listings; a given account can usually trade one of them,
// and picking another is how you reach an order the venue refuses outright
// (see untradeable.go).
func (e *Engine) TradeableAtBroker(ctx context.Context, symbol string) (tradeable, known bool) {
	r, ok := e.brk.(broker.SymbolResolver)
	if !ok {
		return false, false
	}

	ctx, cancel := context.WithTimeout(ctx, brokerResolveTimeout)
	defer cancel()

	if _, err := r.ResolveSymbol(ctx, symbol); err != nil {
		// A context failure is our problem, not a verdict on the instrument.
		if ctx.Err() != nil {
			return false, false
		}

		return false, true
	}

	return true, true
}

// resolveNames fills in names, sectors AND asset classes for tracked symbols
// missing any of them, bounded by nameLookupLimit (one Lookup call serves all
// three — Yahoo's search response already carries a sector and an instrument
// type alongside the name, see market.Instrument.Sector/Type). It is
// best-effort: whatever cannot be found is left blank — a name renders as its
// ticker, a missing sector simply isn't subject to the portfolio sector caps,
// and an unclassified symbol isn't subject to the asset-class allocation caps.
//
// Called after symbols join the watchlist, which covers both a fresh add and —
// because start-up re-adds the whole persisted watchlist — the one-time backfill
// for an installation upgrading to this. The three fields are checked
// separately (not skipping a symbol just because its name is already known), so
// a symbol that only picked up a name in an earlier version keeps getting
// reconsidered for a sector and a class on every restart until it has them.
//
// Search is not required. A provider that cannot search (Finnhub, Binance)
// leaves names and sectors unresolvable, but the broker can still classify —
// so the pass runs regardless and simply fills in less.
func (e *Engine) resolveNames(ctx context.Context, symbols []string) {
	lookup := e.instrumentLookup()

	classifier, canClassify := e.brk.(broker.AssetClassifier)
	if lookup == nil && !canClassify {
		return
	}

	found := 0

	for _, sym := range symbols {
		if found >= nameLookupLimit {
			return
		}

		needName := e.SymbolName(sym) == ""
		needSector := !e.hasSector(sym)
		needClass := !e.hasAssetClass(sym)

		if !needName && !needSector && !needClass {
			continue
		}

		var (
			inst     market.Instrument
			resolved bool
		)

		if lookup != nil {
			// Only a lookup that actually found something counts against the
			// budget, so a handful of permanently unresolvable symbols at the
			// head of the watchlist cannot starve every symbol behind them.
			if inst, resolved = lookup(ctx, sym); resolved {
				found++
			}
		}

		if needName {
			e.SetSymbolName(sym, inst.Name)
		}

		if needSector {
			e.setAutoSector(sym, inst.Sector)
		}

		if !needClass {
			continue
		}

		// Search first, broker second: the provider's own classification
		// matches the symbol convention the watchlist and history are in,
		// and the broker is the fallback for what it could not place.
		class := market.NormalizeAssetClass(inst.Type)
		if class == "" && canClassify {
			if class = e.brokerAssetClass(ctx, classifier, sym); class != "" && !resolved {
				found++
			}
		}

		e.setAutoAssetClass(sym, class)
	}
}

// instrumentLookup returns the provider's exact-symbol lookup, or nil when no
// configured provider can search.
func (e *Engine) instrumentLookup() func(context.Context, string) (market.Instrument, bool) {
	s := e.searcher()
	if s == nil {
		return nil
	}

	l, ok := s.(interface {
		Lookup(context.Context, string) (market.Instrument, bool)
	})
	if !ok {
		return nil
	}

	return l.Lookup
}

// brokerAssetClass asks the broker to classify symbol, for the symbols the data
// provider's search cannot place. It returns "" when the broker has no
// instrument metadata (the simulator) or simply does not know the symbol — both
// of which mean "leave it unclassified" rather than "guess".
func (e *Engine) brokerAssetClass(
	ctx context.Context,
	c broker.AssetClassifier,
	symbol string,
) string {
	ctx, cancel := context.WithTimeout(ctx, brokerResolveTimeout)
	defer cancel()

	class, err := c.AssetClass(ctx, symbol)
	if err != nil {
		return ""
	}

	return market.NormalizeAssetClass(class)
}

// persistNames writes the cache so a restart does not re-fetch every name.
func (e *Engine) persistNames() {
	if err := e.store.SaveNames(e.SymbolNames()); err != nil {
		e.log.Warn("persist symbol names failed", "err", err)
	}
}

// restoreNames loads the persisted cache.
func (e *Engine) restoreNames() {
	names, err := e.store.LoadNames()
	if err != nil || len(names) == 0 {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	for sym, name := range names {
		e.names[sym] = name
	}
}

// hasSector reports whether symbol already has a sector label, from either
// config.yaml or a prior auto-fetch.
func (e *Engine) hasSector(symbol string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()

	_, ok := e.sectors[strings.ToUpper(strings.TrimSpace(symbol))]

	return ok
}

// setAutoSector records a sector learned from search, without overriding an
// explicit config.yaml entry or a sector already learned — config.yaml stays
// the override layer for anyone who wants to state or correct one by hand;
// this fills in everything else with zero maintenance. A blank sector (funds
// have none) is a no-op, not a recorded empty string, so a later restart
// still reconsiders the symbol rather than treating "no sector" as final.
func (e *Engine) setAutoSector(symbol, sector string) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	sector = strings.ToLower(strings.TrimSpace(sector))

	if symbol == "" || sector == "" {
		return
	}

	e.mu.Lock()

	if _, known := e.sectors[symbol]; known {
		e.mu.Unlock()

		return
	}

	merged := make(map[string]string, len(e.sectors)+1)
	for sym, sec := range e.sectors {
		merged[sym] = sec
	}

	merged[symbol] = sector
	cfg, classes := e.portCfg, e.assetClasses

	e.mu.Unlock()

	// Rebuilds the portfolio guard and logs, same as a live settings PUT —
	// SetPortfolioConfig does not persist to config.yaml itself (that file
	// is the hand-maintained override layer, not where auto-learned data
	// belongs), so the auto-fetch cache is saved separately below.
	e.SetPortfolioConfig(cfg, merged, classes)
	e.persistSectors()
}

// persistSectors writes the auto-learned sector cache so a restart does not
// re-fetch every one of them. Unlike restoreSectors (which must merge under
// config.yaml), this simply saves the engine's current live map: whatever
// config.yaml contributed is already folded in by the time this is called,
// and re-saving it alongside the auto-learned entries is harmless — the
// config file itself remains the source of truth for its own entries on the
// next restore, since restoreSectors always re-applies it on top.
func (e *Engine) persistSectors() {
	e.mu.RLock()
	sectors := make(map[string]string, len(e.sectors))

	for sym, sec := range e.sectors {
		sectors[sym] = sec
	}
	e.mu.RUnlock()

	if err := e.store.SaveSectors(sectors); err != nil {
		e.log.Warn("persist auto-learned sectors failed", "err", err)
	}
}

// restoreSectors loads the auto-learned sector cache and layers it under
// whatever config.yaml already contributed to e.sectors (via the startup
// SetPortfolioConfig call) — config always wins, this only fills gaps.
func (e *Engine) restoreSectors() {
	auto, err := e.store.LoadSectors()
	if err != nil || len(auto) == 0 {
		return
	}

	e.mu.Lock()

	merged := make(map[string]string, len(e.sectors)+len(auto))
	for sym, sec := range auto {
		merged[sym] = sec
	}

	for sym, sec := range e.sectors { // config.yaml entries win on conflict
		merged[sym] = sec
	}

	cfg, classes := e.portCfg, e.assetClasses
	e.mu.Unlock()

	e.SetPortfolioConfig(cfg, merged, classes)
}

// hasAssetClass reports whether symbol already has an asset class, from either
// config.yaml or a prior auto-classification.
func (e *Engine) hasAssetClass(symbol string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()

	_, ok := e.assetClasses[strings.ToUpper(strings.TrimSpace(symbol))]

	return ok
}

// AssetClasses returns a copy of the live symbol -> asset class map, for the
// API and the UI: an allocation cap that refuses an order is only legible when
// you can see which bucket the bot put each position in.
func (e *Engine) AssetClasses() map[string]string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	out := make(map[string]string, len(e.assetClasses))
	for sym, class := range e.assetClasses {
		out[sym] = class
	}

	return out
}

// setAutoAssetClass records a class learned from search or the broker, without
// overriding an explicit config.yaml entry or one already learned — config.yaml
// stays the override layer for anyone who wants to state or correct a
// classification by hand; this fills in everything else with zero maintenance.
//
// A blank class is a no-op, not a recorded empty string, so a symbol nothing
// could classify today is reconsidered on the next pass rather than being
// permanently marked "unclassifiable" — and, while it is unclassified, it
// simply does not count against any allocation cap.
func (e *Engine) setAutoAssetClass(symbol, class string) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	class = strings.ToLower(strings.TrimSpace(class))

	if symbol == "" || class == "" {
		return
	}

	e.mu.Lock()

	if _, known := e.assetClasses[symbol]; known {
		e.mu.Unlock()

		return
	}

	merged := make(map[string]string, len(e.assetClasses)+1)
	for sym, c := range e.assetClasses {
		merged[sym] = c
	}

	merged[symbol] = class
	cfg, sectors := e.portCfg, e.sectors

	e.mu.Unlock()

	// Rebuilds the portfolio guard, same as setAutoSector does — and for the
	// same reason the auto-learned cache is persisted separately rather than
	// written back into config.yaml.
	e.SetPortfolioConfig(cfg, sectors, merged)
	e.persistAssetClasses()
}

// persistAssetClasses writes the auto-learned classification cache so a restart
// does not re-classify every symbol.
func (e *Engine) persistAssetClasses() {
	if err := e.store.SaveAssetClasses(e.AssetClasses()); err != nil {
		e.log.Warn("persist auto-learned asset classes failed", "err", err)
	}
}

// restoreAssetClasses loads the auto-learned classification cache and layers it
// under whatever config.yaml already contributed (via the startup
// SetPortfolioConfig call) — config always wins, this only fills gaps.
func (e *Engine) restoreAssetClasses() {
	auto, err := e.store.LoadAssetClasses()
	if err != nil || len(auto) == 0 {
		return
	}

	e.mu.Lock()

	merged := make(map[string]string, len(e.assetClasses)+len(auto))
	for sym, class := range auto {
		merged[sym] = class
	}

	for sym, class := range e.assetClasses { // config.yaml entries win on conflict
		merged[sym] = class
	}

	cfg, sectors := e.portCfg, e.sectors
	e.mu.Unlock()

	e.SetPortfolioConfig(cfg, sectors, merged)
}
