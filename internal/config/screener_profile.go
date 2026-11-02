package config

import (
	"errors"
	"fmt"

	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// errEmptyScreenerClass and errProbeWithoutMode are the two screener
// misconfigurations with nothing useful to interpolate into the message.
var (
	errEmptyScreenerClass = errors.New(
		"screener: an asset-class key is empty — name the class (etf, stock, ...)")
	errProbeWithoutMode = errors.New(
		"screener.probe.enabled needs backtest and/or autotune set, " +
			"or there is nothing for it to run")
)

// Resolving a per-asset-class screening profile.
//
// The screener asks one question per candidate — "is this worth trading?" —
// but the right way to answer it depends entirely on what the instrument IS.
// A broad accumulating ETF has no intraday character to measure: judged on
// one-minute bars its ATR%, spread estimate and momentum all describe the tick
// rather than the fund. A liquid megacap judged on daily bars over a year
// hides exactly the intraday behaviour you would trade it for.
//
// So every screenable setting can be overridden per class, and a profile
// inherits every field it does not name. ForAssetClass does that overlay and
// hands back a plain ScreenerConfig, so the rest of the screener never has to
// know profiles exist.

// defaultScreenerResolution is the bar size used when none is configured,
// matching the resolution the live engine keeps its own history at.
const defaultScreenerResolution = "1"

// defaultScreenerLookbackDays is the history window used when none is
// configured.
const defaultScreenerLookbackDays = 30

// ForAssetClass returns the screening settings for one asset class: the base
// config with that class's profile overlaid. An unknown or empty class gets the
// base config unchanged, so a symbol nothing could classify is still screened
// rather than silently skipped.
//
// The returned value is a complete ScreenerConfig with its own ByAssetClass
// cleared — it is a resolved leaf, and leaving the map in place would invite a
// caller to resolve it twice.
func (c ScreenerConfig) ForAssetClass(class string) ScreenerConfig {
	out := c
	out.ByAssetClass = nil
	out.Universe = c.universeFor(class)

	if out.Resolution == "" {
		out.Resolution = defaultScreenerResolution
	}

	if out.LookbackDays <= 0 {
		out.LookbackDays = defaultScreenerLookbackDays
	}

	p, ok := c.profileFor(class)
	if !ok {
		return out
	}

	applyPtr(&out.Resolution, p.Resolution)
	applyPtr(&out.LookbackDays, p.LookbackDays)
	applyPtr(&out.MaxSymbols, p.MaxSymbols)
	applyPtr(&out.MaxSymbolsPerSector, p.MaxSymbolsPerSector)
	applyPtr(&out.Rank, p.Rank)
	applyPtr(&out.MinPrice, p.MinPrice)
	applyPtr(&out.MaxPrice, p.MaxPrice)
	applyPtr(&out.MinAvgVolume, p.MinAvgVolume)
	applyPtr(&out.MinVolatilityPct, p.MinVolatilityPct)
	applyPtr(&out.MaxVolatilityPct, p.MaxVolatilityPct)
	applyPtr(&out.MinHistoryBars, p.MinHistoryBars)
	applyPtr(&out.MaxSpreadPct, p.MaxSpreadPct)
	applyPtr(&out.MinTrendR2, p.MinTrendR2)
	applyPtr(&out.MinTrendSlopePct, p.MinTrendSlopePct)
	applyPtr(&out.MaxDrawdownPct, p.MaxDrawdownPct)
	applyPtr(&out.MinCostMultiple, p.MinCostMultiple)
	applyPtr(&out.SuggestUniverse, p.SuggestUniverse)

	if len(p.Filters) > 0 {
		out.Filters = p.Filters
	}

	// Re-apply the zero-value defaults: a profile is free to set 0, and 0 for
	// these two means "unset", not "no history".
	if out.Resolution == "" {
		out.Resolution = defaultScreenerResolution
	}

	if out.LookbackDays <= 0 {
		out.LookbackDays = defaultScreenerLookbackDays
	}

	return out
}

// universeFor picks the candidate pool for a class: the class profile's own
// Universe, then UniverseByAssetClass, then the shared top-level Universe.
//
// Most-specific-wins, and an empty pool at every level is left empty — the
// caller (screener.candidates) is what decides whether to fall back to the
// current watchlist or to a suggested starter pool, since only it knows
// whether SuggestUniverse is set.
func (c ScreenerConfig) universeFor(class string) []string {
	if p, ok := c.profileFor(class); ok && len(p.Universe) > 0 {
		return p.Universe
	}

	if u := c.ownUniverse(class); len(u) > 0 {
		return u
	}

	return c.Universe
}

// ownUniverse is class's universe_by_asset_class entry, its key matched in
// canonical form as profileFor matches profiles. It indexed the map with the
// canonical key against keys stored as written: {ETF: [...]} was listed as a
// configured class and then never found — the curated pool was not screened,
// and with suggest_universe the starter pool replaced it (SC3-5).
func (c ScreenerConfig) ownUniverse(class string) []string {
	key := normalizeClassKey(class)
	if key == "" {
		return nil
	}

	for name, u := range c.UniverseByAssetClass {
		if normalizeClassKey(name) == key {
			return u
		}
	}

	return nil
}

// HasOwnUniverse reports whether a class has a candidate pool written for it
// specifically — a profile Universe or a universe_by_asset_class entry — as
// opposed to merely inheriting the shared top-level one.
//
// The distinction is what makes per-class seeding safe: a class with a curated
// pool is left alone, because quietly appending a dozen suggestions to a list
// someone chose deliberately would dilute the decision rather than help.
func (c ScreenerConfig) HasOwnUniverse(class string) bool {
	if p, ok := c.profileFor(class); ok && len(p.Universe) > 0 {
		return true
	}

	return len(c.ownUniverse(class)) > 0
}

// profileFor looks a class's profile up case-insensitively — config files are
// written by hand, and a profile keyed "ETF" must match a symbol classified
// "etf".
func (c ScreenerConfig) profileFor(class string) (ScreenerProfile, bool) {
	key := normalizeClassKey(class)
	if key == "" || len(c.ByAssetClass) == 0 {
		return ScreenerProfile{}, false
	}

	for name, p := range c.ByAssetClass {
		if normalizeClassKey(name) == key {
			return p, true
		}
	}

	return ScreenerProfile{}, false
}

// AssetClassesConfigured lists every class with a profile or a universe of its
// own, so the screener knows which classes it is expected to screen for even
// before it has classified a single candidate.
func (c ScreenerConfig) AssetClassesConfigured() []string {
	seen := make(map[string]bool, len(c.ByAssetClass)+len(c.UniverseByAssetClass))

	for name := range c.ByAssetClass {
		if k := normalizeClassKey(name); k != "" {
			seen[k] = true
		}
	}

	for name := range c.UniverseByAssetClass {
		if k := normalizeClassKey(name); k != "" {
			seen[k] = true
		}
	}

	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}

	return out
}

// normalizeClassKey canonicalises an asset-class name the same way
// market.NormalizeAssetClass leaves them: lower-cased and trimmed. It does not
// import market (config is a leaf that everything else depends on), and does
// not need to: by the time a class reaches config it has already been through
// the classifier.
func normalizeClassKey(class string) string {
	return CanonicalAssetClass(class)
}

// CanonicalAssetClass is the one form an asset-class key is compared in:
// market.NormalizeAssetClass, aliases included — the form the classifier's
// output already takes. Lower-casing alone left a profile keyed "stocks" or
// "equity" matching nothing (the classifier says "stock"), and
// asset_classes: {X: ETFs} a class "etfs" no cap, profile or pool knew,
// each accepted at load and silently doing nothing (SC3-5).
func CanonicalAssetClass(class string) string {
	return market.NormalizeAssetClass(class)
}

// applyPtr overwrites dst when the override is present. Generic over the
// field types a profile can override.
func applyPtr[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}

// Validate checks every per-class profile by resolving it and running the base
// config's own validation over the result.
//
// Resolving first is the point: a profile is only ever used overlaid, so
// checking the overlay is what catches a class whose min/max pair crosses only
// after inheritance — e.g. a base max_price of 100 with a profile min_price of
// 500, where neither value is wrong on its own.
func (c ScreenerConfig) validateProfiles() error {
	for name := range c.ByAssetClass {
		if normalizeClassKey(name) == "" {
			return errEmptyScreenerClass
		}

		resolved := c.ForAssetClass(name)
		resolved.ByAssetClass = nil

		if err := resolved.validateBounds(); err != nil {
			return fmt.Errorf("screener.by_asset_class[%s]: %w", name, err)
		}
	}

	for name, u := range c.UniverseByAssetClass {
		if normalizeClassKey(name) == "" {
			return errEmptyScreenerClass
		}

		if len(u) == 0 {
			return fmt.Errorf(
				"screener.universe_by_asset_class[%s] is empty — remove the key "+
					"instead, or it reads as a deliberate empty pool", name)
		}
	}

	return c.Probe.Validate()
}

// Validate rejects a probe configuration that cannot do anything useful.
func (p ScreenerProbeConfig) Validate() error {
	if !p.Enabled {
		return nil
	}

	if !p.Backtest && !p.Autotune {
		return errProbeWithoutMode
	}

	if p.MaxCandidates < 0 {
		return fmt.Errorf("screener.probe.max_candidates must be >= 0, got %d", p.MaxCandidates)
	}

	if p.Bars < 0 {
		return fmt.Errorf("screener.probe.bars must be >= 0, got %d", p.Bars)
	}

	if p.MinTrades < 0 {
		return fmt.Errorf("screener.probe.min_trades must be >= 0, got %d", p.MinTrades)
	}

	return nil
}
