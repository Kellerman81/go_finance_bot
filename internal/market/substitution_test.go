package market

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/errlog"
)

// hourlyFallback serves 90 days of hourly bars whatever is asked — what a
// Yahoo fallback for an illiquid symbol hands the cache on a miss.
type hourlyFallback struct{}

func (hourlyFallback) Candles(_ context.Context, symbol string, _ Resolution, _, to time.Time) ([]Candle, error) {
	n := 90 * 24
	out := make([]Candle, n)

	for i := range out {
		out[i] = Candle{Symbol: symbol, Time: to.Add(-time.Duration(n-i) * time.Hour), Open: 10, High: 10, Low: 10, Close: 10}
	}

	return out, nil
}

// MK3-3: the miss path returns a coarser fallback whole (MK2-6), but a hit
// within the TTL sliced it to the narrower request: a warmup got three months
// of hourly bars or nine days of them depending on whether the screener had
// fetched the symbol in the last 15 s. A hit must answer like a miss.
func TestACacheHitKeepsAFallbacksHistory(t *testing.T) {
	t.Parallel()

	now := time.Now()
	c := NewCachingHistory(hourlyFallback{}, time.Minute)

	miss, err := c.Candles(t.Context(), "THIN.DE", Res1Min, now.Add(-30*24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}

	hit, err := c.Candles(t.Context(), "THIN.DE", Res1Min, now.Add(-9*24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}

	if len(hit) != len(miss) {
		t.Errorf("the same fallback gave %d bars on a miss and %d on a hit", len(miss), len(hit))
	}
}

// MK3-4: the substitution note was written only when some interval reached
// 60 bars; below that, the most bars could still come from the fallback and
// were served with no note — 40 hourly bars for a 1m request, silently.
func TestASubstitutionBelowEnoughIsStillNoted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Query().Get("interval") {
		case "1m":
			_, _ = w.Write([]byte(chartBars(5)))
		case "1h":
			_, _ = w.Write([]byte(chartBars(40)))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	y := NewYahooHistory()
	y.base = srv.URL
	y.minInterval = 0

	got, err := y.Candles(context.Background(), "SUBNOTE", Res1Min, time.Time{}, time.Time{})
	if err != nil || len(got) != 40 {
		t.Fatalf("Candles = %d bars, %v; want the 40 hourly ones", len(got), err)
	}

	for _, e := range errlog.Recent() {
		if strings.Contains(e.Message, "SUBNOTE: requested 1m") {
			return
		}
	}

	t.Error("40 hourly bars were served for a 1m request with no substitution note")
}

// MK3-5: a Binance pair whose quote was not in the hard-coded list fell
// through to the US equity session — BTCBRL refused on a Saturday, its exits
// included, until Monday 09:30 ET. Common fiat quotes are listed now, and a
// pair the crypto feed has priced is crypto whatever it ends in.
func TestCryptoPairsOutsideTheOldQuoteListTradeAtTheWeekend(t *testing.T) {
	t.Parallel()

	saturday := time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC)

	for _, sym := range []string{"BTCBRL", "ETHJPY", "SOLAUD"} {
		if ok, why := OrderingAllowed(sym, saturday, 0); !ok {
			t.Errorf("%s refused on a Saturday: %s", sym, why)
		}
	}

	markCryptoFeedSymbol("BTCARS")

	if ok, why := OrderingAllowed("BTCARS", saturday, 0); !ok {
		t.Errorf("BTCARS, priced by the crypto feed, refused on a Saturday: %s", why)
	}

	if ok, _ := OrderingAllowed("CARS", saturday, 0); ok {
		t.Error("CARS, a NYSE listing, was read as a 24/7 crypto pair")
	}
}

// EL3-1: the substitution note was recorded on every call, with the varying
// bar count in its text, and venuePrice asks every evaluation — one illiquid
// symbol filled Yahoo's errlog share within the hour and evicted the real
// error an operator needed. It is announced once while it stands.
func TestASubstitutionIsAnnouncedOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Query().Get("interval") == "1h" {
			_, _ = w.Write([]byte(chartBars(65)))

			return
		}

		_, _ = w.Write([]byte(chartBars(5)))
	}))
	t.Cleanup(srv.Close)

	y := NewYahooHistory()
	y.base = srv.URL
	y.minInterval = 0

	for range 5 {
		if _, err := y.Candles(context.Background(), "ONCEX", Res1Min, time.Time{}, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}

	n := 0

	for _, e := range errlog.Recent() {
		if strings.Contains(e.Message, "ONCEX: requested 1m") {
			n++
		}
	}

	if n != 1 {
		t.Errorf("%d substitution notes after 5 calls, want 1", n)
	}
}
