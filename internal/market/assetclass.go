package market

import "strings"

// An instrument's asset class is the coarsest description of what you are
// actually holding, and the one a percentage allocation is written in: "no more
// than 30% of the book in ETFs" is a sentence about classes, not about sectors
// or correlations.
//
// Every data source spells it differently. Yahoo's search calls a share
// "EQUITY", Saxo calls it "Stock", Trading 212 calls it "STOCK"; a tracker fund
// is "ETF" to two of them and "Etf"/"Etc"/"Etn" to Saxo depending on its legal
// wrapper. Rather than let each caller invent its own spelling — which is how a
// cap on "etf" silently fails to see a position classified as "Etf" — every
// source is normalised through NormalizeAssetClass into the small vocabulary
// below, and that vocabulary is what config.Portfolio.MaxPctPerAssetClass is
// keyed by.

// The canonical asset classes. Anything a provider reports that does not map to
// one of these is left as its own lower-cased label rather than being forced
// into a wrong bucket — an unrecognised class is still a class, and a cap can be
// written against it once you know what your provider calls it.
const (
	AssetClassStock    = "stock"    // ordinary shares, ADRs, depositary receipts
	AssetClassETF      = "etf"      // exchange-traded funds, and ETC/ETN wrappers
	AssetClassFund     = "fund"     // mutual funds / non-exchange-traded funds
	AssetClassBond     = "bond"     // individual bonds (a bond ETF is an etf)
	AssetClassCrypto   = "crypto"   // cryptocurrencies and crypto trackers
	AssetClassOption   = "option"   // options on anything
	AssetClassFuture   = "future"   // futures and CFDs on futures
	AssetClassCFD      = "cfd"      // contracts for difference on cash instruments
	AssetClassCurrency = "currency" // FX spot/forwards
	AssetClassIndex    = "index"    // index instruments (rarely tradeable directly)
)

// assetClassAliases maps every provider spelling this bot has seen onto a
// canonical class. Keys are upper-cased with spaces, hyphens and underscores
// removed, which is what NormalizeAssetClass reduces its input to — so one entry
// covers "Exchange Traded Fund", "EXCHANGE-TRADED-FUND" and "exchangetradedfund".
//
// Sources: Yahoo search quoteType (EQUITY/ETF/MUTUALFUND/CRYPTOCURRENCY/
// FUTURE/OPTION/INDEX/CURRENCY), Saxo AssetType (Stock/Etf/Etc/Etn/Bond/
// FxSpot/StockOption/ContractFutures/CfdOn*), Trading 212 instrument type
// (STOCK/ETF/CRYPTOCURRENCY/...).
var assetClassAliases = map[string]string{
	// Shares.
	"EQUITY":          AssetClassStock,
	"STOCK":           AssetClassStock,
	"STOCKS":          AssetClassStock,
	"SHARE":           AssetClassStock,
	"COMMONSTOCK":     AssetClassStock,
	"ADR":             AssetClassStock,
	"GDR":             AssetClassStock,
	"DEPOSITARYSHARE": AssetClassStock,

	// Exchange-traded products. ETC (commodity) and ETN (note) are different
	// legal wrappers around the same "exchange-traded tracker" exposure, and
	// nobody writing an allocation means to budget for them separately.
	"ETF":                AssetClassETF,
	"ETC":                AssetClassETF,
	"ETN":                AssetClassETF,
	"EXCHANGETRADEDFUND": AssetClassETF,
	"EXCHANGETRADEDNOTE": AssetClassETF,
	"ETFS":               AssetClassETF,
	"FUNDETF":            AssetClassETF,

	// Non-exchange-traded funds.
	"MUTUALFUND":  AssetClassFund,
	"FUND":        AssetClassFund,
	"RIGHTS":      AssetClassStock,
	"MANAGEDFUND": AssetClassFund,

	// Debt.
	"BOND":           AssetClassBond,
	"BONDS":          AssetClassBond,
	"CORPORATEBOND":  AssetClassBond,
	"GOVERNMENTBOND": AssetClassBond,

	// Crypto.
	"CRYPTOCURRENCY": AssetClassCrypto,
	"CRYPTO":         AssetClassCrypto,
	"DIGITALASSET":   AssetClassCrypto,

	// Derivatives.
	"OPTION":           AssetClassOption,
	"OPTIONS":          AssetClassOption,
	"STOCKOPTION":      AssetClassOption,
	"STOCKINDEXOPTION": AssetClassOption,
	"FUTURESOPTION":    AssetClassOption,
	"CFDONOPTION":      AssetClassOption,

	"FUTURE":          AssetClassFuture,
	"FUTURES":         AssetClassFuture,
	"CONTRACTFUTURES": AssetClassFuture,
	"CFDONFUTURES":    AssetClassFuture,

	"CFD":        AssetClassCFD,
	"CFDONSTOCK": AssetClassCFD,
	"CFDONETF":   AssetClassCFD,
	"CFDONETC":   AssetClassCFD,
	"CFDONETN":   AssetClassCFD,
	"CFDONINDEX": AssetClassCFD,
	"CFDONFUND":  AssetClassCFD,
	"CFDINDEX":   AssetClassCFD,

	// FX.
	"CURRENCY":     AssetClassCurrency,
	"FXSPOT":       AssetClassCurrency,
	"FXFORWARDS":   AssetClassCurrency,
	"FXVANILLAOPT": AssetClassCurrency,

	// Indices.
	"INDEX":      AssetClassIndex,
	"INDICES":    AssetClassIndex,
	"STOCKINDEX": AssetClassIndex,
}

// NormalizeAssetClass maps a provider's instrument-type string onto a canonical
// asset class. It returns "" for empty input, so callers can tell "this symbol
// has no known class" (leave it unclassified, reconsider it later) from "this
// symbol is a stock".
//
// An unrecognised type is returned lower-cased rather than discarded or mapped
// to stock. Guessing "stock" would quietly count an exotic instrument against
// the equity budget, and discarding it would make the position invisible to
// every cap; keeping the provider's own word means the operator can see the
// label in the UI and write a cap for it.
func NormalizeAssetClass(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}

	key := strings.ToUpper(trimmed)
	for _, sep := range []string{" ", "-", "_", "."} {
		key = strings.ReplaceAll(key, sep, "")
	}

	if class, ok := assetClassAliases[key]; ok {
		return class
	}

	return strings.ToLower(trimmed)
}
