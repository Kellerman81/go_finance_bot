package backtest

import (
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

func TestOptimizeRanksAndFilters(t *testing.T) {
	data := Generate([]string{"AAPL", "MSFT"}, 400)
	grid := ParamGrid{
		DetectorSets: [][]string{{"ema_cross", "macd"}, {"rsi", "macd"}},
		Combine:      []string{"majority", "weighted"},
		MinStrength:  []float64{0.1, 0.3},
		CapitalFrac:  []float64{0.1, 0.25},
		StopLoss:     []float64{0.05},
		TrailingStop: []float64{0.08},
		TakeProfit:   []float64{0.2},
	}
	const minTrades = 3
	trials := Optimize(config.Default(), data, grid, 50000, minTrades, ScoreBlend, false)
	if len(trials) == 0 {
		t.Fatal("expected at least one valid trial")
	}

	// Sorted best-first by score.
	for i := 1; i < len(trials); i++ {
		if trials[i-1].Score < trials[i].Score {
			t.Fatalf("trials not sorted: %f < %f at %d", trials[i-1].Score, trials[i].Score, i)
		}
	}
	// Min-trades filter respected.
	for _, tr := range trials {
		if tr.Result.NumTrades < minTrades {
			t.Errorf("trial with %d trades slipped past min-trades %d", tr.Result.NumTrades, minTrades)
		}
	}
}

func TestOptimizeRandomDeterministicAndTunesParams(t *testing.T) {
	data := Generate([]string{"AAPL", "MSFT"}, 400)

	a := OptimizeRandom(config.Default(), data, 50000, 500, 3, ScoreReturn, false, 42)
	b := OptimizeRandom(config.Default(), data, 50000, 500, 3, ScoreReturn, false, 42)
	if len(a) == 0 {
		t.Fatal("expected trials")
	}
	// Same seed => identical best result.
	if a[0].Result.TotalReturnPct != b[0].Result.TotalReturnPct || a[0].Score != b[0].Score {
		t.Error("random search not reproducible for a fixed seed")
	}
	// Indicator periods must actually vary across trials (the whole point).
	periods := map[int]bool{}
	for _, tr := range a {
		periods[tr.Params.MACDFast] = true
	}
	if len(periods) < 2 {
		t.Error("random search did not explore indicator periods")
	}
	// Sorted best-first.
	for i := 1; i < len(a); i++ {
		if a[i-1].Score < a[i].Score {
			t.Fatal("random trials not sorted by score")
		}
	}
}

func TestSensitivityVariesAndMarksBaseline(t *testing.T) {
	data := Generate([]string{"AAPL", "MSFT"}, 400)
	cfg := config.Default()
	cfg.Strategy.Detectors = []string{"ema_cross", "rsi", "macd"} // active so params bite

	baseline, axes := Sensitivity(cfg, data, 50000, 0.25, false)
	_ = baseline
	if len(axes) == 0 {
		t.Fatal("expected axis sweeps")
	}

	varied := false
	for _, ax := range axes {
		if len(ax.Rows) < 2 {
			t.Errorf("axis %s tested only %d value(s)", ax.Axis, len(ax.Rows))
		}
		baseCount := 0
		first := ax.Rows[0].Result.TotalReturnPct
		for _, r := range ax.Rows {
			if r.Baseline {
				baseCount++
			}
			if r.Result.TotalReturnPct != first {
				varied = true
			}
		}
		if baseCount > 1 {
			t.Errorf("axis %s marked %d baselines", ax.Axis, baseCount)
		}
	}
	if !varied {
		t.Error("no parameter changed the result — sensitivity is not exercising configs")
	}
}

func TestScorerByName(t *testing.T) {
	r := Result{TotalReturnPct: 5, WinRatePct: 80, Sharpe: 1.2, MaxDrawdownPct: 2}
	if ScorerByName("return")(r) != 5 {
		t.Error("return scorer wrong")
	}
	if ScorerByName("winrate")(r) != 80 {
		t.Error("winrate scorer wrong")
	}
	if ScorerByName("sharpe")(r) != 1.2 {
		t.Error("sharpe scorer wrong")
	}
	// blend = 5 + (80-50)*0.04 - 2*0.1 = 5 + 1.2 - 0.2 = 6.0
	if got := ScorerByName("blend")(r); got != 6.0 {
		t.Errorf("blend scorer = %f, want 6.0", got)
	}
}
