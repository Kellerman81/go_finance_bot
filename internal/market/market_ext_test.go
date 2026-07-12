package market

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// countingFetcher records how many times Candles is called and returns a fixed
// series (or an error).
type countingFetcher struct {
	calls   int64
	candles []Candle
	err     error
}

func (c *countingFetcher) Candles(_ context.Context, _ string, _ Resolution, from, to time.Time) ([]Candle, error) {
	atomic.AddInt64(&c.calls, 1)
	if c.err != nil {
		return nil, c.err
	}
	return c.candles, nil
}

func minuteCandles(n int, start time.Time) []Candle {
	out := make([]Candle, n)
	for i := 0; i < n; i++ {
		out[i] = Candle{Close: float64(100 + i), Time: start.Add(time.Duration(i) * time.Minute)}
	}
	return out
}

func TestCachingHistoryServesWithinTTL(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	src := &countingFetcher{candles: minuteCandles(60, start)}
	c := NewCachingHistory(src, time.Minute)
	from, to := start, start.Add(time.Hour)

	if _, err := c.Candles(context.Background(), "AAPL", Res1Min, from, to); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Candles(context.Background(), "AAPL", Res1Min, from, to); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&src.calls); got != 1 {
		t.Errorf("cache made %d upstream calls, want 1 (second served from cache)", got)
	}
	// A different symbol is a distinct key => new fetch.
	if _, err := c.Candles(context.Background(), "MSFT", Res1Min, from, to); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&src.calls); got != 2 {
		t.Errorf("distinct symbol => %d calls, want 2", got)
	}
	// Invalidate forces a refetch.
	c.Invalidate("AAPL", Res1Min)
	if _, err := c.Candles(context.Background(), "AAPL", Res1Min, from, to); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&src.calls); got != 3 {
		t.Errorf("after invalidate => %d calls, want 3", got)
	}
}

func TestCachingHistorySlicesToWindow(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	src := &countingFetcher{candles: minuteCandles(60, start)}
	c := NewCachingHistory(src, time.Minute)
	// Prime the cache with the full hour.
	if _, err := c.Candles(context.Background(), "AAPL", Res1Min, start, start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// A narrower sub-window is served from cache, sliced to [from,to].
	from := start.Add(10 * time.Minute)
	to := start.Add(20 * time.Minute)
	got, err := c.Candles(context.Background(), "AAPL", Res1Min, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&src.calls) != 1 {
		t.Errorf("sub-window should be served from cache, got %d calls", src.calls)
	}
	for _, cd := range got {
		if cd.Time.Before(from) || cd.Time.After(to) {
			t.Fatalf("candle %v outside requested window [%v,%v]", cd.Time, from, to)
		}
	}
	if len(got) != 11 { // minutes 10..20 inclusive
		t.Errorf("sliced window returned %d candles, want 11", len(got))
	}
}

func TestFallbackHistoryOrder(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	primary := &countingFetcher{err: errors.New("primary down")}
	secondary := &countingFetcher{candles: minuteCandles(5, start)}
	fb := NewFallbackHistory(primary, secondary)

	got, err := fb.Candles(context.Background(), "AAPL", Res1Min, start, start.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Errorf("fallback returned %d candles, want 5 from secondary", len(got))
	}
	if primary.calls != 1 || secondary.calls != 1 {
		t.Errorf("expected both sources tried once, got primary=%d secondary=%d", primary.calls, secondary.calls)
	}

	// Empty (not error) also falls through to the next source.
	empty := &countingFetcher{candles: nil}
	fb2 := NewFallbackHistory(empty, secondary)
	if _, err := fb2.Candles(context.Background(), "AAPL", Res1Min, start, start.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	// All sources empty => error.
	fb3 := NewFallbackHistory(&countingFetcher{}, &countingFetcher{})
	if _, err := fb3.Candles(context.Background(), "AAPL", Res1Min, start, start.Add(time.Hour)); err == nil {
		t.Error("expected error when every source is empty")
	}
}

func TestResampleAggregatesBuckets(t *testing.T) {
	start := time.Date(2024, 1, 1, 9, 30, 0, 0, time.UTC)
	// 10 one-minute candles: closes 100..109, each volume 1.
	in := make([]Candle, 10)
	for i := 0; i < 10; i++ {
		p := float64(100 + i)
		in[i] = Candle{Open: p, High: p + 0.5, Low: p - 0.5, Close: p, Volume: 1, Time: start.Add(time.Duration(i) * time.Minute)}
	}
	out := Resample(in, Res5Min)
	if len(out) != 2 {
		t.Fatalf("10x1m into 5m => %d bars, want 2", len(out))
	}
	// First bucket: open of bar0 (100), close of bar4 (104), high 104.5, low 99.5, volume 5.
	b := out[0]
	if b.Open != 100 || b.Close != 104 {
		t.Errorf("bucket0 open/close = %.1f/%.1f, want 100/104", b.Open, b.Close)
	}
	if b.High != 104.5 || b.Low != 99.5 {
		t.Errorf("bucket0 high/low = %.1f/%.1f, want 104.5/99.5", b.High, b.Low)
	}
	if b.Volume != 5 {
		t.Errorf("bucket0 volume = %.0f, want 5", b.Volume)
	}
}

func TestReplayStreamsAndServesHistory(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	data := map[string][]Candle{"AAPL": minuteCandles(30, start)}
	r := NewReplay(data, 5*time.Millisecond)
	defer r.Close()

	if err := r.Subscribe("AAPL"); err != nil {
		t.Fatal(err)
	}
	// History is served through the same interface.
	hist, err := r.Candles(context.Background(), "AAPL", Res1Min, start, start.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 30 {
		t.Errorf("replay history = %d candles, want 30", len(hist))
	}
	// A few ticks stream, and LastQuote reflects the latest.
	select {
	case q := <-r.Quotes():
		if q.Symbol != "AAPL" || q.Price <= 0 {
			t.Errorf("bad tick %+v", q)
		}
	case <-time.After(time.Second):
		t.Fatal("no tick emitted within 1s")
	}
	// Give the streamer a moment, then LastQuote should be set.
	time.Sleep(20 * time.Millisecond)
	if _, ok := r.LastQuote("AAPL"); !ok {
		t.Error("LastQuote not set after streaming")
	}
}

func TestMockLastQuote(t *testing.T) {
	m := NewMock(2 * time.Millisecond)
	defer m.Close()
	_ = m.Subscribe("AAPL")
	deadline := time.After(time.Second)
	for {
		if _, ok := m.LastQuote("AAPL"); ok {
			return
		}
		select {
		case <-deadline:
			t.Fatal("Mock.LastQuote never set")
		case <-time.After(2 * time.Millisecond):
		}
	}
}
