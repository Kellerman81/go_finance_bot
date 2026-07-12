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
	CloseMin int    // session close, minutes since local midnight
}

func hm(h, m int) int { return h*60 + m }

// Common European cash sessions (local time).
var (
	exUS      = Exchange{"US", "America/New_York", hm(9, 30), hm(16, 0)}
	exXetra   = Exchange{"Xetra", "Europe/Berlin", hm(9, 0), hm(17, 30)}
	exParis   = Exchange{"Euronext", "Europe/Paris", hm(9, 0), hm(17, 30)}
	exAms     = Exchange{"Euronext", "Europe/Amsterdam", hm(9, 0), hm(17, 30)}
	exBrussels = Exchange{"Euronext", "Europe/Brussels", hm(9, 0), hm(17, 30)}
	exLisbon  = Exchange{"Euronext", "Europe/Lisbon", hm(8, 0), hm(16, 30)}
	exDublin  = Exchange{"Euronext", "Europe/Dublin", hm(8, 0), hm(16, 30)}
	exLSE     = Exchange{"LSE", "Europe/London", hm(8, 0), hm(16, 30)}
	exSIX     = Exchange{"SIX", "Europe/Zurich", hm(9, 0), hm(17, 30)}
	exMilan   = Exchange{"Borsa Italiana", "Europe/Rome", hm(9, 0), hm(17, 30)}
	exMadrid  = Exchange{"BME", "Europe/Madrid", hm(9, 0), hm(17, 30)}
	exVienna  = Exchange{"Wiener Börse", "Europe/Vienna", hm(9, 0), hm(17, 30)}
	exStock   = Exchange{"Nasdaq Stockholm", "Europe/Stockholm", hm(9, 0), hm(17, 30)}
	exHelsinki = Exchange{"Nasdaq Helsinki", "Europe/Helsinki", hm(10, 0), hm(18, 30)}
	exOslo    = Exchange{"Oslo Børs", "Europe/Oslo", hm(9, 0), hm(16, 30)}
	exCopen   = Exchange{"Nasdaq Copenhagen", "Europe/Copenhagen", hm(9, 0), hm(17, 0)}
)

// suffixExchange maps a Yahoo-style exchange suffix to its venue. "" (no suffix)
// is treated as a US listing. German regional exchanges share Xetra's hours.
var suffixExchange = map[string]Exchange{
	"":   exUS,
	"DE": exXetra, "F": exXetra, "SG": exXetra, "MU": exXetra,
	"DU": exXetra, "HM": exXetra, "HA": exXetra, "BE": exXetra, "BM": exXetra,
	"PA": exParis,
	"AS": exAms,
	"BR": exBrussels,
	"LS": exLisbon,
	"IR": exDublin,
	"L":  exLSE, "IL": exLSE,
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
	ex, ok := suffixExchange[suffix]
	return ex, ok
}

var (
	locMu    sync.Mutex
	locCache = map[string]*time.Location{}
)

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

// OrderingAllowed reports whether an order for symbol may be placed at time t,
// allowing a window of preOpen before the exchange opens (and through to the
// close). It fails OPEN — returning true — when the venue calendar or timezone
// is unknown, so unmodelled instruments are never silently blocked. The reason
// string explains a refusal. Weekends are treated as closed; holidays are not
// modelled.
func OrderingAllowed(symbol string, t time.Time, preOpen time.Duration) (bool, string) {
	ex, known := ExchangeFor(symbol)
	if !known {
		return true, ""
	}
	loc, err := loadLoc(ex.TZ)
	if err != nil {
		return true, "" // can't resolve tz (no zoneinfo) — don't block
	}
	lt := t.In(loc)
	if wd := lt.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false, fmt.Sprintf("%s closed (weekend)", ex.Name)
	}
	midnight := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc)
	open := midnight.Add(time.Duration(ex.OpenMin) * time.Minute)
	close := midnight.Add(time.Duration(ex.CloseMin) * time.Minute)
	earliest := open.Add(-preOpen)
	switch {
	case lt.Before(earliest):
		return false, fmt.Sprintf("%s opens %s (now %s)", ex.Name, open.Format("15:04"), lt.Format("15:04 MST"))
	case lt.After(close):
		return false, fmt.Sprintf("%s closed at %s (now %s)", ex.Name, close.Format("15:04"), lt.Format("15:04 MST"))
	}
	return true, ""
}
