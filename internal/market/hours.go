package market

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Exchange describes a venue's regular cash-trading session in its local
// timezone. Hours are approximate (continuous trading; auctions and holidays
// are not modelled) — enough to avoid placing orders when a market is plainly
// closed, which is when a reference price is most likely to be stale.
type Exchange struct {
	Name     string
	TZ       string // IANA timezone, e.g. "Europe/Berlin"
	OpenMin  int    // session open, minutes since local midnight
	CloseMin int    // session closeAt, minutes since local midnight
}

// hm converts an hour and minute into minutes since local midnight.
func hm(h, m int) int { return h*60 + m }

// Common European cash sessions (local time).
var (
	exUS    = Exchange{"US", "America/New_York", hm(9, 30), hm(16, 0)}
	exXetra = Exchange{"Xetra", "Europe/Berlin", hm(9, 0), hm(17, 30)}
	// The German regional exchanges are NOT on Xetra's session: Frankfurt's
	// specialist trading in shares and ETPs, and Stuttgart, Munich/gettex,
	// Düsseldorf, Hamburg-Hannover and Berlin, all run 08:00-22:00. Frankfurt
	// was set to 20:00 here, holding a .F stop that triggered at 20:30 until
	// the next morning while the venue traded — 22:00 checked against
	// Deutsche Börse's published hours (MK3-6). (Bonds close at 17:30; the
	// bot trades no bonds on .F.) Gating them to Xetra held a
	// stop on a .SG holding that triggered at 18:00 until the next morning
	// while its own venue was open — the hours gate also gates exits on a
	// broker with no schedule (T212, Alpaca) (MK2-8).
	exFrankfurt  = Exchange{"Börse Frankfurt", "Europe/Berlin", hm(8, 0), hm(22, 0)}
	exDERegional = Exchange{"German regional exchange", "Europe/Berlin", hm(8, 0), hm(22, 0)}
	exParis      = Exchange{"Euronext", "Europe/Paris", hm(9, 0), hm(17, 30)}
	exAms        = Exchange{"Euronext", "Europe/Amsterdam", hm(9, 0), hm(17, 30)}
	exBrussels   = Exchange{"Euronext", "Europe/Brussels", hm(9, 0), hm(17, 30)}
	exLisbon     = Exchange{"Euronext", "Europe/Lisbon", hm(8, 0), hm(16, 30)}
	exDublin     = Exchange{"Euronext", "Europe/Dublin", hm(8, 0), hm(16, 30)}
	exLSE        = Exchange{"LSE", "Europe/London", hm(8, 0), hm(16, 30)}
	// Cboe Europe's UK order book. Yahoo reports its listings as exchange
	// "Cboe UK" in Europe/London and they trade the LSE cash session; they
	// are named separately only so a refusal says which venue it means.
	exCboeUK   = Exchange{"Cboe UK", "Europe/London", hm(8, 0), hm(16, 30)}
	exSIX      = Exchange{"SIX", "Europe/Zurich", hm(9, 0), hm(17, 30)}
	exMilan    = Exchange{"Borsa Italiana", "Europe/Rome", hm(9, 0), hm(17, 30)}
	exMadrid   = Exchange{"BME", "Europe/Madrid", hm(9, 0), hm(17, 30)}
	exVienna   = Exchange{"Wiener Börse", "Europe/Vienna", hm(9, 0), hm(17, 30)}
	exStock    = Exchange{"Nasdaq Stockholm", "Europe/Stockholm", hm(9, 0), hm(17, 30)}
	exHelsinki = Exchange{"Nasdaq Helsinki", "Europe/Helsinki", hm(10, 0), hm(18, 30)}
	exOslo     = Exchange{"Oslo Børs", "Europe/Oslo", hm(9, 0), hm(16, 30)}
	exCopen    = Exchange{"Nasdaq Copenhagen", "Europe/Copenhagen", hm(9, 0), hm(17, 0)}
)

// exCrypto marks a symbol as trading continuously — OrderingAllowed treats a
// zero TZ as "24/7, no session to gate" and returns true unconditionally,
// with no weekday/holiday logic at all (crypto trades on weekends too).
var exCrypto = Exchange{Name: "Crypto"}

// cryptoQuoteCurrencies are the quote-currency suffixes Binance pairs are
// built from (see Binance.normalizePair) — a pair like "BTCUSDT" or
// "ETHEUR" has no Yahoo-style dot suffix, so without this it fell through
// to the "" (no-suffix) case below, which ExchangeFor treats as a US equity
// listing — silently gating a 24/7 market to ~76% closed (NYSE hours,
// weekends) and deferring every protective exit over the weekend, exactly
// when crypto moves most. See CODE_REVIEW_FINDINGS.md's crypto-trading-
// hours finding.
//
// This is a heuristic, not a real asset-class field on the tracked symbol
// (the review's suggested proper fix, and a larger change than this pass
// makes) — a non-crypto ticker that happens to end in one of these codes
// would be misclassified as 24/7. Given this bot's tickers are Yahoo-style
// equity codes (AAPL, SAP.DE) or Binance pairs (BTCUSDT), that collision is
// judged unlikely enough to accept for fixing a bug that otherwise disables
// the documented crypto trading path almost entirely.
var cryptoQuoteCurrencies = []string{
	"USDT", "USDC", "BUSD", "FDUSD", "TUSD",
	"USD", "EUR", "GBP", "TRY",
	// Fiat quotes Binance lists that no common equity ticker ends in. ARS and
	// DAI are left out on purpose (CARS is a NYSE listing): those pairs are
	// recognised through the feed instead — see cryptoFeedSymbols.
	"BRL", "JPY", "AUD", "PLN", "MXN", "ZAR", "RON", "UAH", "IDR",
	"BTC", "ETH", "BNB",
}

// cryptoFeedSymbols holds every symbol a crypto feed (Binance) has priced. The quote-suffix list is a heuristic, and a real Binance quote
// outside it — BRL, JPY, AUD, PLN, ARS, MXN, ZAR, DAI, … — fell through to the
// US equity session: BTCBRL was refused on a Saturday, exits included, until
// Monday 09:30 ET (MK3-5). A symbol the crypto feed carries is a crypto pair
// whatever it ends in; the list stays for symbols not (yet) subscribed.
var cryptoFeedSymbols sync.Map

// markCryptoFeedSymbol records that a crypto feed streams symbol.
func markCryptoFeedSymbol(symbol string) {
	cryptoFeedSymbols.Store(normalizePair(symbol), true)
}

// isCryptoFeedSymbol reports whether a crypto feed streams symbol.
func isCryptoFeedSymbol(symbol string) bool {
	_, ok := cryptoFeedSymbols.Load(normalizePair(symbol))

	return ok
}

// looksLikeCryptoPair reports whether symbol matches a Binance-style pair: a
// base asset concatenated directly with one of cryptoQuoteCurrencies, no
// separator.
//
// Against a BTC or ETH quote the base must be at least two characters. The US
// crypto ETFs and trusts are a one-letter prefix on exactly those two codes —
// GBTC, FBTC, FETH, CETH, EETH, SETH — which a one-character minimum read as
// 24/7 pairs: OrderingAllowed said yes on a Saturday and, the venue being
// "known", entryAllowed's weekend guard was skipped too, so a weekend buy was
// queued at the broker for Monday's open (MK2-5). The stablecoin and fiat
// quotes keep the one-character minimum: one-letter Binance bases exist
// (WUSDT, SUSDT, TUSDT), and misreading a real pair as an NYSE listing is the
// costly direction — the hours gate holds back its EXITS too.
func looksLikeCryptoPair(symbol string) bool {
	up := strings.ToUpper(symbol)

	for _, q := range cryptoQuoteCurrencies {
		minBase := 1
		if q == "BTC" || q == "ETH" {
			minBase = 2
		}

		if len(up) >= len(q)+minBase && strings.HasSuffix(up, q) {
			return true
		}
	}

	return false
}

// suffixExchange maps a Yahoo-style exchange suffix to its venue. "" (no suffix)
// is treated as a US listing. German regional exchanges keep their own, longer
// sessions — see exDERegional.
var suffixExchange = map[string]Exchange{
	"":   exUS,
	"DE": exXetra, "F": exFrankfurt,
	"SG": exDERegional, "MU": exDERegional, "DU": exDERegional, "HM": exDERegional,
	"HA": exDERegional, "BE": exDERegional, "BM": exDERegional,
	"PA": exParis,
	"AS": exAms,
	"BR": exBrussels,
	"LS": exLisbon,
	"IR": exDublin,
	"L":  exLSE, "IL": exLSE,
	"XC": exCboeUK,
	"SW": exSIX, "VX": exSIX,
	"MI": exMilan,
	"MC": exMadrid,
	"VI": exVienna,
	"ST": exStock,
	"HE": exHelsinki,
	"OL": exOslo,
	"CO": exCopen,
}

// ExchangeFor returns the exchange a symbol trades on, inferred from its
// Yahoo-style suffix, and whether the venue's calendar is known. Unknown
// suffixes (e.g. a custom feed code) report known=false so the caller can choose
// not to gate them.
func ExchangeFor(symbol string) (Exchange, bool) {
	suffix := ""
	if i := strings.LastIndexByte(symbol, '.'); i > 0 {
		suffix = strings.ToUpper(symbol[i+1:])
	}

	// No dot suffix is ambiguous between "US equity" and "crypto pair" (both
	// e.g. "AAPL" and "BTCUSDT" have none) — try the crypto pattern first,
	// since misreading a US equity ticker as 24/7 is far less costly than
	// gating a 24/7 market to NYSE hours.
	if suffix == "" && (isCryptoFeedSymbol(symbol) || looksLikeCryptoPair(symbol)) {
		return exCrypto, true
	}

	ex, ok := suffixExchange[suffix]

	return ex, ok
}

var (
	locMu    sync.Mutex
	locCache = make(map[string]*time.Location)
)

// loadLoc loads (and caches) an IANA timezone location.
func loadLoc(tz string) (*time.Location, error) {
	locMu.Lock()
	defer locMu.Unlock()

	if l, ok := locCache[tz]; ok {
		return l, nil
	}

	l, err := time.LoadLocation(tz)
	if err != nil {
		return nil, err
	}

	locCache[tz] = l

	return l, nil
}

// isWeekend reports whether t — already in whichever location the caller means
// to judge by — falls on a Saturday or Sunday.
func isWeekend(t time.Time) bool {
	wd := t.Weekday()

	return wd == time.Saturday || wd == time.Sunday
}

// IsWeekendUTC reports whether t is a Saturday or Sunday in UTC.
//
// It exists for the one question that needs no venue calendar: no cash-equity
// market anywhere trades on a weekend. OrderingAllowed cannot use it for an
// unknown venue — that answer also gates exits, which must never be refused on
// a calendar the bot does not have — but an ENTRY can, and should: an unmapped
// suffix (PRAJL.XC, Cboe UK — mapped above now, but there is always a next one)
// otherwise passes the hours gate all weekend, runs on to the venue-price
// check, and records a blocked trade saying no current price could be
// confirmed. Every evaluation, for two days.
//
// UTC because without a venue there is no local time to ask. The error that
// leaves is a market far enough east that its Monday has begun while UTC is
// still on Sunday, which delays an entry by a few hours — and declining to OPEN
// a position costs nothing, which is the trade Engine.entryAllowed already
// makes deliberately.
func IsWeekendUTC(t time.Time) bool { return isWeekend(t.UTC()) }

// OrderingAllowed reports whether an order for symbol may be placed at time t,
// allowing a window of preOpen before the exchange opens (and through to the
// closeAt). It fails OPEN — returning true — when the venue's session or
// timezone is unknown, so unmodelled instruments are never silently blocked.
// The reason string explains a refusal. Weekends are treated as closed;
// holidays are not modelled.
//
// Failing open covers the weekend too, for an unknown venue: this answer is
// what an EXIT is gated on, and an exit must never be refused by a calendar the
// bot does not actually have (see Engine.marketOpen). Entries resolve that
// uncertainty the other way — see IsWeekendUTC and Engine.entryAllowed.
func OrderingAllowed(symbol string, t time.Time, preOpen time.Duration) (bool, string) {
	ex, known := ExchangeFor(symbol)
	if !known {
		return true, ""
	}

	if ex.TZ == "" { // 24/7 venue (crypto) — no session to gate, ever
		return true, ""
	}

	loc, err := loadLoc(ex.TZ)
	if err != nil {
		return true, "" // can't resolve tz (no zoneinfo) — don't block
	}

	lt := t.In(loc)
	if isWeekend(lt) {
		return false, fmt.Sprintf("%s closed (weekend)", ex.Name)
	}

	midnight := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc)
	open := midnight.Add(time.Duration(ex.OpenMin) * time.Minute)
	closeAt := midnight.Add(time.Duration(ex.CloseMin) * time.Minute)
	earliest := open.Add(-preOpen)

	switch {
	case lt.Before(earliest):
		return false, fmt.Sprintf(
			"%s opens %s (now %s)",
			ex.Name,
			open.Format("15:04"),
			lt.Format("15:04 MST"),
		)

	case lt.After(closeAt):
		return false, fmt.Sprintf(
			"%s closed at %s (now %s)",
			ex.Name,
			closeAt.Format("15:04"),
			lt.Format("15:04 MST"),
		)
	}

	return true, ""
}
