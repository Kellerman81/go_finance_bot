package screener

import (
	"context"
	"math"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// TestAFailedFetchIsUnmeasuredNotRejected is the screener half of SC2-3: a
// history fetch that fails, or returns nothing, is a provider problem, and
// the verdict must say so (Unmeasured) so the engine can keep the symbol
// rather than drop it as if it had been measured and found wanting.
func TestAFailedFetchIsUnmeasuredNotRejected(t *testing.T) {
	t.Parallel()

	cfg := config.ScreenerConfig{Enabled: true}
	s := New(cfg, fakeHistory{fail: map[string]bool{"AAA": true}, series: map[string][]market.Candle{}},
		market.Res1Min, Options{})

	res := s.Screen(context.Background(), Inputs{Current: []string{"AAA", "BBB"}})

	for _, v := range res.Verdicts {
		if !v.Unmeasured {
			t.Errorf("%s: no history (%s) but Unmeasured is false", v.Symbol, v.Reason)
		}
	}
}

// classifier answers "" for FLAKY on the first call only — a provider lookup
// that fails once and then succeeds.
type flakyClassifier struct{ calls map[string]int }

func (f *flakyClassifier) AssetClass(_ context.Context, sym string) string {
	f.calls[sym]++

	switch sym {
	case "ETF1":
		return "etf"
	case "FLAKY":
		if f.calls[sym] == 1 {
			return ""
		}

		return "etf"
	}

	return ""
}

// TestEverySymbolIsScreenedOnce is the regression for SC2-4. The bucketing
// line called classOf on both sides of the assignment; when the first call
// failed and the second succeeded, the unclassified bucket was overwritten
// with the etf one — the unclassified symbol got no verdict at all, and the
// etf symbols were screened and selected twice ([ETF1 FLAKY ETF1]).
func TestEverySymbolIsScreenedOnce(t *testing.T) {
	t.Parallel()

	series := map[string][]market.Candle{
		"ETF1":  trending(300, 100, 0.05, 0.01, 1e6),
		"FLAKY": trending(300, 100, 0.05, 0.01, 1e6),
		"AAA":   trending(300, 100, 0.05, 0.01, 1e6),
	}
	cfg := config.ScreenerConfig{Enabled: true, Universe: []string{"AAA", "ETF1", "FLAKY"}, Filters: []string{"history"}}

	s := New(cfg, fakeHistory{series: series}, market.Res1Min,
		Options{Classify: &flakyClassifier{calls: map[string]int{}}})
	res := s.Screen(context.Background(), Inputs{})

	seen := map[string]int{}
	for _, sym := range res.Selected {
		seen[sym]++
	}

	for _, sym := range []string{"AAA", "ETF1", "FLAKY"} {
		if seen[sym] != 1 {
			t.Errorf("%s selected %d times, want once (selected %v)", sym, seen[sym], res.Selected)
		}
	}
}

// TestRoundTripCostCountsTheSpreadOnce is the regression for SC2-5.
// Corwin-Schultz S is the full spread and each fill pays half of it — how the
// backtester and the simulated broker charge it — so a round trip costs S, not
// 2S. Counting it twice made min_cost_multiple: 3 demand a 6x move on a
// commission-free broker. Slippage, which the old formula ignored, is paid on
// both legs.
func TestRoundTripCostCountsTheSpreadOnce(t *testing.T) {
	t.Parallel()

	m := Measures{Price: 100, SpreadPct: 0.01, SpreadOK: true}
	m.measureCost(series{closes: []float64{100, 101, 100}}, config.CostsConfig{SlippagePct: 0.001})

	if math.Abs(m.RoundTripCostPct-0.012) > 1e-12 {
		t.Errorf("round trip = %v for a 1%% spread and 0.1%% slippage, want 0.012 (S + 2×slippage)", m.RoundTripCostPct)
	}
}

// TestTypicalMoveCountsUnchangedBars is the regression for SC2-6: dropping
// zero moves before the median inflated exactly the stale listings the cost
// filter exists to catch — 80% unchanged bars and 20% at ±0.5% read as a
// 0.5% typical move instead of 0.
func TestTypicalMoveCountsUnchangedBars(t *testing.T) {
	t.Parallel()

	px := 100.0
	closes := []float64{px}

	for i := range 100 {
		if i%5 == 0 {
			if (i/5)%2 == 0 {
				px *= 1.005
			} else {
				px /= 1.005
			}
		}

		closes = append(closes, px)
	}

	if got := medianAbsReturn(closes); got != 0 {
		t.Errorf("median |return| = %v for a series that is unchanged on 80%% of bars, want 0", got)
	}
}
