package backtest

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// TestMutateStaysNearParent checks the guided search's exploration step: a low
// rate keeps most of the parent, a rate of 1 resamples everything. Without this
// property "guided" search would be either random search or a no-op.
func TestMutateStaysNearParent(t *testing.T) {
	t.Parallel()

	space := defaultSpace(Pins{}, config.Default().Strategy)
	rng := rand.New(rand.NewSource(7)) //nolint:gosec // deterministic test
	parent := space.sample(rng, config.Default().Strategy)

	same, total := 0, 0

	for range 200 {
		child := space.mutate(rng, parent, 0.1)

		total++

		if child.signature() == parent.signature() {
			same++
		}
	}

	if same == 0 {
		t.Error("a 10% mutation rate never reproduced the parent; it is not a local search")
	}

	// With ~30 axes at 10%, an unchanged child is possible but should be rare.
	if same == total {
		t.Error("mutation never changed anything")
	}
}

// TestMutateFullRateExplores checks rate 1.0 is a fresh draw.
func TestMutateFullRateExplores(t *testing.T) {
	t.Parallel()

	space := defaultSpace(Pins{}, config.Default().Strategy)
	rng := rand.New(rand.NewSource(9)) //nolint:gosec // deterministic test
	parent := space.sample(rng, config.Default().Strategy)

	changed := 0

	for range 20 {
		if space.mutate(rng, parent, 1.0).signature() != parent.signature() {
			changed++
		}
	}

	if changed < 15 {
		t.Errorf("only %d/20 full-rate mutations differed from the parent", changed)
	}
}

// TestGuidedIsDeterministic checks a fixed seed reproduces the run, which is
// what makes an optimisation result something you can report and re-check.
func TestGuidedIsDeterministic(t *testing.T) {
	t.Parallel()

	data := Generate([]string{"AAPL", "MSFT"}, 400)

	a := OptimizeGuided(context.Background(), config.Default(), data, 50000, 120, 4, 1, ScoreReturn, false, 42, Pins{})
	b := OptimizeGuided(context.Background(), config.Default(), data, 50000, 120, 4, 1, ScoreReturn, false, 42, Pins{})

	if len(a) == 0 {
		t.Fatal("no trials")
	}

	if len(a) != len(b) || a[0].Score != b[0].Score ||
		a[0].Result.TotalReturnPct != b[0].Result.TotalReturnPct {
		t.Error("guided search is not reproducible for a fixed seed")
	}
}

// TestOptimizeGuidedStopsEarlyWhenContextIsCancelled reproduces the
// "cmd/optimize: no cancellation" finding for the guided/random search path:
// once ctx is cancelled, further candidates must not be evaluated, so an
// interrupted run keeps whatever it already found instead of running to
// completion regardless.
func TestOptimizeGuidedStopsEarlyWhenContextIsCancelled(t *testing.T) {
	t.Parallel()

	data := Generate([]string{"AAPL", "MSFT"}, 400)

	full := OptimizeGuided(context.Background(), config.Default(), data, 50000, 120, 4, 1, ScoreReturn, false, 42, Pins{})

	ctx := newCountAfterCtx(60) // half of n=120
	partial := OptimizeGuided(ctx, config.Default(), data, 50000, 120, 4, 1, ScoreReturn, false, 42, Pins{})

	if len(partial) >= len(full) {
		t.Errorf("cancelled run returned %d trials, want fewer than the uncancelled "+
			"run's %d", len(partial), len(full))
	}
}

// TestGuidedSingleGenerationIsRandom checks the documented equivalence: one
// generation is plain random search, not a degenerate special case.
func TestGuidedSingleGenerationIsRandom(t *testing.T) {
	t.Parallel()

	data := Generate([]string{"AAPL"}, 300)

	guided := OptimizeGuided(context.Background(), config.Default(), data, 50000, 60, 1, 1, ScoreReturn, false, 5, Pins{})
	random := OptimizeRandom(context.Background(), config.Default(), data, 50000, 60, 1, ScoreReturn, false, 5, Pins{})

	if len(guided) != len(random) {
		t.Fatalf("guided(1 gen) produced %d trials, random produced %d", len(guided), len(random))
	}

	if len(guided) > 0 && guided[0].Score != random[0].Score {
		t.Error("guided search with one generation differs from random search")
	}
}

// TestGuidedConcentratesTheSearch checks the defining property of the guided
// search: it spends its later budget near the configurations that scored well,
// instead of sampling the whole space uniformly like random search.
//
// It deliberately does NOT assert that guided beats random on this data. These
// are synthetic random walks with no structure to learn, so there is nothing for
// refinement to exploit and the better score is decided by luck. Concentration
// is a property of the algorithm and holds regardless of the data; a
// score comparison would be a coin flip dressed up as a test.
func TestGuidedConcentratesTheSearch(t *testing.T) {
	t.Parallel()

	data := Generate([]string{"AAPL", "MSFT"}, 400)

	const budget = 200

	guided := OptimizeGuided(context.Background(), config.Default(), data, 50000, budget, 5, 1, ScoreReturn, false, 11, Pins{})
	random := OptimizeRandom(context.Background(), config.Default(), data, 50000, budget, 1, ScoreReturn, false, 11, Pins{})

	if len(guided) == 0 || len(random) == 0 {
		t.Fatal("no qualifying trials")
	}

	g, r := modalShare(guided), modalShare(random)
	if g <= r {
		t.Errorf("the most-tried detector set took %.0f%% of the guided budget vs %.0f%% "+
			"of the random one; refinement should concentrate the search", g*100, r*100)
	}

	// Random search draws uniformly from 10 detector sets, so no set should
	// dominate it — if this fails, the comparison above proves nothing.
	if r > 0.25 {
		t.Errorf("random search concentrated %.0f%% on one detector set; "+
			"it should be roughly uniform", r*100)
	}
}

// modalShare is the fraction of a run's trials that used its single most common
// detector set — a measure of how concentrated the search was.
func modalShare(trials []Trial) float64 {
	if len(trials) == 0 {
		return 0
	}

	counts := make(map[string]int, len(trials))
	most := 0

	for _, t := range trials {
		key := fmt.Sprint(t.Detectors)

		counts[key]++
		if counts[key] > most {
			most = counts[key]
		}
	}

	return float64(most) / float64(len(trials))
}

// TestNewScorers checks every documented objective resolves, and that an
// unknown name falls back to the blend rather than a nil function.
func TestNewScorers(t *testing.T) {
	t.Parallel()

	r := Result{
		TotalReturnPct: 10, WinRatePct: 60, MaxDrawdownPct: 5, Sharpe: 1.5,
		Sortino: 2.5, Calmar: 1.2, ProfitFactor: 1.8, ExpectancyPct: 0.4,
	}

	for _, name := range ScorerNames {
		score, ok := ScorerByName(name)
		if score == nil {
			t.Errorf("ScorerByName(%q) = nil", name)
		}

		if !ok {
			t.Errorf("ScorerByName(%q) ok = false, want true for a name in ScorerNames", name)
		}
	}

	if score, _ := ScorerByName("sortino"); score(r) != 2.5 {
		t.Errorf("sortino scorer = %v, want 2.5", score(r))
	}

	// The calmar objective scores a smoothed ratio (see ScoreCalmar, AT2-4),
	// not the raw Result.Calmar; what this test pins is the name mapping.
	if score, _ := ScorerByName("calmar"); score(r) != ScoreCalmar(r) {
		t.Errorf("calmar scorer = %v, want ScoreCalmar's %v", score(r), ScoreCalmar(r))
	}

	if score, _ := ScorerByName("expectancy"); score(r) != 0.4 {
		t.Errorf("expectancy scorer = %v, want 0.4", score(r))
	}

	// TestScorerByNameReportsUnrecognizedNames (optimize_test.go) covers the
	// ok=false / typo-detection half of this in more depth — this check
	// keeps the pre-existing "still falls back to blend" behavior covered
	// alongside it.
	if score, ok := ScorerByName("nonsense"); score(r) != ScoreBlend(r) || ok {
		t.Errorf("unknown objective: score=%v ok=%v, want ScoreBlend's value and ok=false",
			score(r), ok)
	}
}

// TestPinsFixEveryDeploymentAxis reproduces the AT-1/AT-11 fix: the axes a
// caller cannot APPLY must be drawn from a single value instead of the grid.
// Otherwise internal/autotune — which can only write StrategyConfig fields
// into a strategy_overrides entry — either wins on position size alone
// (CapitalFrac, AT-1) or wins on an exit level it then has to throw the whole
// proposal away for depending on (StopLoss/TrailingStop/TakeProfit, AT-11).
func TestPinsFixEveryDeploymentAxis(t *testing.T) {
	t.Parallel()

	var (
		frac = 0.025
		sl   = 0.07
		ts   = 0.0 // 0 is a real pinned VALUE here, not "unpinned"
		tp   = 0.0
	)

	pins := Pins{CapitalFrac: &frac, StopLoss: &sl, TrailingStop: &ts, TakeProfit: &tp}
	space := defaultSpace(pins, config.Default().Strategy)

	check := func(where string, c candidate) {
		t.Helper()

		if c.frac != frac || c.sl != sl || c.ts != ts || c.tp != tp {
			t.Fatalf("%s: frac/sl/ts/tp = %v/%v/%v/%v, want the pinned %v/%v/%v/%v",
				where, c.frac, c.sl, c.ts, c.tp, frac, sl, ts, tp)
		}
	}

	rng := rand.New(rand.NewSource(3)) //nolint:gosec // deterministic test
	for range 50 {
		check("sample", space.sample(rng, config.Default().Strategy))
	}

	parent := space.sample(rng, config.Default().Strategy)
	for range 50 {
		// rate=1 resamples every axis, so an unpinned one would move here.
		check("mutate", space.mutate(rng, parent, 1.0))
	}

	// End to end through the actual entry point autotune calls.
	data := Generate([]string{"AAPL"}, 400)

	guided := OptimizeGuided(context.Background(), config.Default(), data, 50000, 60, 3, 1, ScoreReturn, false, 5, pins)
	if len(guided) == 0 {
		t.Fatal("no qualifying trials")
	}

	for _, tr := range guided {
		if tr.CapitalFrac != frac || tr.StopLoss != sl || tr.TrailingStop != ts || tr.TakeProfit != tp {
			t.Errorf("trial frac/sl/ts/tp = %v/%v/%v/%v, want every trial pinned to %v/%v/%v/%v",
				tr.CapitalFrac, tr.StopLoss, tr.TrailingStop, tr.TakeProfit, frac, sl, ts, tp)
		}
	}
}

// TestDefaultSpaceSeedsTheIncumbentConfig reproduces the AT-13 fix: the grid is
// a fixed menu, so a config the operator actually runs can be unreachable —
// meaning the search can never propose "keep the detectors, tune the periods"
// and every proposal necessarily changes the detector set.
func TestDefaultSpaceSeedsTheIncumbentConfig(t *testing.T) {
	t.Parallel()

	base := config.Default().Strategy
	base.Detectors = []string{"rsi", "adx"} // not one of DefaultGrid's ten sets
	base.MinStrength = 0.25                 // not one of {0.10, 0.20, 0.30}

	space := defaultSpace(Pins{}, base)

	if !slices.ContainsFunc(space.grid.DetectorSets, func(s []string) bool {
		return slices.Equal(s, base.Detectors)
	}) {
		t.Errorf("detector sets %v do not include the incumbent %v", space.grid.DetectorSets, base.Detectors)
	}

	if !slices.Contains(space.grid.MinStrength, base.MinStrength) {
		t.Errorf("MinStrength %v does not include the incumbent %v", space.grid.MinStrength, base.MinStrength)
	}

	// A config the grid already covers must not be duplicated into it.
	covered := config.Default().Strategy
	covered.Detectors = slices.Clone(DefaultGrid().DetectorSets[0])
	covered.MinStrength = 0.20

	sp2 := defaultSpace(Pins{}, covered)
	if len(sp2.grid.DetectorSets) != len(DefaultGrid().DetectorSets) ||
		len(sp2.grid.MinStrength) != len(DefaultGrid().MinStrength) {
		t.Error("an incumbent the grid already offers was appended again")
	}
}

// TestZeroPinsPreserveTheDefaultGrid checks the other half: the zero Pins
// value (cmd/optimize and every other caller) must keep exploring the grid's
// own values exactly as before pinning existed.
func TestZeroPinsPreserveTheDefaultGrid(t *testing.T) {
	t.Parallel()

	space := defaultSpace(Pins{}, config.StrategyConfig{})

	g := DefaultGrid()
	if !slices.Equal(space.grid.CapitalFrac, g.CapitalFrac) ||
		!slices.Equal(space.grid.StopLoss, g.StopLoss) ||
		!slices.Equal(space.grid.TrailingStop, g.TrailingStop) ||
		!slices.Equal(space.grid.TakeProfit, g.TakeProfit) {
		t.Errorf("zero Pins changed the grid: frac=%v sl=%v ts=%v tp=%v",
			space.grid.CapitalFrac, space.grid.StopLoss, space.grid.TrailingStop, space.grid.TakeProfit)
	}
}
