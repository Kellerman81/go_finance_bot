package backtest

import (
	"math"
	"testing"
	"time"
)

// mcTrips builds n round trips alternating between the given win and loss P&Ls.
func mcTrips(n int, win, loss float64) []RoundTrip {
	out := make([]RoundTrip, n)
	for i := range out {
		pnl := win
		if i%2 == 1 {
			pnl = loss
		}

		out[i] = trip("TST", pnl, pnl/100, ExitStrategy, time.Hour)
	}

	return out
}

// TestBootstrapIsDeterministic checks a fixed seed reproduces the distribution —
// what makes a reported confidence interval something you can re-check.
func TestBootstrapIsDeterministic(t *testing.T) {
	t.Parallel()

	trips := mcTrips(40, 120, -80)

	a := Bootstrap(trips, 100000, 500, 0.3, 7)
	b := Bootstrap(trips, 100000, 500, 0.3, 7)

	if a == nil || b == nil {
		t.Fatal("Bootstrap returned nil for 40 trades")
	}

	if *a != *b {
		t.Error("bootstrap is not reproducible for a fixed seed")
	}
}

// TestBootstrapOrdersPercentiles checks the distribution is internally coherent:
// percentiles ascend, and drawdown percentiles ascend toward the worst case.
func TestBootstrapOrdersPercentiles(t *testing.T) {
	t.Parallel()

	mc := Bootstrap(mcTrips(60, 150, -100), 100000, 1000, 0.3, 3)
	if mc == nil {
		t.Fatal("nil result")
	}

	if !(mc.ReturnP5 <= mc.ReturnP25 &&
		mc.ReturnP25 <= mc.ReturnP50 &&
		mc.ReturnP50 <= mc.ReturnP75 &&
		mc.ReturnP75 <= mc.ReturnP95) {
		t.Errorf("return percentiles not ascending: %+v", mc)
	}

	if !(mc.MaxDDP50 <= mc.MaxDDP95 &&
		mc.MaxDDP95 <= mc.MaxDDP99 &&
		mc.MaxDDP99 <= mc.MaxDDWorst) {
		t.Errorf("drawdown percentiles not ascending: %+v", mc)
	}

	if mc.ProbProfit < 0 || mc.ProbProfit > 1 {
		t.Errorf("prob profit = %v, want a fraction", mc.ProbProfit)
	}

	if mc.Trades != 60 || mc.Runs != 1000 {
		t.Errorf("mc = %+v, want 60 trades over 1000 runs", mc)
	}
}

// TestBootstrapSpreadsAroundTheRealisedResult checks the median resampled return
// lands near the actual one — a bootstrap that systematically disagreed with the
// sample it drew from would be measuring nothing.
func TestBootstrapSpreadsAroundTheRealisedResult(t *testing.T) {
	t.Parallel()

	// 50 trades averaging +25 => +1250 on 100k => +1.25%.
	trips := mcTrips(50, 150, -100)
	mc := Bootstrap(trips, 100000, 4000, 0.3, 11)

	var sum float64

	for _, tr := range trips {
		sum += tr.PnL
	}

	realised := sum / 100000 * 100

	if math.Abs(mc.ReturnP50-realised) > 1.0 {
		t.Errorf("median resampled return %.2f%% is far from the realised %.2f%%",
			mc.ReturnP50, realised)
	}

	// A distribution with no width would mean the resampling did nothing.
	if mc.ReturnP95-mc.ReturnP5 <= 0 {
		t.Error("the distribution has no spread; resampling had no effect")
	}
}

// TestBootstrapDetectsFragileEdge checks the case the feature exists for: a
// result driven by a couple of outsized winners should show a materially
// negative low percentile even though the realised total is positive.
func TestBootstrapDetectsFragileEdge(t *testing.T) {
	t.Parallel()

	// 19 small losses and one huge win: profitable overall, pure luck.
	trips := make([]RoundTrip, 0, 20)
	for range 19 {
		trips = append(trips, trip("TST", -100, -1, ExitStopLoss, time.Hour))
	}

	trips = append(trips, trip("TST", 4000, 40, ExitTakeProfit, time.Hour))

	mc := Bootstrap(trips, 100000, 4000, 0.3, 5)
	if mc == nil {
		t.Fatal("nil result")
	}

	if mc.ProbProfit > 0.9 {
		t.Errorf("probability of profit = %.2f; a one-winner result should not look near-certain",
			mc.ProbProfit)
	}

	if mc.ReturnP5 >= mc.ReturnP95 {
		t.Error("no spread on a result that is obviously luck-dependent")
	}
}

// TestBootstrapDeclinesTinySamples checks it refuses to manufacture confidence
// from a handful of trades.
func TestBootstrapDeclinesTinySamples(t *testing.T) {
	t.Parallel()

	if mc := Bootstrap(mcTrips(9, 100, -50), 100000, 500, 0.3, 1); mc != nil {
		t.Error("bootstrapped 9 trades; too few to mean anything")
	}

	if mc := Bootstrap(nil, 100000, 500, 0.3, 1); mc != nil {
		t.Error("bootstrapped an empty trade list")
	}

	if mc := Bootstrap(mcTrips(20, 100, -50), 0, 500, 0.3, 1); mc != nil {
		t.Error("bootstrapped with no starting cash")
	}
}

// TestRuinProbabilityUsesTheThreshold checks a tighter ruin definition can only
// increase the reported probability.
func TestRuinProbabilityUsesTheThreshold(t *testing.T) {
	t.Parallel()

	trips := mcTrips(60, 900, -1000) // volatile, deep drawdowns

	tight := Bootstrap(trips, 100000, 2000, 0.02, 9)
	loose := Bootstrap(trips, 100000, 2000, 0.50, 9)

	if tight.ProbRuin < loose.ProbRuin {
		t.Errorf("prob ruin at 2%% (%.3f) is below 50%% (%.3f); the threshold is inverted",
			tight.ProbRuin, loose.ProbRuin)
	}

	if tight.RuinPct != 2 || loose.RuinPct != 50 {
		t.Errorf("reported thresholds = %v/%v, want 2/50", tight.RuinPct, loose.RuinPct)
	}
}

// TestPercentileInterpolates checks the percentile helper on a known series.
func TestPercentileInterpolates(t *testing.T) {
	t.Parallel()

	s := []float64{0, 10, 20, 30, 40}

	for p, want := range map[float64]float64{0: 0, 50: 20, 100: 40, 25: 10} {
		if got := percentile(s, p); math.Abs(got-want) > 1e-9 {
			t.Errorf("percentile(%v) = %v, want %v", p, got, want)
		}
	}

	if got := percentile(nil, 50); got != 0 {
		t.Errorf("percentile(nil) = %v, want 0", got)
	}

	if got := percentile([]float64{7}, 95); got != 7 {
		t.Errorf("percentile of one value = %v, want 7", got)
	}
}
