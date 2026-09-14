package market

import "testing"

// Every provider spells the same instrument differently. The whole point of
// normalisation is that one cap on "etf" binds all of these.
func TestNormalizeAssetClassAcrossProviders(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		// Yahoo search quoteType.
		"EQUITY":         AssetClassStock,
		"ETF":            AssetClassETF,
		"MUTUALFUND":     AssetClassFund,
		"CRYPTOCURRENCY": AssetClassCrypto,
		"FUTURE":         AssetClassFuture,
		"OPTION":         AssetClassOption,
		"INDEX":          AssetClassIndex,
		"CURRENCY":       AssetClassCurrency,

		// Saxo AssetType.
		"Stock":           AssetClassStock,
		"Etf":             AssetClassETF,
		"Etc":             AssetClassETF,
		"Etn":             AssetClassETF,
		"Bond":            AssetClassBond,
		"FxSpot":          AssetClassCurrency,
		"StockOption":     AssetClassOption,
		"ContractFutures": AssetClassFuture,
		"CfdOnStock":      AssetClassCFD,
		"ManagedFund":     AssetClassFund,

		// Trading 212 instrument type.
		"STOCK": AssetClassStock,

		// Spelling variants that must not fragment a bucket.
		"Exchange Traded Fund": AssetClassETF,
		"exchange-traded-fund": AssetClassETF,
		"  etf  ":              AssetClassETF,
	}

	for raw, want := range cases {
		if got := NormalizeAssetClass(raw); got != want {
			t.Errorf("NormalizeAssetClass(%q) = %q, want %q", raw, got, want)
		}
	}
}

// An empty type means "unknown", which callers must be able to distinguish
// from a real class: an unclassified symbol is left out of every allocation
// cap rather than charged to a guessed one.
func TestNormalizeAssetClassEmptyStaysEmpty(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"", "   ", "\t"} {
		if got := NormalizeAssetClass(raw); got != "" {
			t.Errorf("NormalizeAssetClass(%q) = %q, want empty", raw, got)
		}
	}
}

// A type nothing maps onto keeps the provider's own word rather than being
// discarded or forced into "stock". Guessing would quietly charge an exotic
// instrument to the equity budget; discarding would hide it from every cap.
func TestNormalizeAssetClassKeepsUnknownTypes(t *testing.T) {
	t.Parallel()

	if got := NormalizeAssetClass("FuturesStrategy"); got != "futuresstrategy" {
		t.Errorf("NormalizeAssetClass(FuturesStrategy) = %q, want the lower-cased original", got)
	}

	if got := NormalizeAssetClass("Warrant"); got == AssetClassStock {
		t.Error("an unrecognised type was guessed as a stock")
	}
}
