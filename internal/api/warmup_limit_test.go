package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/ml"
)

// ST3-3: a strategy whose warmup needs more than the 3,000 bars the engine
// keeps — a higher-timeframe multiple on the default trend detector, say —
// was accepted, and live then voted HOLD on every symbol forever while a
// backtest kept trading. The save is refused.
func TestAStrategyThatCanNeverWarmUpIsRefused(t *testing.T) {
	s := newTestServer(t)

	cfg := config.Default().Strategy
	cfg.HTFMultiple = 150 // default trend: warmup ~3,300 bars

	body, _ := json.Marshal(cfg)

	if code, resp := do(t, s, http.MethodPut, "/api/strategy", string(body)); code != http.StatusBadRequest {
		t.Errorf("status %d (%s), want 400", code, resp)
	}
}

// ML3-3: ml.train_bars past ~2,950 pushed the strategy's warmup beyond the
// engine's history: the whole live strategy, every detector of every symbol,
// stayed in warmup for good. The ML save is refused — and rolled back, so it
// is wholly refused.
func TestAnMLSaveThatCannotWarmUpIsRefusedAndRolledBack(t *testing.T) {
	prev := ml.Default().Config()
	t.Cleanup(func() { ml.Configure(prev, ml.ParamsFor(config.Default().Strategy)) })

	s := newTestServer(t)

	strat := config.Default().Strategy
	strat.Detectors = []string{"ml"}
	s.eng.SetStrategyConfig(strat)

	ml.Configure(config.MLConfig{Enabled: true, TrainBars: 1000}, ml.ParamsFor(strat))
	s.eng.RebuildStrategies()

	in := config.Default().ML
	in.Enabled, in.TrainBars = true, 4000
	body, _ := json.Marshal(in)

	if code, resp := do(t, s, http.MethodPut, "/api/settings/ml", string(body)); code == http.StatusOK {
		t.Errorf("an ML save with train_bars 4000 was accepted: %s", resp)
	}

	if got := ml.Default().Config().TrainBars; got != 1000 {
		t.Errorf("train_bars after the refused save = %d, want the previous 1000", got)
	}
}
