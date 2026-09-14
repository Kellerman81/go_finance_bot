package screener

import (
	"slices"
	"strings"

	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// Starter candidate pools.
//
// "Screen a universe" is useless advice if you do not already know what belongs
// in one, and an empty universe makes the screener filter-only: it can drop
// symbols but can never find any. These pools exist so that turning the
// screener on actually does something before the operator has researched a
// single ticker.
//
// They are deliberately boring — large, liquid, widely available instruments in
// the symbol convention the history provider speaks (Yahoo), spanning US and
// European listings so an EU account has something it can actually trade. They
// are a place to BEGIN, not a recommendation: nothing here bypasses a single
// filter, a cost check or a probe, and a starter symbol that cannot clear its
// own spread is refused exactly like any other candidate.
//
// Curated rather than fetched. A broker's full catalogue is tens of thousands
// of instruments in the broker's own ticker convention (Trading 212's
// BAYNd_EQ), which is not what the watchlist or the history source speak —
// resolving each one back to a Yahoo symbol costs a lookup per instrument and
// still fails for many. A short, known-good list is more useful than a long,
// mostly-unresolvable one.

// suggested maps an asset class to its starter pool.
var suggested = map[string][]string{
	market.AssetClassStock: {
		// US megacaps — the most liquid, tightest-spread instruments there are.
		"AAPL", "MSFT", "NVDA", "GOOGL", "AMZN", "META", "AVGO", "AMD",
		"JPM", "V", "UNH", "XOM", "JNJ", "PG", "COST", "WMT",
		// European large caps, for an account that trades locally.
		"SAP.DE", "SIE.DE", "ALV.DE", "MBG.DE", "BAS.DE", "BAYN.DE",
		"ASML.AS", "MC.PA", "OR.PA", "AIR.PA", "NESN.SW", "NOVN.SW",
	},
	market.AssetClassETF: {
		// Broad global and US equity trackers, EUR- and USD-denominated.
		"IWDA.AS", "VWCE.DE", "EUNL.DE", "SPY", "VOO", "IVV", "QQQ", "VTI",
		// Regional and factor tilts.
		"EIMI.L", "IEUR", "EXS1.DE", "IUSQ.DE", "XDWD.DE",
		// Sector and thematic, for a screener ranking on trend.
		"XLK", "XLE", "XLF", "XLV", "SMH",
		// Bond and short-duration trackers — the quiet end of the range.
		"AGGH.DE", "IBTA.L", "XEON.DE",
	},
	market.AssetClassCrypto: {
		// Only meaningful with a crypto-capable feed; harmless otherwise,
		// since an unfetchable symbol simply fails the history filter.
		"BTC-USD", "ETH-USD", "SOL-USD",
	},
}

// SuggestedUniverse returns the starter candidate pool for an asset class, or
// nil when there is none. The result is a copy: a caller appending to it must
// not mutate the shared list.
func SuggestedUniverse(class string) []string {
	pool, ok := suggested[normalizeClass(class)]
	if !ok {
		return nil
	}

	return slices.Clone(pool)
}

// SuggestedClasses lists every class with a starter pool, sorted — for the API,
// so the UI can offer the real set rather than a hard-coded copy.
func SuggestedClasses() []string {
	out := make([]string, 0, len(suggested))
	for class := range suggested {
		out = append(out, class)
	}

	slices.Sort(out)

	return out
}

// Suggestions returns every starter pool, keyed by class — what the settings UI
// shows when an operator asks "what should I put in here?".
func Suggestions() map[string][]string {
	out := make(map[string][]string, len(suggested))
	for class, pool := range suggested {
		out[class] = slices.Clone(pool)
	}

	return out
}

// normalizeClass canonicalises an asset-class name to the spelling the pools
// are keyed by, via the same normaliser the classifier itself uses so "ETF",
// "Etf" and "etf" all find the same pool.
func normalizeClass(class string) string {
	if c := market.NormalizeAssetClass(class); c != "" {
		return c
	}

	return strings.ToLower(strings.TrimSpace(class))
}
