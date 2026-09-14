package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// spacedHistory serves a candle series at a chosen bar spacing, so a test can
// hand venuePrice the daily/hourly data a history source silently substitutes
// when the fine interval has too few bars.
type spacedHistory struct {
	spacing time.Duration
	bars    int
	close   float64
}

func (h spacedHistory) Candles(
	_ context.Context, _ string, _ market.Resolution, _, _ time.Time,
) ([]market.Candle, error) {
	out := make([]market.Candle, h.bars)
	start := time.Now().Add(-time.Duration(h.bars) * h.spacing)

	for i := range out {
		out[i] = market.Candle{
			Close: h.close,
			Time:  start.Add(time.Duration(i) * h.spacing),
		}
	}

	return out, nil
}

// venuePrice asks for 1-minute bars, but market.YahooHistory answers a thin
// series by silently falling back to hourly and then daily candles. Treating a
// daily close as "the venue's current price" is what produced a 3.5% deviation
// block on a price that was simply a day old — sizing an order on it would have
// been a real mispricing, so it must be refused outright.
func TestVenuePriceRejectsCoarserThanIntradayData(t *testing.T) {
	t.Parallel()

	for name, spacing := range map[string]time.Duration{
		"hourly": time.Hour,
		"daily":  24 * time.Hour,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			e := fxTestEngine(plainBroker{})
			e.history = spacedHistory{spacing: spacing, bars: 100, close: 143.28}

			_, reason, ok := e.venuePrice("ENR.DE")
			if ok {
				t.Fatalf("%s bars were accepted as a current venue price", name)
			}

			if !strings.Contains(reason, "not a current price") {
				t.Errorf("reason %q does not explain the granularity problem", reason)
			}
		})
	}
}

// Genuine 1-minute data is accepted, including the multi-minute holes a thinly
// traded instrument leaves in it — the inference takes the series' FINEST gap
// precisely so those holes cannot make real minute data look hourly.
func TestVenuePriceAcceptsGappyMinuteData(t *testing.T) {
	t.Parallel()

	e := fxTestEngine(plainBroker{})
	e.history = gappyMinuteHistory{}

	got, reason, ok := e.venuePrice("SAP.DE")
	if !ok {
		t.Fatalf("gappy 1-minute data was rejected: %s", reason)
	}

	if got != 100 {
		t.Errorf("venuePrice = %v, want 100", got)
	}
}

// gappyMinuteHistory is minute data with a long hole in the middle, the shape
// an illiquid instrument's intraday series actually has.
type gappyMinuteHistory struct{}

func (gappyMinuteHistory) Candles(
	_ context.Context, _ string, _ market.Resolution, _, _ time.Time,
) ([]market.Candle, error) {
	base := time.Now().Add(-2 * time.Hour)

	return []market.Candle{
		{Close: 100, Time: base},
		{Close: 100, Time: base.Add(time.Minute)},      // one real minute step
		{Close: 100, Time: base.Add(90 * time.Minute)}, // then nothing for 90 min
		{Close: 100, Time: base.Add(91 * time.Minute)},
	}, nil
}

// The block reason a refused buy records must say WHY the venue price could not
// be confirmed — "unavailable" and "that was daily data" send an operator to
// completely different places.
func TestConfirmVenuePriceSurfacesTheGranularityReason(t *testing.T) {
	t.Parallel()

	e := fxTestEngine(plainBroker{})
	e.cfg = config.EngineConfig{VenuePriceCheck: true, MaxPriceDeviation: 0.02}
	e.history = spacedHistory{spacing: 24 * time.Hour, bars: 100, close: 143.28}

	_, reason, ok := e.confirmVenuePrice("ENR.DE", 148.48)
	if ok {
		t.Fatal("a daily close was accepted for a buy")
	}

	if !strings.Contains(reason, "not a current price") {
		t.Errorf("reason %q does not name the granularity problem", reason)
	}
}

// finestBarGap is the whole basis of the check, so its edge cases matter.
func TestFinestBarGap(t *testing.T) {
	t.Parallel()

	at := func(mins ...int) []market.Candle {
		base := time.Now()
		out := make([]market.Candle, len(mins))

		for i, m := range mins {
			out[i] = market.Candle{Time: base.Add(time.Duration(m) * time.Minute)}
		}

		return out
	}

	if _, ok := finestBarGap(at(0)); ok {
		t.Error("a single bar reported a measurable interval")
	}

	if got, ok := finestBarGap(at(0, 60, 61, 180)); !ok || got != time.Minute {
		t.Errorf("finestBarGap = %v (ok=%v), want 1m — the smallest gap, not the average", got, ok)
	}

	// Duplicate timestamps carry no spacing information and must not be read
	// as a zero-length interval.
	if got, ok := finestBarGap(at(0, 0, 60)); !ok || got != time.Hour {
		t.Errorf("finestBarGap = %v (ok=%v), want 1h with duplicate stamps ignored", got, ok)
	}
}
