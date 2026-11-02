package backtest

import (
	"context"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// TestOptimizeTrialsDropHeavyPerBarFields reproduces the RAM-exhaustion bug:
// every trial's Result used to retain its full per-bar equity curve and
// every trade/round-trip for as long as the whole search ran. With a large
// grid over a long series (a real report: 86,400 bars crashed the editor a
// search was running inside), that is thousands of trials' worth of per-bar
// arrays held in memory simultaneously. Nothing in the search, ranking,
// refinement or CLI printing ever reads these fields from a trial (see
// dropHeavyFields' own comment) — only the scalar summary fields drive
// ranking and the leaderboard.
func TestOptimizeTrialsDropHeavyPerBarFields(t *testing.T) {
	data := Generate([]string{"AAPL", "MSFT"}, 800)
	grid := ParamGrid{
		DetectorSets: [][]string{{"ema_cross", "macd"}},
		Combine:      []string{"majority"},
		// 0.03, not this codebase's usual 0.1+: this test only needs SOME
		// valid trial to exercise dropHeavyFields, and macd's strength is
		// now normalized by price (see CODE_REVIEW_FINDINGS.md's AT-7
		// finding), which this short synthetic series clears less often at
		// the higher, more realistic thresholds used elsewhere.
		MinStrength:  []float64{0.03},
		CapitalFrac:  []float64{0.25},
		StopLoss:     []float64{0.05},
		TrailingStop: []float64{0.08},
		TakeProfit:   []float64{0.2},
	}

	// minTrades 1, not 3: the gate now counts CLOSED trades rather than fills
	// (BT2-3), and this short series closes fewer than it fills. Nothing here
	// is about the gate — it only needs a surviving trial to inspect.
	trials := Optimize(context.Background(), config.Default(), data, grid, 50000, 1, ScoreBlend, false)
	if len(trials) == 0 {
		t.Fatal("expected at least one valid trial")
	}

	for i, tr := range trials {
		if tr.Result.Trades != nil {
			t.Errorf("trial %d: Result.Trades has %d entries, want nil (dropped)", i, len(tr.Result.Trades))
		}

		if tr.Result.Equity != nil {
			t.Errorf("trial %d: Result.Equity has %d entries, want nil (dropped)", i, len(tr.Result.Equity))
		}

		if tr.Result.RoundTrips != nil {
			t.Errorf(
				"trial %d: Result.RoundTrips has %d entries, want nil (dropped)",
				i,
				len(tr.Result.RoundTrips),
			)
		}
	}
}

// TestRunTrialConfigDropsHeavyPerBarFields is the Refine-side counterpart:
// runTrialConfig is called once per candidate on every refinement axis
// (dozens of backtests per run), so it must drop the same fields runConfig
// does.
func TestRunTrialConfigDropsHeavyPerBarFields(t *testing.T) {
	data := Generate([]string{"AAPL", "MSFT"}, 800)
	cfg := config.Default()

	trial := Trial{
		Detectors: []string{"ema_cross", "macd"}, Combine: "majority", MinStrength: 0.1,
		CapitalFrac: 0.25, OrderSize: 50000 * 0.25,
		StopLoss: 0.05, TrailingStop: 0.08, TakeProfit: 0.2,
		Params: cfg.Strategy,
		Exits:  config.ExitsConfig{Enabled: true, StopLossPct: 0.05, TrailingStopPct: 0.08, TakeProfitPct: 0.2},
	}

	res := runTrialConfig(cfg, data, trial, 50000, false)

	if res.Trades != nil {
		t.Errorf("Result.Trades has %d entries, want nil (dropped)", len(res.Trades))
	}

	if res.Equity != nil {
		t.Errorf("Result.Equity has %d entries, want nil (dropped)", len(res.Equity))
	}

	if res.RoundTrips != nil {
		t.Errorf("Result.RoundTrips has %d entries, want nil (dropped)", len(res.RoundTrips))
	}
}

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
	trials := Optimize(context.Background(), config.Default(), data, grid, 50000, minTrades, ScoreBlend, false)
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
			t.Errorf(
				"trial with %d trades slipped past min-trades %d",
				tr.Result.NumTrades,
				minTrades,
			)
		}
	}
}

// TestOptimizeStopsEarlyWhenContextIsCancelled reproduces the "cmd/optimize:
// no cancellation" finding at the search-function level: Optimize must
// return whatever it has already found once ctx is cancelled, rather than
// running the whole grid regardless — otherwise a multi-hour grid search
// gives an operator no way to stop it early and keep the partial result.
func TestOptimizeStopsEarlyWhenContextIsCancelled(t *testing.T) {
	data := Generate([]string{"AAPL", "MSFT"}, 400)
	grid := ParamGrid{
		DetectorSets: [][]string{{"ema_cross"}, {"rsi"}, {"macd"}},
		Combine:      []string{"majority", "weighted", "unanimous"},
		MinStrength:  []float64{0.1, 0.2, 0.3},
		CapitalFrac:  []float64{0.1, 0.2},
		StopLoss:     []float64{0.05},
		TrailingStop: []float64{0.08},
		TakeProfit:   []float64{0.2},
	}

	total := grid.Combos()
	if total < 10 {
		t.Fatalf("grid too small to demonstrate an early stop: %d combos", total)
	}

	full := Optimize(context.Background(), config.Default(), data, grid, 50000, 1, ScoreBlend, false)

	ctx := newCountAfterCtx(total / 2)
	partial := Optimize(ctx, config.Default(), data, grid, 50000, 1, ScoreBlend, false)

	if len(partial) >= len(full) {
		t.Errorf("cancelled run returned %d trials, want fewer than the uncancelled "+
			"run's %d (cancelled partway through a %d-combo grid)", len(partial), len(full), total)
	}
}

func TestOptimizeRandomDeterministicAndTunesParams(t *testing.T) {
	data := Generate([]string{"AAPL", "MSFT"}, 400)

	a := OptimizeRandom(context.Background(), config.Default(), data, 50000, 500, 3, ScoreReturn, false, 42, Pins{})
	b := OptimizeRandom(context.Background(), config.Default(), data, 50000, 500, 3, ScoreReturn, false, 42, Pins{})
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

// TestRunConfigModelsProtections checks that runConfig — the shared helper
// behind Sensitivity/SensitivityAxis and the grid optimizer's runTrial — wires
// base.Protections into the Backtester it builds, the same way runTrialConfig
// already does. Without this, a sweep or a grid search would measure every
// candidate with protections silently off, and could recommend a
// configuration the live bot halts on day one.
//
// An absurdly tight drawdown limit is the unambiguous signal: it reports a
// halt if and only if the Backtester actually received base.Protections, so
// this can't pass by coincidence the way comparing trade counts could (a
// cooldown that happens not to bind on this sample would make an on-vs-off
// trade-count comparison pass whether or not the wiring exists).
func TestRunConfigModelsProtections(t *testing.T) {
	t.Parallel()

	data := Generate([]string{"AAPL", "MSFT", "NVDA"}, 2000)

	cfg := config.Default()
	cfg.Strategy = permissiveStrategy()
	cfg.Limits = testLimits()
	cfg.Exits = config.ExitsConfig{Enabled: true, StopLossPct: 0.02}
	cfg.Protections = config.ProtectionsConfig{Enabled: true, MaxDrawdownPct: 0.0001}

	baseline, _ := Sensitivity(cfg, data, 100000, 0.20, false)
	if baseline.NumTrades == 0 {
		t.Skip("no trades on this sample")
	}

	if baseline.HaltedAt == "" {
		t.Error("a 0.01% drawdown limit did not report a halt — " +
			"runConfig is not applying base.Protections")
	}
}

func TestSensitivityAxisRunsOnlyTheRequestedAxis(t *testing.T) {
	data := Generate([]string{"AAPL", "MSFT"}, 400)
	cfg := config.Default()
	cfg.Strategy.Detectors = []string{"ema_cross", "rsi", "macd"}

	baseline, sweep := SensitivityAxis(cfg, data, 50000, 0.25, false, "fast_ma")
	if sweep == nil {
		t.Fatal("expected a sweep for fast_ma")
	}
	if sweep.Axis != "fast_ma" {
		t.Errorf("got axis %q, want fast_ma", sweep.Axis)
	}
	if len(sweep.Rows) < 2 {
		t.Errorf("expected multiple candidate values, got %d", len(sweep.Rows))
	}
	if baseline.NumTrades == 0 {
		t.Error("expected the baseline to have traded")
	}

	if _, sweep := SensitivityAxis(cfg, data, 50000, 0.25, false, "not_a_real_axis"); sweep != nil {
		t.Errorf("expected nil sweep for an unknown axis, got %+v", sweep)
	}
}

func TestScorerByName(t *testing.T) {
	r := Result{TotalReturnPct: 5, WinRatePct: 80, Sharpe: 1.2, MaxDrawdownPct: 2}

	if score, ok := ScorerByName("return"); score(r) != 5 || !ok {
		t.Errorf("return scorer: value=%v ok=%v, want 5/true", score(r), ok)
	}

	if score, ok := ScorerByName("winrate"); score(r) != 80 || !ok {
		t.Errorf("winrate scorer: value=%v ok=%v, want 80/true", score(r), ok)
	}

	if score, ok := ScorerByName("sharpe"); score(r) != 1.2 || !ok {
		t.Errorf("sharpe scorer: value=%v ok=%v, want 1.2/true", score(r), ok)
	}

	// blend = 5 + (80-50)*0.04 - 2*0.1 = 5 + 1.2 - 0.2 = 6.0
	if score, ok := ScorerByName("blend"); score(r) != 6.0 || !ok {
		t.Errorf("blend scorer: value=%f ok=%v, want 6.0/true", score(r), ok)
	}
}

// TestScorerByNameReportsUnrecognizedNames reproduces the "-rank typos
// silently fall back to the blend scorer" finding at the function level:
// ScorerByName must report ok=false for an unrecognized name, not just
// silently substitute blend with no way for a caller to tell.
func TestScorerByNameReportsUnrecognizedNames(t *testing.T) {
	score, ok := ScorerByName("sharpee") // typo of "sharpe"
	if ok {
		t.Error("ScorerByName(\"sharpee\") ok = true, want false — this is not a recognized name")
	}

	r := Result{TotalReturnPct: 5, WinRatePct: 80, Sharpe: 1.2, MaxDrawdownPct: 2}
	if score(r) != ScoreBlend(r) {
		t.Errorf("unrecognized name's scorer = %v, want ScoreBlend's value (%v) as the fallback",
			score(r), ScoreBlend(r))
	}

	for _, name := range ScorerNames {
		if _, ok := ScorerByName(name); !ok {
			t.Errorf("ScorerByName(%q) ok = false, want true — every name in ScorerNames must be recognized", name)
		}
	}
}
