package backtest

import (
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/ml"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// TestBacktestLeavesTheLivePredictorAlone is the regression for ML2-3, the
// half of CRIT-7 its fix left open. Run reset the process-wide ML predictor
// before every replay — and inside the bot that IS the live predictor, so
// every autotune pass, screener probe and optimize click deleted the models
// the live loop trades on, then trained replacements into the live map from
// the replay's own bars. A replay must train into a predictor of its own.
//
// Not parallel: it installs the process-wide predictor.
func TestBacktestLeavesTheLivePredictorAlone(t *testing.T) {
	prev := ml.Default()
	t.Cleanup(func() { restoreDefaultPredictor(prev) })

	cfg := config.Default()
	cfg.ML.Enabled = true
	cfg.ML.TrainBars, cfg.ML.MinSamples, cfg.ML.RetrainEvery = 300, 50, 100
	cfg.Strategy.Detectors = []string{"ml"}

	ml.Configure(cfg.ML, ml.ParamsFor(cfg.Strategy))
	live := ml.Default()

	data := Generate([]string{"LIVE", "TST"}, 800)
	live.Verdict("LIVE", data["LIVE"])

	before := live.Stats()
	if before.Symbols != 1 {
		t.Fatalf("live predictor tracks %d symbols after one verdict; the scenario did not set up", before.Symbols)
	}

	bt := &Backtester{
		Strategy: strategy.New(cfg.Strategy), Limits: testLimits(), Cash: 100000,
		Exits: cfg.Exits,
	}
	bt.Run(map[string][]market.Candle{"TST": data["TST"]})

	after := live.Stats()
	if after.Symbols != before.Symbols || after.Trained != before.Trained || after.Failures != before.Failures {
		t.Errorf("a backtest changed the live predictor: before %+v, after %+v", before, after)
	}
}

// restoreDefaultPredictor reinstalls p as the process-wide predictor.
func restoreDefaultPredictor(p *ml.Predictor) {
	if p == nil {
		ml.Configure(config.MLConfig{}, ml.ParamsFor(config.Default().Strategy)) // disabled: Default() == nil

		return
	}

	ml.Configure(p.Config(), ml.ParamsFor(config.Default().Strategy))
}
