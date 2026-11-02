package screener

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// spaced builds n flat bars step apart at the given volume.
func spaced(n int, step time.Duration, volume float64) []market.Candle {
	out := make([]market.Candle, n)
	base := time.Date(2026, 1, 5, 8, 0, 0, 0, time.UTC)

	for i := range out {
		out[i] = market.Candle{
			Time: base.Add(time.Duration(i) * step),
			Open: 100, High: 101, Low: 99, Close: 100, Volume: volume,
		}
	}

	return out
}

// SC3-3 / MK3-4: Yahoo falls back to hourly or daily bars when it has too few
// at the requested interval, and the screener judged them as the requested
// resolution — 4× the volume per bar and a bar count that clears
// min_history_bars. THIN, with too few 15-minute bars to qualify, passed on
// 400 substituted hourly bars, labelled "15", and was adopted. Too few bars at
// the requested resolution is the answer.
func TestCoarserServedBarsAreNotJudgedAsTheRequestedResolution(t *testing.T) {
	t.Parallel()

	h := fakeHistory{series: map[string][]market.Candle{
		"THIN": spaced(400, time.Hour, 4000),
		"GOOD": spaced(400, 15*time.Minute, 4000),
	}}

	s := newScreener(t, config.ScreenerConfig{
		Resolution: "15", LookbackDays: 30, MinHistoryBars: 100, MinAvgVolume: 2000,
		Filters: []string{"history", "volume"},
	}, h)

	res := s.Screen(context.Background(), Inputs{Current: []string{"THIN", "GOOD"}})

	for _, v := range res.Verdicts {
		switch v.Symbol {
		case "THIN":
			if v.Passed {
				t.Errorf("THIN passed a 15-minute screen on hourly bars: %s", v.Reason)
			}

			if !strings.Contains(v.Reason, "1h0m0s") {
				t.Errorf("THIN's reason %q does not name the resolution actually served", v.Reason)
			}
		case "GOOD":
			if !v.Passed {
				t.Errorf("GOOD, served the 15-minute bars it asked for, was refused: %s", v.Reason)
			}
		}
	}
}
