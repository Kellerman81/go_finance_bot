package autotune

import (
	"io"
	"log/slog"
	"math"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/backtest"
	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// TestProbeExpectancyIsAFraction is the regression for SC2-2 / AT2-2.
// backtest.Result.ExpectancyPct is a percent; the screener's probe stage,
// min_expectancy_pct and the verdict table read Probe.ExpectancyPct as a
// fraction. Passed through raw, every non-zero threshold was 100x too loose,
// "set it below -1 to never reject" was false, and 0.8%/trade showed as 80%.
func TestProbeExpectancyIsAFraction(t *testing.T) {
	t.Parallel()

	p := probeFrom("backtest", backtest.Result{ExpectancyPct: 0.8, NumTrades: 10}, 1)

	if math.Abs(p.ExpectancyPct-0.008) > 1e-12 {
		t.Errorf("Probe.ExpectancyPct = %v for a 0.8%%/trade backtest, want the fraction 0.008", p.ExpectancyPct)
	}
}

// TestScreenProbeRefusesAnAIStrategy is the regression for AT2-3, AT-14
// reopened through the probe. RunSymbolOptimize refuses a strategy with an
// "ai" or "ml" detector before backtesting it; the screener probe's live-
// strategy backtest called runBaseline directly and skipped that guard, so a
// replay read the live advisor's verdict cache on every historical bar — and
// its refresh overwrote the verdict the live loop trades on.
func TestScreenProbeRefusesAnAIStrategy(t *testing.T) {
	eng, rm, pf := newTestEngine(t, nil)

	strat := config.Default().Strategy
	strat.Detectors = []string{"ema_cross", "rsi", "macd", "ai"}
	strat.MinStrength = 0
	eng.SetStrategyConfig(strat)

	p := NewScreenProbe(eng, rm, pf, slog.New(slog.NewTextHandler(io.Discard, nil)))

	data := backtest.Generate([]string{"AAPL"}, 3000)
	if _, ok := p.runLiveStrategy("AAPL", data["AAPL"]); ok {
		t.Error("the probe backtested a strategy with an \"ai\" detector")
	}
}
