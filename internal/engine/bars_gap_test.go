package engine

import (
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// A bar is only closed by the arrival of a later tick, so an instrument that
// trades a handful of times an hour used to produce a handful of bars an hour
// and call them one-minute bars.
//
// That is not a charting problem. Every indicator period in this bot counts
// BARS — trend_period is documented as "a week of 1-minute bars", the EMAs are
// 12 and 26, ATR is 14 — so on a thin instrument each silently covered far more
// time than it claimed. And warmup seeds from the provider's candles, which do
// include every interval, so one symbol's history changed shape partway along
// and the backtester measured a different strategy than the live engine ran.

// base is a bar-aligned starting point.
var base = time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)

// at returns base plus n minutes.
func at(n int) time.Time { return base.Add(time.Duration(n) * time.Minute) }

// feed applies ticks and returns every bar they completed.
func feed(b *barBuilder, ticks []struct {
	price float64
	min   int
},
) []market.Candle {
	var out []market.Candle

	for _, t := range ticks {
		out = append(out, b.add(t.price, 0, at(t.min))...)
	}

	return out
}

func TestQuietIntervalsProduceFlatBars(t *testing.T) {
	t.Parallel()

	b := newBarBuilder("THIN", time.Minute)

	// A tick at minute 0, then silence until minute 5.
	bars := feed(b, []struct {
		price float64
		min   int
	}{
		{100, 0},
		{101, 5},
	})

	if len(bars) != 5 {
		t.Fatalf("got %d bars for minutes 0..4, want 5 — quiet minutes must still "+
			"produce a bar, or every indicator period covers more time than it says",
			len(bars))
	}

	// Bar 0 is the real one; 1..4 are the filled gap.
	if bars[0].Close != 100 || bars[0].Volume != 1 {
		t.Errorf("first bar = %+v, want the traded bar at 100", bars[0])
	}

	for i := 1; i < len(bars); i++ {
		c := bars[i]

		if c.Open != 100 || c.High != 100 || c.Low != 100 || c.Close != 100 {
			t.Errorf("filled bar %d = O%v H%v L%v C%v, want all 100 (the last close)",
				i, c.Open, c.High, c.Low, c.Close)
		}

		if c.Volume != 0 {
			t.Errorf("filled bar %d has volume %v; nothing traded, so it must be 0",
				i, c.Volume)
		}
	}
}

// The whole point: consecutive bars are one interval apart, always.
func TestBarsAreEvenlySpaced(t *testing.T) {
	t.Parallel()

	b := newBarBuilder("THIN", time.Minute)

	bars := feed(b, []struct {
		price float64
		min   int
	}{
		{100, 0}, {101, 1}, {102, 9}, {103, 10}, {104, 25},
	})

	if len(bars) < 2 {
		t.Fatal("not enough bars to check spacing")
	}

	for i := 1; i < len(bars); i++ {
		if gap := bars[i].Time.Sub(bars[i-1].Time); gap != time.Minute {
			t.Fatalf("bars %d and %d are %v apart, want 1m", i-1, i, gap)
		}
	}
}

// A close is not silence. Filling 17 hours of it would invent bars for a shut
// venue and flush the ring of real history.
func TestSessionBreakIsNotFilled(t *testing.T) {
	t.Parallel()

	b := newBarBuilder("THIN", time.Minute)

	b.add(100, 0, at(0))

	// The next tick is the following morning.
	bars := b.add(101, 0, at(17*60+30))

	if len(bars) != 1 {
		t.Fatalf("got %d bars across an overnight gap, want 1 (the bar that was open) — "+
			"a closed market must not be filled", len(bars))
	}

	if bars[0].Close != 100 {
		t.Errorf("the completed bar = %+v, want the one that was open at 100", bars[0])
	}
}

// The boundary is stated once, in maxFillSpan, and has to hold.
func TestFillStopsAtMaxFillSpan(t *testing.T) {
	t.Parallel()

	within := newBarBuilder("THIN", time.Minute)

	within.add(100, 0, at(0))

	if got := len(within.add(101, 0, at(int(maxFillSpan/time.Minute)))); got == 1 {
		t.Errorf("a gap of exactly maxFillSpan produced only the open bar; "+
			"it is within the limit and should be filled (got %d)", got)
	}

	beyond := newBarBuilder("THIN", time.Minute)

	beyond.add(100, 0, at(0))

	if got := len(beyond.add(101, 0, at(int(maxFillSpan/time.Minute)+1))); got != 1 {
		t.Errorf("a gap past maxFillSpan produced %d bars, want 1 (no fill)", got)
	}
}

// Filling scales with the bar interval: on hourly bars, 90 minutes of silence is
// at most one missing bar, not ninety.
func TestFillScalesWithTheInterval(t *testing.T) {
	t.Parallel()

	b := newBarBuilder("THIN", time.Hour)

	b.add(100, 0, base)

	// Three hours later: beyond maxFillSpan, so no fill.
	if got := len(b.add(101, 0, base.Add(3*time.Hour))); got != 1 {
		t.Errorf("hourly bars three hours apart produced %d bars, want 1", got)
	}
}

// Ticks inside one interval still aggregate into a single bar.
func TestTicksWithinAnIntervalStillAggregate(t *testing.T) {
	t.Parallel()

	b := newBarBuilder("X", time.Minute)

	b.add(100, 0, at(0))
	b.add(105, 0, base.Add(10*time.Second))
	b.add(95, 0, base.Add(20*time.Second))

	bars := b.add(99, 0, at(1))
	if len(bars) != 1 {
		t.Fatalf("got %d bars, want 1", len(bars))
	}

	c := bars[0]
	if c.Open != 100 || c.High != 105 || c.Low != 95 || c.Close != 95 || c.Volume != 3 {
		t.Errorf("aggregated bar = %+v, want O100 H105 L95 C95 V3", c)
	}
}

// The first tick opens a bar and completes nothing.
func TestFirstTickCompletesNothing(t *testing.T) {
	t.Parallel()

	if bars := newBarBuilder("X", time.Minute).add(100, 0, base); len(bars) != 0 {
		t.Errorf("the first tick completed %d bar(s), want 0", len(bars))
	}
}

// TestBarVolumeUsesReportedTickSizeWhenAvailable reproduces the CRIT-13 fix:
// a provider that reports real per-tick trade size (Finnhub does; Binance's
// ticker-price poll does not) must have that size summed into bar volume
// instead of the bar volume being a flat tick count regardless of what the
// feed actually knows.
func TestBarVolumeUsesReportedTickSizeWhenAvailable(t *testing.T) {
	t.Parallel()

	b := newBarBuilder("X", time.Minute)

	b.add(100, 250, at(0)) // opens the bar: reported size 250
	b.add(101, 80, base.Add(10*time.Second))
	b.add(99, 170, base.Add(20*time.Second))

	bars := b.add(102, 0, at(1)) // next bucket, completing the first bar
	if len(bars) != 1 {
		t.Fatalf("got %d bars, want 1", len(bars))
	}

	if got, want := bars[0].Volume, 500.0; got != want {
		t.Errorf("volume = %v, want %v (250+80+170 — real reported trade size, not a tick count of 3)", got, want)
	}
}

// TestBarVolumeFallsBackToTickCountWhenUnreported checks a provider that
// reports no per-tick size (volume 0, e.g. Binance's ticker-price poll)
// still gets the old tick-count behavior, unchanged.
func TestBarVolumeFallsBackToTickCountWhenUnreported(t *testing.T) {
	t.Parallel()

	b := newBarBuilder("X", time.Minute)

	b.add(100, 0, at(0))
	b.add(101, 0, base.Add(10*time.Second))
	b.add(99, 0, base.Add(20*time.Second))

	bars := b.add(102, 0, at(1))
	if len(bars) != 1 {
		t.Fatalf("got %d bars, want 1", len(bars))
	}

	if got, want := bars[0].Volume, 3.0; got != want {
		t.Errorf("volume = %v, want %v (tick count fallback)", got, want)
	}
}

// The whole series, as the engine assembles it, must be evenly spaced — this is
// what the indicators and the backtester both assume.
func TestStoredSeriesIsEvenlySpaced(t *testing.T) {
	t.Parallel()

	s := newBarStore(3000, time.Minute)

	s.add("THIN")

	for _, min := range []int{0, 1, 7, 8, 20, 21} {
		s.ingest("THIN", 100+float64(min), 0, at(min))
	}

	series := s.series("THIN")
	if len(series) < 20 {
		t.Fatalf("stored %d bars for a 21-minute span, want the gaps filled", len(series))
	}

	for i := 1; i < len(series); i++ {
		if gap := series[i].Time.Sub(series[i-1].Time); gap != time.Minute {
			t.Fatalf("stored bars %d and %d are %v apart, want 1m", i-1, i, gap)
		}
	}
}

// TestLateTickDoesNotOverwriteTheOpenBar reproduces the EN-4 finding. Feeds
// that carry the venue's own trade timestamp — Finnhub batches trades and
// stamps each with the exchange clock — can deliver them out of order. Folding
// such a tick into the CURRENT bar made a stale price that bar's close and
// dragged its high/low, so the series no longer described what the market did.
func TestLateTickDoesNotOverwriteTheOpenBar(t *testing.T) {
	t.Parallel()

	b := newBarBuilder("X", time.Minute)

	b.add(100, 0, at(0))
	b.add(110, 0, base.Add(61*time.Second)) // opens the 10:01 bar
	b.add(111, 0, base.Add(70*time.Second)) // trades up to 111

	// A trade that actually happened inside the PREVIOUS bar, delivered late.
	if got := b.add(99, 0, base.Add(30*time.Second)); got != nil {
		t.Errorf("a late tick completed %d bars, want none", len(got))
	}

	cur, ok := b.current()
	if !ok {
		t.Fatal("no in-progress bar")
	}

	if cur.Close != 111 {
		t.Errorf("close = %v, want 111 — a stale tick must not become the bar's close", cur.Close)
	}

	if cur.Low != 110 {
		t.Errorf("low = %v, want 110 — a stale tick must not drag the bar's range", cur.Low)
	}
}

// TestZeroTimestampTickIsRejected reproduces the EN-5 finding: Finnhub
// occasionally reports t=0, which time.UnixMilli turns into 1970-01-01. Such a
// tick used to open a bar stamped 1970 that the next real tick then closed and
// pushed into the ring, leaving a non-monotonic series — which market.Resample
// ("input is assumed chronologically ordered") and internal/autotune (which
// anchors its comparison window on candles[0].Time) both assume cannot happen.
func TestZeroTimestampTickIsRejected(t *testing.T) {
	t.Parallel()

	bs := newBarStore(100, time.Minute)
	bs.add("X")

	bs.seed("X", []market.Candle{
		{Symbol: "X", Close: 100, Time: base.Add(-2 * time.Minute)},
		{Symbol: "X", Close: 101, Time: base.Add(-1 * time.Minute)},
	})

	bs.ingest("X", 102, 0, time.UnixMilli(0))
	bs.ingest("X", 103, 0, base.Add(time.Minute))

	series := bs.series("X")
	for i := 1; i < len(series); i++ {
		if series[i].Time.Before(series[i-1].Time) {
			t.Fatalf("series is not chronological: %s follows %s (bar %d of %d)",
				series[i].Time.UTC(), series[i-1].Time.UTC(), i, len(series))
		}
	}
}

// A replayed session streams historical candles as live quotes stamped with
// their ORIGINAL times (market.Replay), so the timestamp guard must not reject
// an old-but-real tick — only a zero one.
func TestReplayAgedTimestampsAreAccepted(t *testing.T) {
	t.Parallel()

	old := time.Date(2020, 3, 2, 14, 30, 0, 0, time.UTC)

	bs := newBarStore(100, time.Minute)
	bs.add("X")

	bs.ingest("X", 100, 0, old)

	if !bs.ingest("X", 101, 0, old.Add(time.Minute)) {
		t.Error("a replayed tick years in the past was rejected; Replay streams exactly these")
	}
}
