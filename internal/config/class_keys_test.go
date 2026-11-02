package config

import "testing"

// SC3-5 (a): universe_by_asset_class was indexed with the canonical class
// against keys stored as written, so {ETF: [...]} was listed as a configured
// class and then never found — the curated pool was not screened, and with
// suggest_universe the starter pool replaced it.
func TestAnUpperCaseUniverseKeyIsFound(t *testing.T) {
	t.Parallel()

	c := ScreenerConfig{UniverseByAssetClass: map[string][]string{"ETF": {"IWDA.AS", "EUNL.DE"}}}

	if !c.HasOwnUniverse("etf") {
		t.Error("HasOwnUniverse(etf) = false for a universe keyed ETF; suggest_universe would replace it")
	}

	if got := c.ForAssetClass("etf").Universe; len(got) != 2 {
		t.Errorf("the etf class screens %v, want the curated IWDA.AS/EUNL.DE pool", got)
	}
}

// SC3-5 (b): profile keys were only lower-cased while the classifier's output
// is alias-normalised, so a profile keyed "stocks" never matched a symbol
// classified "stock": its resolution and max_symbols silently did not apply.
func TestAnAliasProfileKeyMatchesItsClass(t *testing.T) {
	t.Parallel()

	res := "D"
	c := ScreenerConfig{ByAssetClass: map[string]ScreenerProfile{"stocks": {Resolution: &res}}}

	if got := c.ForAssetClass("stock").Resolution; got != "D" {
		t.Errorf("a profile keyed stocks gives class stock resolution %q, want D", got)
	}

	p := PortfolioConfig{MaxPctPerAssetClass: map[string]float64{"ETFs": 0.3}}
	if got := p.NormalizedAssetClassCaps()["etf"]; got != 0.3 {
		t.Errorf("max_pct_per_asset_class {ETFs: 0.3} caps etf at %v, want 0.3", got)
	}
}
