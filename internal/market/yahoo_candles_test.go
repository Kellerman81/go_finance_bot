package market

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/errlog"
)

// chartBars builds a minimal Yahoo chart JSON body with n bars, each closing
// at 100+i.
func chartBars(n int) string {
	now := time.Now().Unix()

	ts := make([]int64, n)
	closes := make([]float64, n)

	for i := range n {
		ts[i] = now - int64(n-i)*60
		closes[i] = 100 + float64(i)
	}

	tsJSON, _ := json.Marshal(ts)
	closeJSON, _ := json.Marshal(closes)

	return fmt.Sprintf(`{"chart":{"result":[{"timestamp":%s,"indicators":{"quote":[{
		"open":%s,"high":%s,"low":%s,"close":%s,"volume":%s
	}]}}],"error":null}}`, tsJSON, closeJSON, closeJSON, closeJSON, closeJSON, closeJSON)
}

// TestYahooCandlesFallsBackAcrossIntervals reproduces the Yahoo-granularity
// finding at the network layer, exercising Candles end to end for the first
// time (the chart path previously ignored y.base entirely, so it had no
// test coverage — see the endpoints() fix alongside this test). A request
// for 1-minute bars that has too few of them falls back to 1-hour bars, and
// the substitution must be recorded in errlog rather than silently served
// as if it were the requested resolution.
func TestYahooCandlesFallsBackAcrossIntervals(t *testing.T) {
	errlog.Clear()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Query().Get("interval") {
		case "1m":
			_, _ = w.Write([]byte(chartBars(5))) // too few: below the "enough" threshold
		case "1h":
			_, _ = w.Write([]byte(chartBars(65))) // enough: the fallback that should win
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	y := NewYahooHistory()
	y.base = srv.URL
	y.minInterval = 0

	candles, err := y.Candles(context.Background(), "AAPL", Res1Min, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Candles: %v", err)
	}

	if len(candles) != 65 {
		t.Fatalf("got %d candles, want 65 (the 1h fallback)", len(candles))
	}

	found := false

	for _, e := range errlog.Recent() {
		if e.Source == "yahoo" && strings.Contains(e.Message, "requested 1m") && strings.Contains(e.Message, "serving 1h") {
			found = true

			break
		}
	}

	if !found {
		t.Error("no errlog entry recorded the silent interval substitution (1m requested, 60m served)")
	}
}

// TestYahooCandlesNoFallbackNeeded checks the log entry is NOT written when
// the first (requested) interval already has enough bars — the log should
// mark an actual substitution, not fire on every call.
func TestYahooCandlesNoFallbackNeeded(t *testing.T) {
	errlog.Clear()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Query().Get("interval") == "1m" {
			_, _ = w.Write([]byte(chartBars(65)))
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	y := NewYahooHistory()
	y.base = srv.URL
	y.minInterval = 0

	candles, err := y.Candles(context.Background(), "AAPL", Res1Min, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Candles: %v", err)
	}

	if len(candles) != 65 {
		t.Fatalf("got %d candles, want 65", len(candles))
	}

	for _, e := range errlog.Recent() {
		if e.Source == "yahoo" && strings.Contains(e.Message, "requested") {
			t.Errorf("unexpected substitution log when no fallback was needed: %q", e.Message)
		}
	}
}

// TestYahooDailyRequestNeverFallsBackToFinerBars is the regression for MK2-2:
// the "coarser" fallback list was hourly then daily, added whenever it merely
// differed from the requested interval — so a daily request with few bars was
// served HOURLY bars. A young ETF then passed min_history_bars, and its trend,
// drawdown, volume and cost were judged on hourly bars against daily
// thresholds while the verdict said "D".
func TestYahooDailyRequestNeverFallsBackToFinerBars(t *testing.T) {
	var asked []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Query().Get("interval"))
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Query().Get("interval") == "1h" {
			_, _ = w.Write([]byte(chartBars(280)))

			return
		}

		_, _ = w.Write([]byte(chartBars(40))) // a young listing: 40 daily bars
	}))
	t.Cleanup(srv.Close)

	y := NewYahooHistory()
	y.base = srv.URL
	y.minInterval = 0

	candles, err := y.Candles(context.Background(), "NEWETF", Res1Day, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Candles: %v", err)
	}

	if len(candles) != 40 {
		t.Errorf("a daily request got %d bars (asked %v), want the 40 daily ones", len(candles), asked)
	}

	for _, iv := range asked {
		if iv == "1h" {
			t.Errorf("a daily request asked Yahoo for hourly bars (%v)", asked)
		}
	}
}

// TestYahooCooldownDoesNotEvictItsOwnCause is the regression for EL2-1. While
// a cooldown was in force every refused request recorded its own errlog entry,
// three per Candles call (the 1m, 1h and 1d attempts), so about 34 calls filled
// yahoo's 100-entry share of the ring and pushed out the real 429 or 5xx that
// started the cooldown: 100 identical "cooling down" lines and no cause.
//
// Not parallel: errlog is process-wide.
func TestYahooCooldownDoesNotEvictItsOwnCause(t *testing.T) {
	errlog.Clear()
	t.Cleanup(errlog.Clear)

	y := NewYahooHistory()
	y.base = "http://127.0.0.1:1"
	y.minInterval = 0

	errlog.Record("yahoo", "HTTP 429: the real cause")
	y.coolDown()

	for range 40 {
		_, _ = y.Candles(context.Background(), "AAPL", Res1Min, time.Time{}, time.Time{})
	}

	found, cooling := false, 0

	for _, e := range errlog.Recent() {
		if e.Source != "yahoo" {
			continue
		}

		if strings.Contains(e.Message, "the real cause") {
			found = true
		}

		if strings.Contains(e.Message, "cooling down") {
			cooling++
		}
	}

	if !found {
		t.Error("40 refused requests during a cooldown evicted the error that caused it")
	}

	if cooling != 1 {
		t.Errorf("the cooldown was recorded %d times, want once", cooling)
	}
}
