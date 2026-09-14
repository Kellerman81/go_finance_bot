package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/backtest"
	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// The field optimizer runs a real (if synthetic-data) backtest, so most of its
// behaviour is exercised at the level that matters: does it build a runnable
// config from the live settings and hand back a sweep an operator can act on.
// Driving candles through the real feed/engine pipeline just to get a
// SeriesResampled result would make these tests slow and brittle for no extra
// coverage, so runFieldSweep and scoredRows are tested directly against
// synthetic candles, and the HTTP layer is tested for the paths that do not
// need any history at all: an unknown symbol, an unsweepable field, and too
// little of it.

func TestFormApplyValueTranslatesTrendGateOnly(t *testing.T) {
	if got := formApplyValue("trend_gate", "on"); got != "true" {
		t.Errorf("trend_gate on -> %q, want true", got)
	}
	if got := formApplyValue("trend_gate", "off"); got != "false" {
		t.Errorf("trend_gate off -> %q, want false", got)
	}
	if got := formApplyValue("fast_ma", "20"); got != "20" {
		t.Errorf("fast_ma -> %q, want the label unchanged", got)
	}
}

func TestOptimizableFieldsAllHaveAnAxis(t *testing.T) {
	fields := optimizableFields()
	if len(fields) == 0 {
		t.Fatal("expected at least one optimizable field")
	}

	for _, fld := range fields {
		if _, ok := optimizeAxis[fld.Key]; !ok {
			t.Errorf("%s listed as optimizable but has no axis mapping", fld.Key)
		}
	}
}

// A field whose setting lives on the Settings tab (not the Strategy tab
// where the optimizer panel renders) has no in-page form input for the
// sweep table's Apply button to fill, so it must be listed without an apply
// target rather than pointing at an id that doesn't exist on this page.
func TestOptimizableFieldsExitAxesHaveNoApplyTarget(t *testing.T) {
	fields := optimizableFields()

	pf, ok := findPickerField(fields, "settings.exits.stop_loss_pct")
	if !ok {
		t.Fatal("expected settings.exits.stop_loss_pct in the picker")
	}

	if pf.ApplyPrefix != "" || pf.ApplyName != "" {
		t.Errorf("stop_loss_pct has an apply target %+v, want none", pf)
	}
}

// The costs block lives on the Settings tab too, and each of its five
// sweepable fields must be listed the same way: present, with no apply
// target.
func TestOptimizableFieldsCostsAxesPresentWithNoApplyTarget(t *testing.T) {
	fields := optimizableFields()

	for _, name := range []string{
		"commission_pct", "commission_flat", "commission_min", "spread_pct", "slippage_pct",
	} {
		key := "settings.costs." + name

		pf, ok := findPickerField(fields, key)
		if !ok {
			t.Errorf("expected %s in the picker", key)

			continue
		}

		if pf.ApplyPrefix != "" || pf.ApplyName != "" {
			t.Errorf("%s has an apply target %+v, want none", key, pf)
		}
	}
}

func TestRunFieldSweepCoversEachCostsAxis(t *testing.T) {
	s := newTestServer(t)

	strat := config.Default().Strategy
	strat.Detectors = []string{"ema_cross", "rsi", "macd"}
	s.eng.SetStrategyConfig(strat)

	data := backtest.Generate([]string{"AAPL"}, 400)

	for _, axis := range []string{
		"commission_pct", "commission_flat", "commission_min", "spread_pct", "slippage_pct",
	} {
		_, sweep := s.runFieldSweep("AAPL", axis, data["AAPL"])
		if sweep == nil {
			t.Errorf("expected a sweep for %s", axis)

			continue
		}

		if len(sweep.Rows) == 0 {
			t.Errorf("%s: expected at least one candidate row", axis)
		}
	}
}

// A strategy field keeps its normal same-page Apply target.
func TestOptimizableFieldsStrategyAxesHaveAnApplyTarget(t *testing.T) {
	fields := optimizableFields()

	pf, ok := findPickerField(fields, "fast_ma")
	if !ok {
		t.Fatal("expected fast_ma in the picker")
	}

	if pf.ApplyPrefix != "strategy" || pf.ApplyName != "fast_ma" {
		t.Errorf("fast_ma apply target = %+v, want strategy/fast_ma", pf)
	}
}

func TestRunFieldSweepRanksCandidatesAndMarksOneBest(t *testing.T) {
	s := newTestServer(t)

	// The test server starts from a zero-value config, so indicator periods
	// (fast_ma, rsi_period, ...) are all 0 and the strategy never trades.
	// Sensitivity needs sane periods and an active detector set for its
	// fast_ma sweep to bite.
	strat := config.Default().Strategy
	strat.Detectors = []string{"ema_cross", "rsi", "macd"}
	s.eng.SetStrategyConfig(strat)

	data := backtest.Generate([]string{"AAPL"}, 400)

	baseline, sweep := s.runFieldSweep("AAPL", "fast_ma", data["AAPL"])
	if sweep == nil {
		t.Fatal("expected a sweep for fast_ma")
	}
	if baseline.NumTrades == 0 {
		t.Error("expected the baseline to have traded against 400 synthetic bars")
	}

	rows, best := scoredRows(*sweep, "fast_ma")
	if len(rows) < 2 {
		t.Fatalf("expected multiple candidate values, got %d", len(rows))
	}
	if best < 0 || best >= len(rows) {
		t.Fatalf("best index %d out of range for %d rows", best, len(rows))
	}

	bestCount := 0
	for i, r := range rows {
		if r.Best {
			bestCount++
			if i != best {
				t.Errorf("row %d marked Best but scoredRows returned index %d", i, best)
			}
		}
		if r.ApplyValue != r.Label {
			t.Errorf("row %q: ApplyValue %q should equal Label for a non-trend_gate field",
				r.Label, r.ApplyValue)
		}
	}
	if bestCount != 1 {
		t.Errorf("expected exactly one row marked Best, got %d", bestCount)
	}
}

func TestOptimizeFieldRejectsAnUnknownSymbol(t *testing.T) {
	s := newTestServer(t)

	rec := fetch(t, s, "/v2/f/optimizeField?symbol=NOPE&field=fast_ma")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not on the watchlist") {
		t.Errorf("expected the unknown-symbol message, got: %s", rec.Body.String())
	}
}

func TestOptimizeFieldRejectsAFieldWithNoAxis(t *testing.T) {
	s := newTestServer(t)

	if err := s.eng.AddSymbols("AAPL"); err != nil {
		t.Fatalf("AddSymbols: %v", err)
	}

	// weights is a real strategy field (a map, not a scalar), but Sensitivity
	// has nothing to sweep it against — exactly the case optimizableFields()
	// is supposed to keep off the dropdown, so posting it anyway must be
	// rejected rather than panic.
	rec := fetch(t, s, "/v2/f/optimizeField?symbol=AAPL&field=weights")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no tested values") {
		t.Errorf("expected the unsweepable-field message, got: %s", rec.Body.String())
	}
}

// The not-enough-history path (len(candles) < optimizeMinBars) is not
// exercised over HTTP: adding a symbol runs the engine's synchronous warmup,
// which backfills a full history from the feed immediately, so there is no
// window in this harness where a tracked symbol has too little of it. The
// guard itself is a single comparison; TestOptimizeFieldRejectsAnUnknownSymbol
// and TestOptimizeFieldRejectsAFieldWithNoAxis cover the same handler's other
// early returns.

// TestOptimizeFieldReturnsBusyWhenASweepIsAlreadyRunning reproduces the
// "synchronous sensitivity-sweep backtest runs inline on a GET request with
// no bound on repetition or concurrency" finding: a second request arriving
// while a sweep is already in flight must fail fast with a clear message,
// not start its own concurrent sweep (unbounded CPU stacking) or queue
// indefinitely for a turn.
func TestOptimizeFieldReturnsBusyWhenASweepIsAlreadyRunning(t *testing.T) {
	s := newTestServer(t)

	if err := s.eng.AddSymbols("AAPL"); err != nil {
		t.Fatalf("AddSymbols: %v", err)
	}

	strat := config.Default().Strategy
	strat.Detectors = []string{"ema_cross", "rsi", "macd"}
	s.eng.SetStrategyConfig(strat)

	// Occupy the sole sweep slot, as if another request's sweep were
	// already running.
	sweepSem <- struct{}{}
	defer func() { <-sweepSem }()

	rec := fetch(t, s, "/v2/f/optimizeField?symbol=AAPL&field=fast_ma")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	if !strings.Contains(rec.Body.String(), "Another sweep is already running") {
		t.Errorf("expected the busy message, got: %s", rec.Body.String())
	}
}

func TestOptimizeFieldWithNoSelectionJustShowsTheForm(t *testing.T) {
	s := newTestServer(t)

	rec := fetch(t, s, "/v2/f/optimizeField")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "opt.") {
		t.Errorf("an untranslated key leaked into the response: %s", rec.Body.String())
	}
}
