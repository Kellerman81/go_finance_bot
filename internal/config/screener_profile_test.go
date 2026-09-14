package config

import (
	"strings"
	"testing"
)

func ptr[T any](v T) *T { return &v }

// A profile inherits every field it does not name. This is the whole contract:
// naming one setting must not silently reset the rest, exactly as
// strategy_overrides already guarantees.
func TestScreenerProfileInheritsUnnamedFields(t *testing.T) {
	t.Parallel()

	base := ScreenerConfig{
		Resolution:   "1",
		LookbackDays: 30,
		MaxSymbols:   25,
		Rank:         "volume",
		MinPrice:     5,
		MinAvgVolume: 100000,
		Filters:      []string{"history", "price", "volume"},
		ByAssetClass: map[string]ScreenerProfile{
			"etf": {
				Resolution:   ptr("D"),
				LookbackDays: ptr(365),
				MaxSymbols:   ptr(8),
				Rank:         ptr("trend"),
			},
		},
	}

	etf := base.ForAssetClass("etf")

	if etf.Resolution != "D" || etf.LookbackDays != 365 || etf.MaxSymbols != 8 || etf.Rank != "trend" {
		t.Fatalf("overrides not applied: %+v", etf)
	}

	// Untouched fields must survive.
	if etf.MinPrice != 5 || etf.MinAvgVolume != 100000 {
		t.Errorf("inherited bounds lost: min_price=%v min_avg_volume=%v",
			etf.MinPrice, etf.MinAvgVolume)
	}

	if len(etf.Filters) != 3 {
		t.Errorf("filters = %v, want the base chain inherited", etf.Filters)
	}

	// The base itself must be unchanged by resolving a profile from it.
	if base.Resolution != "1" || base.MaxSymbols != 25 {
		t.Errorf("resolving a profile mutated the base: %+v", base)
	}

	// A class with no profile gets the base, with defaults filled in.
	stock := base.ForAssetClass("stock")
	if stock.Resolution != "1" || stock.MaxSymbols != 25 || stock.Rank != "volume" {
		t.Errorf("unprofiled class = %+v, want the base", stock)
	}
}

// Config files are written by hand: a profile keyed "ETF" must match a symbol
// the classifier labelled "etf".
func TestScreenerProfileLookupIsCaseInsensitive(t *testing.T) {
	t.Parallel()

	base := ScreenerConfig{
		MaxSymbols:   25,
		ByAssetClass: map[string]ScreenerProfile{" ETF ": {MaxSymbols: ptr(3)}},
	}

	if got := base.ForAssetClass("etf").MaxSymbols; got != 3 {
		t.Errorf("max_symbols = %d, want the differently-cased profile to apply", got)
	}
}

// A profile may legitimately set 0 to mean "no bound" — but 0 for resolution
// and lookback means "unset", and must fall back to the documented default
// rather than producing a zero-length history window.
func TestScreenerProfileZeroValuesFallBackToDefaults(t *testing.T) {
	t.Parallel()

	base := ScreenerConfig{
		Resolution:   "15",
		LookbackDays: 90,
		MaxSymbols:   10,
		ByAssetClass: map[string]ScreenerProfile{
			"etf": {Resolution: ptr(""), LookbackDays: ptr(0), MaxSymbols: ptr(0)},
		},
	}

	etf := base.ForAssetClass("etf")

	if etf.Resolution != defaultScreenerResolution {
		t.Errorf("resolution = %q, want the default", etf.Resolution)
	}

	if etf.LookbackDays != defaultScreenerLookbackDays {
		t.Errorf("lookback_days = %d, want the default", etf.LookbackDays)
	}

	// 0 for a cap genuinely means "no cap", and must be preserved.
	if etf.MaxSymbols != 0 {
		t.Errorf("max_symbols = %d, want 0 (no cap) preserved", etf.MaxSymbols)
	}
}

// Universe precedence: the class profile's own pool, then the per-class map,
// then the shared list. The pools have nothing to do with each other, so the
// most specific one must win outright rather than merging.
func TestScreenerUniversePrecedence(t *testing.T) {
	t.Parallel()

	base := ScreenerConfig{
		Universe:             []string{"SHARED"},
		UniverseByAssetClass: map[string][]string{"etf": {"BYCLASS"}, "stock": {"STOCKPOOL"}},
		ByAssetClass: map[string]ScreenerProfile{
			"etf": {Universe: []string{"INPROFILE"}},
		},
	}

	if got := base.ForAssetClass("etf").Universe; len(got) != 1 || got[0] != "INPROFILE" {
		t.Errorf("etf universe = %v, want the profile's own pool to win", got)
	}

	if got := base.ForAssetClass("stock").Universe; len(got) != 1 || got[0] != "STOCKPOOL" {
		t.Errorf("stock universe = %v, want universe_by_asset_class", got)
	}

	if got := base.ForAssetClass("crypto").Universe; len(got) != 1 || got[0] != "SHARED" {
		t.Errorf("crypto universe = %v, want the shared fallback", got)
	}
}

// Bounds are validated AFTER inheritance, which is the only way to catch a
// pair that crosses only once the profile is overlaid — neither value being
// wrong on its own.
func TestScreenerValidatesProfilesAfterInheritance(t *testing.T) {
	t.Parallel()

	bad := ScreenerConfig{
		MaxPrice:     100, // base caps price at 100 ...
		ByAssetClass: map[string]ScreenerProfile{"etf": {MinPrice: ptr(500.0)}},
	}

	err := bad.Validate()
	if err == nil {
		t.Fatal("a min_price above the inherited max_price was accepted")
	}

	if !strings.Contains(err.Error(), "etf") {
		t.Errorf("error %q does not name the offending class", err)
	}

	// Each value is fine in isolation, proving the check needed the overlay.
	solo := ScreenerConfig{MaxPrice: 100}
	if err := solo.Validate(); err != nil {
		t.Errorf("the base alone was rejected: %v", err)
	}
}

// R² is a fraction; 50 meaning 50% would reject every candidate while looking
// like a deliberate setting.
func TestScreenerValidatesNewBounds(t *testing.T) {
	t.Parallel()

	for name, c := range map[string]ScreenerConfig{
		"r2 above 1":           {MinTrendR2: 50},
		"negative r2":          {MinTrendR2: -0.1},
		"negative slope":       {MinTrendSlopePct: -1},
		"negative drawdown":    {MaxDrawdownPct: -1},
		"negative cost mult":   {MinCostMultiple: -1},
		"negative lookback":    {LookbackDays: -1},
		"empty class key":      {ByAssetClass: map[string]ScreenerProfile{"  ": {}}},
		"empty class universe": {UniverseByAssetClass: map[string][]string{"etf": {}}},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	ok := ScreenerConfig{MinTrendR2: 0.5, MinTrendSlopePct: 0.001, MaxDrawdownPct: 0.3, MinCostMultiple: 3}
	if err := ok.Validate(); err != nil {
		t.Errorf("a valid configuration was rejected: %v", err)
	}
}

// A probe with neither mode set has nothing to run — an enabled switch that
// silently does nothing is worse than a refusal at load.
func TestScreenerProbeValidation(t *testing.T) {
	t.Parallel()

	if err := (ScreenerProbeConfig{Enabled: true}).Validate(); err == nil {
		t.Error("probe.enabled with no mode was accepted")
	}

	if err := (ScreenerProbeConfig{Enabled: true, Backtest: true, MaxCandidates: -1}).Validate(); err == nil {
		t.Error("a negative max_candidates was accepted")
	}

	if err := (ScreenerProbeConfig{Enabled: false, MaxCandidates: -1}).Validate(); err != nil {
		t.Errorf("a disabled probe block was validated: %v", err)
	}

	if err := (ScreenerProbeConfig{Enabled: true, Autotune: true}).Validate(); err != nil {
		t.Errorf("a valid probe config was rejected: %v", err)
	}
}

// AssetClassesConfigured is what tells the screener which classes to build
// pools for before it has classified anything.
func TestAssetClassesConfigured(t *testing.T) {
	t.Parallel()

	c := ScreenerConfig{
		ByAssetClass:         map[string]ScreenerProfile{"ETF": {}, "stock": {}},
		UniverseByAssetClass: map[string][]string{"etf": {"X"}, "crypto": {"Y"}},
	}

	got := c.AssetClassesConfigured()
	if len(got) != 3 {
		t.Fatalf("classes = %v, want 3 distinct (etf, stock, crypto)", got)
	}

	for _, want := range []string{"etf", "stock", "crypto"} {
		if !contains(got, want) {
			t.Errorf("classes = %v, missing %q", got, want)
		}
	}
}

func contains(all []string, want string) bool {
	for _, s := range all {
		if s == want {
			return true
		}
	}

	return false
}
