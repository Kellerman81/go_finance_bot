package backtest

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// LoadCSV reads OHLCV candles from a CSV file with a header row. Required
// columns (case-insensitive): symbol, time, open, high, low, close, volume.
// The time column accepts RFC3339, "2006-01-02 15:04:05", or a unix seconds
// integer. Rows are grouped by symbol and sorted chronologically.
func LoadCSV(path string) (map[string][]market.Candle, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	rd := csv.NewReader(f)
	rd.TrimLeadingSpace = true
	rows, err := rd.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) < 2 {
		return nil, fmt.Errorf("csv has no data rows")
	}

	col := make(map[string]int)
	for i, h := range rows[0] {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, req := range []string{"symbol", "time", "open", "high", "low", "close", "volume"} {
		if _, ok := col[req]; !ok {
			return nil, fmt.Errorf("csv missing required column %q", req)
		}
	}

	out := make(map[string][]market.Candle)
	for n, row := range rows[1:] {
		sym := strings.ToUpper(strings.TrimSpace(row[col["symbol"]]))
		if sym == "" {
			continue
		}
		t, err := parseTime(row[col["time"]])
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", n+2, err)
		}
		c := market.Candle{
			Symbol: sym, Time: t,
			Open:   atof(row[col["open"]]),
			High:   atof(row[col["high"]]),
			Low:    atof(row[col["low"]]),
			Close:  atof(row[col["close"]]),
			Volume: atof(row[col["volume"]]),
		}
		out[sym] = append(out[sym], c)
	}
	for sym := range out {
		series := out[sym]
		sort.Slice(series, func(i, j int) bool { return series[i].Time.Before(series[j].Time) })
		out[sym] = series
	}
	return out, nil
}

// SaveCSV writes candle data in the same format LoadCSV reads, so a generated
// or loaded dataset can be persisted once and reused (identically) across
// backtest, optimize and sensitivity runs. Rows are grouped by symbol and
// ordered chronologically; times are written as RFC3339.
func SaveCSV(path string, data map[string][]market.Candle) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{"symbol", "time", "open", "high", "low", "close", "volume"}); err != nil {
		return err
	}

	symbols := make([]string, 0, len(data))
	for s := range data {
		symbols = append(symbols, s)
	}
	sort.Strings(symbols)

	for _, sym := range symbols {
		candles := append([]market.Candle(nil), data[sym]...)
		sort.Slice(candles, func(i, j int) bool { return candles[i].Time.Before(candles[j].Time) })
		for _, c := range candles {
			rec := []string{
				sym,
				c.Time.UTC().Format(time.RFC3339Nano),
				ftoa(c.Open), ftoa(c.High), ftoa(c.Low), ftoa(c.Close), ftoa(c.Volume),
			}
			if err := w.Write(rec); err != nil {
				return err
			}
		}
	}
	w.Flush()
	return w.Error()
}

func ftoa(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// Generate produces synthetic OHLCV history for the given symbols using the
// offline mock provider, for testing the harness without real data.
func Generate(symbols []string, bars int) map[string][]market.Candle {
	m := market.NewMock(time.Millisecond)
	defer m.Close()
	_ = m.Subscribe(symbols...)
	to := time.Now()
	from := to.Add(-time.Duration(bars) * time.Minute)
	out := make(map[string][]market.Candle, len(symbols))
	for _, s := range symbols {
		c, _ := m.Candles(context.Background(), s, market.Res1Min, from, to)
		out[s] = c
	}
	return out
}

func parseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	if sec, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(sec, 0), nil
	}
	return time.Time{}, fmt.Errorf("unrecognised time %q", s)
}

func atof(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}
