package backtest

import (
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

func TestWalkForwardProducesOutOfSampleFolds(t *testing.T) {
	data := Generate([]string{"AAPL", "MSFT"}, 1200)
	grid := ParamGrid{
		DetectorSets: [][]string{{"ema_cross", "macd"}, {"rsi", "macd"}},
		Combine:      []string{"majority", "weighted"},
		MinStrength:  []float64{0.1, 0.3},
		CapitalFrac:  []float64{0.1, 0.25},
		StopLoss:     []float64{0.05},
		TrailingStop: []float64{0.08},
		TakeProfit:   []float64{0.2},
	}
	const folds = 3
	wf := WalkForwardOptimize(config.Default(), data, grid, 50000, 1, folds, ScoreReturn, false)
	if len(wf.Folds) == 0 {
		t.Fatal("expected walk-forward folds")
	}
	if len(wf.Folds) > folds {
		t.Fatalf("got %d folds, want <= %d", len(wf.Folds), folds)
	}
	// Each fold trains on strictly more data than the previous (anchored WF) and
	// carries a chosen parameter set applied out-of-sample.
	prevTrain := 0
	for i, f := range wf.Folds {
		if f.TestBars <= 0 {
			t.Errorf("fold %d has no test bars", i)
		}
		if f.TrainBars <= prevTrain {
			t.Errorf("fold %d train bars %d not growing (prev %d)", i, f.TrainBars, prevTrain)
		}
		prevTrain = f.TrainBars
		if len(f.Best.Params.Detectors) == 0 {
			t.Errorf("fold %d selected no detectors", i)
		}
	}
}

func TestSliceDataWindows(t *testing.T) {
	data := Generate([]string{"AAPL"}, 100)
	mid := sliceData(data, 20, 60)
	if len(mid["AAPL"]) != 40 {
		t.Errorf("sliceData [20,60) => %d bars, want 40", len(mid["AAPL"]))
	}
	// Out-of-range highs clamp; inverted ranges drop the symbol.
	if got := len(sliceData(data, 90, 1000)["AAPL"]); got != 10 {
		t.Errorf("clamped high => %d bars, want 10", got)
	}
	if _, ok := sliceData(data, 60, 20)["AAPL"]; ok {
		t.Error("inverted range should drop the symbol")
	}
}
