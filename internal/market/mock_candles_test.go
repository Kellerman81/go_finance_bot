package market

import (
	"context"
	"testing"
	"time"
)

// TestCandlesRandomWalkIsUnbiased reproduces a drift bug in the synthetic
// history generator: the per-bar multiplicative step was drawn from
// rng.Float64()-0.48 instead of rng.Float64()-0.5. rand.Float64() is uniform
// on [0,1), so that 0.02 offset shifts the step's mean from 0 to +0.0002
// (0.02%) per bar — invisible over a handful of bars, but compounded over
// the tens of thousands of bars a multi-month backtest requests, it explodes
// exponentially. A real 50,000-bar run this bug produced took AAPL from
// ~$223 to over $6,200,000 — a ~27,800x runaway that made every backtest
// metric derived from it meaningless (see the 2026-08-30 session notes).
//
// An unbiased random walk over N steps has log-return standard deviation
// sqrt(N * Var(step)); for the parameters here (step in [-0.005,0.005),
// Var = 0.01^2/12) and N=20,000 that is ~0.41, so a final/initial ratio
// outside [0.1, 10] is a >5-sigma event under the correct formula — this
// bound has essentially zero false-positive risk while still being tight
// enough to catch the ~50x drift the buggy formula produces at this N.
func TestCandlesRandomWalkIsUnbiased(t *testing.T) {
	m := &Mock{}

	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(20000 * time.Minute)

	for _, symbol := range []string{"AAPL", "MSFT", "NVDA", "TESTSYM"} {
		candles, err := m.Candles(context.Background(), symbol, Res1Min, from, to)
		if err != nil {
			t.Fatalf("%s: Candles: %v", symbol, err)
		}

		if len(candles) < 2 {
			t.Fatalf("%s: got %d candles, want ~20000", symbol, len(candles))
		}

		first, last := candles[0].Open, candles[len(candles)-1].Close
		ratio := last / first

		if ratio < 0.1 || ratio > 10 {
			t.Errorf("%s: price went from %.2f to %.2f (ratio %.2fx) over %d bars — "+
				"an unbiased random walk should not drift this far; want ratio in [0.1, 10]",
				symbol, first, last, ratio, len(candles))
		}
	}
}
