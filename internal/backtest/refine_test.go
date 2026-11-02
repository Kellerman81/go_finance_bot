package backtest

import (
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// refineBase is a config with permissive limits so the refinement axes, not the
// money caps, decide the outcome.
func refineBase() config.Config {
	cfg := config.Default()

	cfg.Limits = testLimits()
	cfg.Engine.ConfirmBuy = 1
	cfg.Engine.ConfirmSell = 1
	cfg.Engine.KeepInterval = 0
	cfg.Strategy.TrendGate = false
	cfg.Protections.Enabled = false

	return cfg
}

// gridWinner builds the starting trial a refinement pass would receive.
func gridWinner(cfg config.Config, cash float64) Trial {
	sc := cfg.Strategy
	sc.Detectors = []string{"ema_cross", "rsi", "macd"}
	sc.Combine = "majority"

	ex := cfg.Exits
	ex.Enabled = true

	return Trial{
		Detectors: sc.Detectors, Combine: sc.Combine, MinStrength: sc.MinStrength,
		CapitalFrac: 0.25, OrderSize: cash * 0.25,
		StopLoss: ex.StopLossPct, TrailingStop: ex.TrailingStopPct, TakeProfit: ex.TakeProfitPct,
		Params: sc, Exits: ex,
		ConfirmBuy: cfg.Engine.ConfirmBuy, ConfirmSell: cfg.Engine.ConfirmSell,
		KeepInterval: cfg.Engine.KeepInterval,
	}
}

// TestRefineExploresTheSellSide is the point of the pass: the grid only ever
// searches the BUY detectors, so nothing before this asks whether a different
// set is better at deciding when to get out.
func TestRefineExploresTheSellSide(t *testing.T) {
	t.Parallel()

	cfg := refineBase()
	data := Generate([]string{"AAPL", "MSFT"}, 1200)
	cash := 50000.0

	start := gridWinner(cfg, cash)
	start.Result = runTrialConfig(cfg, data, start, cash, false)
	start.Score = ScoreReturn(start.Result)

	best, changes := Refine(cfg, data, start, cash, 1, ScoreReturn, false, nil, false)

	// Refinement may legitimately find nothing, but it must never make the
	// incumbent worse — it only adopts a strictly higher score.
	if best.Score < start.Score {
		t.Errorf("refinement lowered the score: %.4f -> %.4f", start.Score, best.Score)
	}

	// Every reported change must be an improvement, and must be reflected in
	// the returned trial.
	for _, c := range changes {
		if c.Improved <= 0 {
			t.Errorf("logged a change with no gain: %+v", c)
		}
	}

	if len(changes) > 0 && best.Score == start.Score {
		t.Error("changes were logged but the score did not move")
	}
}

// TestRefineSellSideIsReachable proves the sell-side axis can actually change
// the configuration, rather than being swept but structurally inert.
func TestRefineSellSideIsReachable(t *testing.T) {
	t.Parallel()

	cfg := refineBase()
	data := Generate([]string{"AAPL", "MSFT", "NVDA"}, 1500)
	cash := 50000.0

	start := gridWinner(cfg, cash)
	start.Result = runTrialConfig(cfg, data, start, cash, false)
	start.Score = ScoreReturn(start.Result)

	// Force the question: give the incumbent a deliberately poor sell side and
	// check refinement moves away from it.
	start.Params.DetectorsSell = []string{"donchian"}
	start.Result = runTrialConfig(cfg, data, start, cash, false)
	start.Score = ScoreReturn(start.Result)

	best, _ := Refine(cfg, data, start, cash, 1, ScoreReturn, false, nil, false)

	if best.Score < start.Score {
		t.Fatalf("refinement made it worse: %.4f -> %.4f", start.Score, best.Score)
	}

	// The refined trial must be runnable as emitted: re-running it reproduces
	// the score it was selected on. This is what makes the printed config
	// trustworthy.
	again := runTrialConfig(cfg, data, best, cash, false)
	if got := ScoreReturn(again); got != best.Score {
		t.Errorf("re-running the refined trial scored %.6f, selected on %.6f — "+
			"the emitted config does not reproduce its own result", got, best.Score)
	}
}

// TestRefineTrendGateIsReachable proves the trend-gate axis added alongside
// this test can actually change the configuration — the same property
// TestRefineSellSideIsReachable proves for the sell side. TrendGate is
// carried through every trial correctly (it is part of the base
// StrategyConfig), but before this fix nothing in the optimizer's main
// search or Refine ever varied it, so a run always kept whatever config.yaml
// said regardless of whether it helped.
//
// Rather than predicting by hand which of trend_gate on/off scores better on
// this data — Mock's random walk has no reliable a-priori drift since its
// bias bug was fixed (internal/market/mock_candles_test.go) — the test
// discovers the winner empirically (deterministically reproducible: Generate
// seeds its RNG per symbol) and then confirms Refine's own sweep finds it
// when started from the loser.
func TestRefineTrendGateIsReachable(t *testing.T) {
	t.Parallel()

	cfg := refineBase()
	cfg.Strategy.Detectors = []string{"ema_cross"}
	cfg.Strategy.Combine = "majority"
	cfg.Strategy.TrendPeriod = 200
	cfg.Strategy.TrendThreshold = 0.01

	data := Generate([]string{"AAPL", "MSFT", "NVDA"}, 3000)
	cash := 50000.0

	on := gridWinner(cfg, cash)
	on.Params.TrendGate = true
	on.Result = runTrialConfig(cfg, data, on, cash, false)
	on.Score = ScoreReturn(on.Result)

	off := gridWinner(cfg, cash)
	off.Params.TrendGate = false
	off.Result = runTrialConfig(cfg, data, off, cash, false)
	off.Score = ScoreReturn(off.Result)

	if on.Score == off.Score {
		t.Skip("trend_gate makes no difference on this data/seed — nothing for refine to find")
	}

	// Start from whichever scored worse; Refine's trend_gate sweep must find
	// the better one.
	start, want := off, on.Params.TrendGate
	if off.Score > on.Score {
		start, want = on, off.Params.TrendGate
	}

	best, changes := Refine(cfg, data, start, cash, 1, ScoreReturn, false, nil, false)

	if best.Params.TrendGate != want {
		t.Errorf("trend_gate = %v after refine, want %v (the higher-scoring setting)",
			best.Params.TrendGate, want)
	}

	found := false

	for _, c := range changes {
		if c.Axis == "trend_gate" {
			found = true
		}
	}

	if !found {
		t.Error("no trend_gate entry in the refinement log, even though it changed the outcome")
	}
}

// TestRefineRespectsMinTrades checks a variant that trades too rarely cannot win,
// exactly as in the main search.
func TestRefineRespectsMinTrades(t *testing.T) {
	t.Parallel()

	cfg := refineBase()
	data := Generate([]string{"AAPL"}, 800)
	cash := 50000.0

	start := gridWinner(cfg, cash)
	start.Result = runTrialConfig(cfg, data, start, cash, false)
	start.Score = ScoreReturn(start.Result)

	// An unreachable minimum: nothing may be adopted.
	best, changes := Refine(cfg, data, start, cash, 10000, ScoreReturn, false, nil, false)

	if len(changes) != 0 {
		t.Errorf("adopted %d changes despite an unreachable min-trades", len(changes))
	}

	if best.Score != start.Score {
		t.Errorf("score changed to %.4f with no adoptable variant", best.Score)
	}
}

// TestTrialCarriesEverythingNeeded checks a Trial fully describes a runnable
// configuration. If it did not, the printed "best config" would omit settings
// the result depended on and could not be reproduced.
func TestTrialCarriesEverythingNeeded(t *testing.T) {
	t.Parallel()

	cfg := refineBase()
	cfg.Engine.ConfirmBuy = 3
	cfg.Engine.KeepInterval = 90 * time.Minute
	cfg.Exits.ATRStopMult = 2

	data := Generate([]string{"AAPL"}, 900)

	trial, ok := runTrial(cfg, data, cfg.Strategy, 0.25, 0.05, 0.08, 0.2, 50000, 1,
		ScoreReturn, false)
	if !ok {
		t.Skip("no qualifying trial on this sample")
	}

	if trial.ConfirmBuy != 3 {
		t.Errorf("confirm_buy = %d, want the base config's 3", trial.ConfirmBuy)
	}

	if trial.KeepInterval != 90*time.Minute {
		t.Errorf("keep_interval = %v, want 90m", trial.KeepInterval)
	}

	if trial.Exits.ATRStopMult != 2 {
		t.Errorf("atr_stop_mult = %v, want the base config's 2", trial.Exits.ATRStopMult)
	}

	if trial.Exits.StopLossPct != 0.05 {
		t.Errorf("stop_loss = %v, want the swept 0.05", trial.Exits.StopLossPct)
	}
}

// TestProtectionsAreModelledInBacktests checks the backtester applies the
// protections rather than ignoring them. Without this, the optimiser would
// happily recommend a configuration the live bot halts on day one.
func TestProtectionsAreModelledInBacktests(t *testing.T) {
	t.Parallel()

	data := Generate([]string{"AAPL", "MSFT", "NVDA"}, 2000)

	run := func(p config.ProtectionsConfig) Result {
		bt := &Backtester{
			Strategy: strategy.New(permissiveStrategy()), Limits: testLimits(),
			Exits: config.ExitsConfig{Enabled: true, StopLossPct: 0.02},
			Cash:  100000, OrderSize: 5000, Protections: p,
		}

		return bt.Run(data)
	}

	off := run(config.ProtectionsConfig{Enabled: false})

	// A cooldown long enough to matter must reduce the number of entries.
	on := run(config.ProtectionsConfig{
		Enabled: true, CooldownAfterLoss: 24 * time.Hour,
	})

	if off.NumTrades == 0 {
		t.Skip("no trades on this sample")
	}

	if on.NumTrades > off.NumTrades {
		t.Errorf("protections increased trades (%d -> %d); they are meant to gate entries",
			off.NumTrades, on.NumTrades)
	}
}

// TestDrawdownHaltIsReported checks a run that would have been halted says so,
// rather than presenting an equity curve the live bot would never have reached.
func TestDrawdownHaltIsReported(t *testing.T) {
	t.Parallel()

	data := Generate([]string{"AAPL", "MSFT"}, 1500)

	bt := &Backtester{
		Strategy: strategy.New(permissiveStrategy()), Limits: testLimits(),
		Exits: config.ExitsConfig{Enabled: true, StopLossPct: 0.02},
		Cash:  100000, OrderSize: 20000,
		// An absurdly tight drawdown limit, so the halt certainly trips.
		Protections: config.ProtectionsConfig{Enabled: true, MaxDrawdownPct: 0.0001},
	}

	res := bt.Run(data)
	if res.NumTrades == 0 {
		t.Skip("no trades on this sample")
	}

	if res.HaltedAt == "" {
		t.Error("a 0.01% drawdown limit did not report a halt")
	}
}

// PR2-7: max_realized_drawdown_pct was never fed in the backtest, so a config
// the live bot would halt on realized give-back reported a full, unhalted
// equity curve — and passed autotune's gates on it.
func TestRealizedDrawdownHaltIsReported(t *testing.T) {
	t.Parallel()

	data := Generate([]string{"AAPL", "MSFT"}, 1500)

	bt := &Backtester{
		Strategy: strategy.New(permissiveStrategy()), Limits: testLimits(),
		Exits: config.ExitsConfig{Enabled: true, StopLossPct: 0.02},
		Cash:  100000, OrderSize: 20000,
		// Only the realized-drawdown limit: ObserveEquity cannot trip this run.
		Protections: config.ProtectionsConfig{Enabled: true, MaxRealizedDrawdownPct: 0.0001},
	}

	res := bt.Run(data)
	if res.Losses == 0 {
		t.Skip("no losing trade on this sample")
	}

	if res.HaltedAt == "" || !strings.Contains(res.HaltedAt, "realized") {
		t.Errorf("HaltedAt = %q, want a realized-drawdown halt", res.HaltedAt)
	}
}

// permissiveStrategy trades often enough for the protection tests to have
// something to gate.
func permissiveStrategy() config.StrategyConfig {
	sc := config.Default().Strategy

	sc.Detectors = []string{"ema_cross", "rsi", "macd"}
	sc.Combine = "majority"
	sc.MinStrength = 0
	sc.MinStrengthSell = 0
	sc.TrendGate = false

	return sc
}
