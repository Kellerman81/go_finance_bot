package backtest

import (
	"math"
	"testing"
)

// referenceSortino is the textbook downside-deviation Sortino: the squared
// shortfall below a zero minimum acceptable return, averaged over EVERY
// observation, not just the losing ones.
func referenceSortino(rets []float64, periods float64) float64 {
	var mean float64
	for _, x := range rets {
		mean += x
	}

	mean /= float64(len(rets))

	var sumSq float64

	for _, x := range rets {
		if x < 0 {
			sumSq += x * x
		}
	}

	dd := math.Sqrt(sumSq / float64(len(rets)))
	if dd == 0 {
		return 0
	}

	return mean / dd * math.Sqrt(periods)
}

// TestSortinoDividesByEveryObservation reproduces the BT-1 finding.
// annualisedRatio used to divide the downside sum by the count of LOSING bars,
// which scales the result by sqrt(losers/len) — a factor that differs per
// strategy, so it reorders the leaderboard rather than merely rescaling it.
func TestSortinoDividesByEveryObservation(t *testing.T) {
	t.Parallel()

	const periods = 252

	cases := map[string][]float64{
		"few losing bars":  {0.02, 0.02, 0.02, 0.02, 0.02, 0.02, 0.02, 0.02, 0.02, -0.04},
		"many losing bars": {0.036, -0.01, 0.036, -0.01, 0.036, -0.01, 0.036, -0.01, -0.01, -0.01},
		"all losing bars":  {-0.01, -0.02, -0.01, -0.03, -0.01, -0.02, -0.01, -0.02, -0.01, -0.02},
	}

	for name, rets := range cases {
		got := annualisedRatio(rets, periods, true)

		want := referenceSortino(rets, periods)
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("%s: Sortino = %.6f, want %.6f (downside deviation is averaged over "+
				"every observation, not over the losing bars alone)", name, got, want)
		}
	}
}

// The reordering is the reason it matters: two series with the same mean and
// the same total downside energy must rank by which one concentrated its losses
// into fewer bars, and the old divisor inverted that.
func TestSortinoRanksFewerLosingBarsHigher(t *testing.T) {
	t.Parallel()

	const periods = 252

	few := []float64{0.02, 0.02, 0.02, 0.02, 0.02, 0.02, 0.02, 0.02, 0.02, -0.04}
	many := []float64{0.036, -0.01, 0.036, -0.01, 0.036, -0.01, 0.036, -0.01, -0.01, -0.01}

	if a, b := annualisedRatio(few, periods, true), annualisedRatio(many, periods, true); a <= b {
		t.Errorf("Sortino(few losing bars) = %.3f, Sortino(many) = %.3f — the series with "+
			"the higher mean and the same downside energy must rank higher", a, b)
	}
}

// No losing bars at all still reports 0 rather than an infinity that would top
// every leaderboard — behaviour the fix must not disturb.
func TestSortinoWithNoLosingBarsIsZero(t *testing.T) {
	t.Parallel()

	if got := annualisedRatio([]float64{0.01, 0.02, 0.0, 0.03}, 252, true); got != 0 {
		t.Errorf("Sortino with no losing bars = %v, want 0", got)
	}
}

// Sharpe keeps the sample standard deviation (len-1); the fix must not have
// changed it while changing its sibling.
func TestSharpeUsesTheSampleDeviation(t *testing.T) {
	t.Parallel()

	rets := []float64{0.01, -0.02, 0.03, -0.01, 0.02}

	var mean float64
	for _, x := range rets {
		mean += x
	}

	mean /= float64(len(rets))

	var sumSq float64

	for _, x := range rets {
		sumSq += (x - mean) * (x - mean)
	}

	want := mean / math.Sqrt(sumSq/float64(len(rets)-1)) * math.Sqrt(252)
	if got := annualisedRatio(rets, 252, false); math.Abs(got-want) > 1e-9 {
		t.Errorf("Sharpe = %.6f, want %.6f", got, want)
	}
}
