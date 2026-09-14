package screener

import (
	"context"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/strategy/indicators"
)

// synthBounce builds n bars as a random walk around price, with a given intrabar
// range and a bid-ask bounce of spread (as a fraction of price).
//
// The walk matters. A constant price makes the estimator's job trivial, because
// then the whole high-low range IS the spread. Separating a genuine spread from
// the volatility around it is the only thing Corwin-Schultz is for, so the test
// data has to contain both.
func synthBounce(n int, price, rangePct, spread float64) []market.Candle {
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic test data, not cryptography
	out := make([]market.Candle, n)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := range out {
		// A random walk keeps the series from being degenerate; the high/low
		// range carries the volatility and the spread carries the bounce.
		price *= 1 + (rng.Float64()-0.5)*0.004

		half := price * rangePct / 2
		out[i] = market.Candle{
			Time:   start.Add(time.Duration(i) * time.Minute),
			Open:   price,
			High:   price + half + price*spread/2,
			Low:    price - half - price*spread/2,
			Close:  price,
			Volume: 100000,
		}
	}

	return out
}

// ----- item 5: the spread estimator -----

// A wider bid-ask bounce must produce a larger estimate. The absolute value of
// the Corwin–Schultz estimate is noisy on synthetic data; its ordering is what
// the filter depends on.
func TestSpreadEstimateOrdersByRealSpread(t *testing.T) {
	tight := synthBounce(400, 100, 0.004, 0.0002)
	wide := synthBounce(400, 100, 0.004, 0.01)

	tightSpread := spreadOf(t, tight)
	wideSpread := spreadOf(t, wide)

	if wideSpread <= tightSpread {
		t.Fatalf("estimator did not separate the two: wide %.5f <= tight %.5f",
			wideSpread, tightSpread)
	}
}

// A negative single-pair estimate is volatility noise, not a negative spread.
func TestSpreadEstimateIsNeverNegative(t *testing.T) {
	c := synthBounce(200, 50, 0.02, 0)

	high := make([]float64, len(c))
	low := make([]float64, len(c))

	for i, x := range c {
		high[i], low[i] = x.High, x.Low
	}

	s, ok := indicators.HighLowSpread(high, low)
	if !ok {
		t.Fatal("estimator returned no result on 200 usable bars")
	}

	if s < 0 || math.IsNaN(s) {
		t.Fatalf("spread estimate %v is not a usable fraction", s)
	}
}

func TestSpreadEstimateNeedsTwoBars(t *testing.T) {
	if _, ok := indicators.HighLowSpread([]float64{10}, []float64{9}); ok {
		t.Error("estimator claimed a result from a single bar")
	}
}

// spreadOf runs the estimator over a candle series.
func spreadOf(t *testing.T, c []market.Candle) float64 {
	t.Helper()

	high := make([]float64, len(c))
	low := make([]float64, len(c))

	for i, x := range c {
		high[i], low[i] = x.High, x.Low
	}

	s, ok := indicators.HighLowSpread(high, low)
	if !ok {
		t.Fatal("estimator returned no result")
	}

	return s
}

// The spread filter drops an instrument whose cost of entry it cannot clear.
func TestSpreadFilterRejectsWideSpreads(t *testing.T) {
	hist := fakeHistory{series: map[string][]market.Candle{
		"TIGHT": synthBounce(400, 100, 0.004, 0.0002),
		"WIDE":  synthBounce(400, 100, 0.004, 0.02),
	}}

	// Set the bound between the two measured values so the test asserts the
	// filter's behaviour rather than the estimator's absolute calibration.
	bound := (spreadOf(t, hist.series["TIGHT"]) + spreadOf(t, hist.series["WIDE"])) / 2

	s := New(config.ScreenerConfig{
		Enabled: true, KeepHeld: true, MaxSpreadPct: bound,
	}, hist, market.Res1Min, Options{})

	res := s.Screen(context.Background(), Inputs{Current: []string{"TIGHT", "WIDE"}})

	if len(res.Selected) != 1 || res.Selected[0] != "TIGHT" {
		t.Fatalf("selected %v, want only TIGHT", res.Selected)
	}

	for _, v := range res.Verdicts {
		if v.Symbol == "WIDE" && v.SpreadPct <= 0 {
			t.Error("the rejected symbol's measured spread was not reported")
		}
	}
}

// TestSpreadFilterFailsClosedWhenUnmeasurable reproduces the "the spread
// filter fails open" finding: HighLowSpread returns ok=false for a broken
// feed (every bar has high < low, which csPair refuses), and the filter
// used to read the resulting zero-value SpreadPct as "spread == 0%",
// indistinguishable from a megacap's genuinely tight spread — passing the
// one filter meant to catch untradeable instruments. It must instead refuse
// a candidate whose spread could not be measured, when a cap was actually
// configured.
func TestSpreadFilterFailsClosedWhenUnmeasurable(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	broken := make([]market.Candle, 400)

	for i := range broken {
		// high < low on every bar: csPair refuses every pair, so
		// HighLowSpread returns ok=false across the whole series. Close
		// stays a normal, valid price so nothing else about the candidate
		// looks obviously wrong.
		broken[i] = market.Candle{
			Time: base.Add(time.Duration(i) * time.Minute),
			Open: 100, High: 1, Low: 2, Close: 100, Volume: 100000,
		}
	}

	hist := fakeHistory{series: map[string][]market.Candle{"BROKEN": broken}}

	s := New(config.ScreenerConfig{
		Enabled: true, KeepHeld: true, MaxSpreadPct: 0.05,
		Filters: []string{"spread"}, // isolate the filter under test
	}, hist, market.Res1Min, Options{})

	res := s.Screen(context.Background(), Inputs{Current: []string{"BROKEN"}})

	if len(res.Selected) != 0 {
		t.Fatalf("selected %v; an unmeasurable spread must be refused, not silently passed",
			res.Selected)
	}
}

// With no spread cap configured, an unmeasurable spread must not block a
// candidate — the filter is a no-op unless it was actually asked to enforce
// something.
func TestSpreadFilterUnmeasurableIsFineWithNoCapConfigured(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	broken := make([]market.Candle, 400)

	for i := range broken {
		broken[i] = market.Candle{
			Time: base.Add(time.Duration(i) * time.Minute),
			Open: 100, High: 1, Low: 2, Close: 100, Volume: 100000,
		}
	}

	hist := fakeHistory{series: map[string][]market.Candle{"BROKEN": broken}}

	s := New(config.ScreenerConfig{
		Enabled: true, KeepHeld: true, // MaxSpreadPct left at 0 (disabled)
		Filters: []string{"spread"},
	}, hist, market.Res1Min, Options{})

	res := s.Screen(context.Background(), Inputs{Current: []string{"BROKEN"}})

	if len(res.Selected) != 1 {
		t.Fatalf("selected %v; the spread filter is disabled with MaxSpreadPct: 0", res.Selected)
	}
}

// ----- item 7: the filter chain -----

// A filter left out of the chain does not run, even with its bound configured.
func TestChainOmitsUnlistedFilters(t *testing.T) {
	hist := fakeHistory{series: map[string][]market.Candle{"X": synthBounce(400, 5, 0.004, 0.0002)}}

	cfg := config.ScreenerConfig{
		Enabled: true, KeepHeld: true,
		MinPrice: 100,                 // X trades near 5, so this would reject it
		Filters:  []string{"history"}, // ...but price is not in the chain
	}

	res := New(
		cfg,
		hist,
		market.Res1Min,
		Options{},
	).Screen(context.Background(), Inputs{Current: []string{"X"}})
	if len(res.Selected) != 1 {
		t.Fatalf("selected %v; a filter not in the chain must not run", res.Selected)
	}

	// And with it listed, it does.
	cfg.Filters = []string{"history", "price"}

	res = New(
		cfg,
		hist,
		market.Res1Min,
		Options{},
	).Screen(context.Background(), Inputs{Current: []string{"X"}})
	if len(res.Selected) != 0 {
		t.Fatalf("selected %v; the price filter should have rejected X", res.Selected)
	}
}

// The reason names the binding constraint, not a list of everything wrong.
func TestChainReportsTheFirstFailure(t *testing.T) {
	hist := fakeHistory{series: map[string][]market.Candle{"X": synthBounce(20, 5, 0.004, 0.0002)}}

	cfg := config.ScreenerConfig{
		Enabled: true, MinHistoryBars: 300, MinPrice: 100,
		Filters: []string{"history", "price"},
	}

	res := New(
		cfg,
		hist,
		market.Res1Min,
		Options{},
	).Screen(context.Background(), Inputs{Current: []string{"X"}})
	if len(res.Verdicts) != 1 {
		t.Fatalf("expected one verdict, got %d", len(res.Verdicts))
	}

	if got := res.Verdicts[0].Reason; got == "" || got[:4] != "only" {
		t.Errorf("reason = %q, want the history failure (the first in the chain)", got)
	}
}

// Reordering the chain changes which constraint is reported.
func TestChainOrderDecidesTheReportedReason(t *testing.T) {
	hist := fakeHistory{series: map[string][]market.Candle{"X": synthBounce(20, 5, 0.004, 0.0002)}}

	cfg := config.ScreenerConfig{
		Enabled: true, MinHistoryBars: 300, MinPrice: 100,
		Filters: []string{"price", "history"},
	}

	res := New(
		cfg,
		hist,
		market.Res1Min,
		Options{},
	).Screen(context.Background(), Inputs{Current: []string{"X"}})
	if got := res.Verdicts[0].Reason; got == "" || got[:5] != "price" {
		t.Errorf("reason = %q, want the price failure (now first)", got)
	}
}

// A typo in the chain must not take a running bot down.
func TestUnknownFilterIsIgnored(t *testing.T) {
	hist := fakeHistory{
		series: map[string][]market.Candle{"X": synthBounce(400, 50, 0.004, 0.0002)},
	}

	cfg := config.ScreenerConfig{
		Enabled: true, KeepHeld: true, Filters: []string{"histroy", "price"},
	}

	res := New(
		cfg,
		hist,
		market.Res1Min,
		Options{},
	).Screen(context.Background(), Inputs{Current: []string{"X"}})
	if len(res.Selected) != 1 {
		t.Fatalf("selected %v; a misspelled filter should be skipped, not fatal", res.Selected)
	}
}

// An empty chain means every filter, in the default order.
func TestEmptyChainUsesEveryFilter(t *testing.T) {
	hist := fakeHistory{series: map[string][]market.Candle{"X": synthBounce(400, 5, 0.004, 0.0002)}}

	cfg := config.ScreenerConfig{Enabled: true, MinPrice: 100}

	res := New(
		cfg,
		hist,
		market.Res1Min,
		Options{},
	).Screen(context.Background(), Inputs{Current: []string{"X"}})
	if len(res.Selected) != 0 {
		t.Fatalf("selected %v; the default chain includes the price filter", res.Selected)
	}
}

func TestFilterNamesCoversTheDefaultOrder(t *testing.T) {
	have := make(map[string]bool)
	for _, n := range FilterNames() {
		have[n] = true
	}

	for _, n := range DefaultFilterOrder {
		if !have[n] {
			t.Errorf("default chain names %q, which the registry does not have", n)
		}
	}
}
