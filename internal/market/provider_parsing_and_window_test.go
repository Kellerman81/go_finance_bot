package market

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// MK2-7: Yahoo emits ragged OHLC arrays around corporate actions and partial
// days. The parser bounded its loop by Close alone and indexed Open/High/Low
// past their end — a panic, which on the engine goroutine disarms trading.
// A short array must cost the rows it lacks, nothing more.
func TestYahooRaggedArraysDoNotPanic(t *testing.T) {
	now := time.Now().Unix()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"chart":{"result":[{"timestamp":[` +
			itoa(now-120) + `,` + itoa(now-60) + `,` + itoa(now) + `],` +
			`"indicators":{"quote":[{"open":[1,2],"high":[1,2,3],"low":[1,2,3],"close":[1,2,3],"volume":[1,1,1]}]}}],"error":null}}`))
	}))
	t.Cleanup(srv.Close)

	y := NewYahooHistory()
	y.base = srv.URL
	y.minInterval = 0

	got, _, err := y.fetchOnce(context.Background(), srv.URL, "AAPL", "1m", "1d")
	if err != nil {
		t.Fatalf("fetchOnce: %v", err)
	}

	if len(got) != 2 {
		t.Errorf("got %d rows, want the 2 every array covers", len(got))
	}
}

// MK2-7, Finnhub's half: the same unchecked indexing over o/h/l/v/t.
func TestFinnhubRaggedArraysDoNotPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"s":"ok","c":[1,2,3],"o":[1,2,3],"h":[1,2,3],"l":[1,2,3],"v":[1,1],"t":[1,2,3]}`))
	}))
	t.Cleanup(srv.Close)

	f := NewFinnhub("key", srv.URL, "", slog.New(slog.NewTextHandler(io.Discard, nil)))

	got, err := f.Candles(context.Background(), "AAPL", Res1Min, time.Unix(0, 0), time.Unix(10, 0))
	if err != nil {
		t.Fatalf("Candles: %v", err)
	}

	if len(got) != 2 {
		t.Errorf("got %d rows, want the 2 every array covers", len(got))
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// MK2-6: Yahoo was asked for fixed ranges whatever the caller's window, so
// the screener's lookback_days was a silent no-op — 90 days at "15" gave 30,
// 10 days at "D" gave a year. The range must widen to cover the window
// (capped at what Yahoo serves), and the result must be the window.
func TestYahooHonoursTheRequestedWindow(t *testing.T) {
	var gotRange string

	now := time.Now()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.URL.Query().Get("range")

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(dailyBars(now, 400)))
	}))
	t.Cleanup(srv.Close)

	y := NewYahooHistory()
	y.base = srv.URL
	y.minInterval = 0

	got, err := y.Candles(context.Background(), "AAPL", Res1Day, now.AddDate(0, 0, -90), now)
	if err != nil {
		t.Fatalf("Candles: %v", err)
	}

	if len(got) > 91 || len(got) < 89 {
		t.Errorf("got %d daily bars for a 90-day window, want ~90", len(got))
	}

	if _, err := y.Candles(context.Background(), "AAPL", Res1Day, now.AddDate(-3, 0, 0), now); err != nil {
		t.Fatalf("Candles: %v", err)
	}

	if gotRange != "5y" {
		t.Errorf("a 3-year window asked Yahoo for range %q, want 5y", gotRange)
	}
}

// dailyBars is a chart body with n daily bars ending at end.
func dailyBars(end time.Time, n int) string {
	var ts, px strings.Builder

	for i := range n {
		if i > 0 {
			ts.WriteByte(',')
			px.WriteByte(',')
		}

		ts.WriteString(strconv.FormatInt(end.AddDate(0, 0, i-n+1).Unix(), 10))
		px.WriteString("100")
	}

	return `{"chart":{"result":[{"timestamp":[` + ts.String() + `],"indicators":{"quote":[{` +
		`"open":[` + px.String() + `],"high":[` + px.String() + `],"low":[` + px.String() +
		`],"close":[` + px.String() + `],"volume":[` + px.String() + `]}]}}],"error":null}}`
}

// Found by the verify-change pass on MK2-6: windowing every attempt cut the
// coarser fallback to warmup's 9-day window — ~60 hourly bars for an illiquid
// symbol where it used to get three months. A fallback exists to supply the
// history the fine interval lacks; only the requested interval is windowed.
func TestYahooFallbackKeepsItsHistory(t *testing.T) {
	now := time.Now()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Query().Get("interval") == "1h" {
			_, _ = w.Write([]byte(barsEvery(now, 400, time.Hour)))

			return
		}

		_, _ = w.Write([]byte(barsEvery(now, 5, time.Minute))) // too few
	}))
	t.Cleanup(srv.Close)

	y := NewYahooHistory()
	y.base = srv.URL
	y.minInterval = 0

	got, err := y.Candles(context.Background(), "ILLQ", Res1Min, now.Add(-9*24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 400 {
		t.Errorf("fallback served %d hourly bars, want all 400", len(got))
	}
}

// barsEvery is a chart body with n flat bars spaced step apart, ending at end.
func barsEvery(end time.Time, n int, step time.Duration) string {
	var ts, px strings.Builder

	for i := range n {
		if i > 0 {
			ts.WriteByte(',')
			px.WriteByte(',')
		}

		ts.WriteString(strconv.FormatInt(end.Add(time.Duration(i-n+1)*step).Unix(), 10))
		px.WriteString("100")
	}

	return `{"chart":{"result":[{"timestamp":[` + ts.String() + `],"indicators":{"quote":[{` +
		`"open":[` + px.String() + `],"high":[` + px.String() + `],"low":[` + px.String() +
		`],"close":[` + px.String() + `],"volume":[` + px.String() + `]}]}}],"error":null}}`
}

// Found by the verify-change pass: a 2-day 1m window (venuePrice's) early on
// a Monday holds only the new session's first bars — the weekend is empty —
// so judging "enough bars" on the window fell through to hourly data, and
// venuePrice refused every entry for the first hour of the week. Yahoo HAS
// the 1m bars; the window being short is not illiquidity.
func TestAShortWindowAfterAWeekendStaysOnTheRequestedInterval(t *testing.T) {
	now := time.Now()
	friday := now.Add(-72 * time.Hour)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Query().Get("interval") {
		case "1h":
			_, _ = w.Write([]byte(barsEvery(now, 400, time.Hour)))
		case "1d":
			_, _ = w.Write([]byte(barsEvery(now, 500, 24*time.Hour)))
		default: // 1m: Friday's session plus this morning's first half hour
			_, _ = w.Write([]byte(twoSessions(friday, 500, now, 30)))
		}
	}))
	t.Cleanup(srv.Close)

	y := NewYahooHistory()
	y.base = srv.URL
	y.minInterval = 0

	got, err := y.Candles(context.Background(), "SAP.DE", Res1Min, now.Add(-48*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 30 {
		t.Fatalf("got %d bars, want this morning's 30 one-minute bars", len(got))
	}

	if gap := got[1].Time.Sub(got[0].Time); gap != time.Minute {
		t.Errorf("bar gap %v: served a coarser fallback instead of 1m data", gap)
	}
}

// twoSessions is a 1m chart body: n1 bars ending at end1, then n2 ending at end2.
func twoSessions(end1 time.Time, n1 int, end2 time.Time, n2 int) string {
	var ts, px strings.Builder

	add := func(end time.Time, n int) {
		for i := range n {
			if ts.Len() > 0 {
				ts.WriteByte(',')
				px.WriteByte(',')
			}

			ts.WriteString(strconv.FormatInt(end.Add(time.Duration(i-n+1)*time.Minute).Unix(), 10))
			px.WriteString("100")
		}
	}

	add(end1, n1)
	add(end2, n2)

	return `{"chart":{"result":[{"timestamp":[` + ts.String() + `],"indicators":{"quote":[{` +
		`"open":[` + px.String() + `],"high":[` + px.String() + `],"low":[` + px.String() +
		`],"close":[` + px.String() + `],"volume":[` + px.String() + `]}]}}],"error":null}}`
}
