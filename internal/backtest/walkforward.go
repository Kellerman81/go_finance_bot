package backtest

import (
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// WFFold is one walk-forward fold: the in-sample-selected parameters and their
// out-of-sample performance on the following segment.
type WFFold struct {
	TrainBars int     `json:"train_bars"` // bars used to select Best (in-sample)
	TestBars  int     `json:"test_bars"`  // bars scored out-of-sample
	Best      Trial   `json:"best"`       // in-sample winner (its params drive the test)
	Test      Result  `json:"test"`       // out-of-sample result of Best's params
	TestScore float64 `json:"test_score"` // score of Test under the chosen Scorer
}

// WalkForward aggregates the out-of-sample folds. MeanOOSScore/Return average the
// per-fold out-of-sample outcomes — a far more honest read of a parameter set
// than the in-sample optimum an ordinary grid/random search reports.
type WalkForward struct {
	Folds         []WFFold `json:"folds"`
	MeanOOSScore  float64  `json:"mean_oos_score"`
	MeanOOSReturn float64  `json:"mean_oos_return"`
}

// WalkForwardOptimize performs an anchored walk-forward analysis. It splits the
// data into folds+1 equal, non-overlapping time segments; for each of the last
// `folds` segments it grid-searches the best parameters on all preceding data
// (in-sample) and measures those parameters on that segment (out-of-sample).
//
// Because each test segment is scored in isolation, the strategy re-warms up
// inside it, so the segments should be comfortably longer than the strategy's
// lookback for the estimate to be meaningful. It returns each fold and the mean
// out-of-sample score/return.
func WalkForwardOptimize(base config.Config, data map[string][]market.Candle, grid ParamGrid,
	cash float64, minTrades, folds int, score Scorer, respectLimits bool,
) WalkForward {
	if folds < 1 {
		folds = 1
	}

	maxLen := 0
	for _, c := range data {
		if len(c) > maxLen {
			maxLen = len(c)
		}
	}

	seg := maxLen / (folds + 1)

	var wf WalkForward

	if seg < 2 {
		return wf // not enough data to split
	}

	var sumScore, sumRet float64

	for k := 1; k <= folds; k++ {
		trainEnd := k * seg
		testEnd := (k + 1) * seg

		if k == folds {
			testEnd = maxLen // last fold soaks up any remainder
		}

		train := sliceData(data, 0, trainEnd)
		test := sliceData(data, trainEnd, testEnd)

		trials := Optimize(base, train, grid, cash, minTrades, score, respectLimits)
		if len(trials) == 0 {
			continue
		}

		best := trials[0]
		res := runConfig(
			base,
			test,
			best.Params,
			best.CapitalFrac,
			best.StopLoss,
			best.TrailingStop,
			best.TakeProfit,
			cash,
			respectLimits,
		)
		s := score(res)

		sumScore += s
		sumRet += res.TotalReturnPct

		wf.Folds = append(wf.Folds, WFFold{
			TrainBars: trainEnd, TestBars: testEnd - trainEnd,
			Best: best, Test: res, TestScore: s,
		})
	}

	if n := len(wf.Folds); n > 0 {
		wf.MeanOOSScore = sumScore / float64(n)
		wf.MeanOOSReturn = sumRet / float64(n)
	}

	return wf
}

// sliceData returns the [lo, hi) index window of every symbol's candles, dropping
// symbols with no bars in range.
func sliceData(data map[string][]market.Candle, lo, hi int) map[string][]market.Candle {
	out := make(map[string][]market.Candle, len(data))
	for s, c := range data {
		a, b := lo, hi
		if a < 0 {
			a = 0
		}

		if b > len(c) {
			b = len(c)
		}

		if a >= b {
			continue
		}

		out[s] = c[a:b]
	}

	return out
}
