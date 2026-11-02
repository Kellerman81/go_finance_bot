package engine

import (
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// TestHTFWarmupFitsTheLiveHistory is the regression for ST2-1. Turning on
// htf_multiple folded the higher timeframe's backtest window — sized off the
// default trend detector's 1950-bar lookback — into the strategy's readiness
// warmup: 3906 bars at a multiple of 2, 29282 at the 15 config.example.yaml
// suggests. The engine keeps at most maxBars of history, so every live
// evaluation returned "warming up" forever and the strategy never traded,
// while the backtest (which sizes its window from MaxLookback) traded fine.
func TestHTFWarmupFitsTheLiveHistory(t *testing.T) {
	t.Parallel()

	for _, multiple := range []int{2, 5, 15} {
		cfg := config.Default().Strategy
		cfg.HTFMultiple = multiple

		if w := strategy.New(cfg).WarmupBars(); w > maxBars {
			t.Errorf("htf_multiple %d: warmup %d bars, more than the %d the live engine keeps — "+
				"the strategy can never leave warmup", multiple, w, maxBars)
		}
	}
}
