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
