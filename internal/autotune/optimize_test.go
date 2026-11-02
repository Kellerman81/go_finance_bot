package autotune

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/backtest"
	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/engine"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
	"github.com/Kellerman81/go_finance_bot/internal/storage"
)

// newTestEngine builds the minimal engine/risk/portfolio trio
// RunSymbolOptimize needs, without pulling in internal/api. store may be
// nil (falls back to an in-memory no-op store, same as engine.New does for
// every other test in this codebase that doesn't care about persistence).
func newTestEngine(t *testing.T, store engine.Store) (*engine.Engine, *risk.Manager, *portfolio.Portfolio) {
	t.Helper()

	var cfg config.Config

	prices := engine.NewPriceCache()
	feed := market.NewMock(time.Hour)
	rm := risk.New(cfg.Limits, cfg.Costs)
	pf := portfolio.New()
	pf.SetCash(10000)
	brk := broker.NewSim(10000, prices.Get, cfg.Costs.Commission)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	eng := engine.New(cfg.Engine, cfg.Exits, cfg.Safety, cfg.Strategy, feed, brk, rm, pf, prices, store, log)

	t.Cleanup(func() {
		eng.Close()
		feed.Close()
	})

	return eng, rm, pf
}

func TestRunSymbolOptimizeProducesABaselineAndAProposal(t *testing.T) {
	eng, rm, pf := newTestEngine(t, nil)

	strat := config.Default().Strategy
	strat.Detectors = []string{"ema_cross", "rsi", "macd"}
	eng.SetStrategyConfig(strat)

	data := backtest.Generate([]string{"AAPL"}, 400)

	result := RunSymbolOptimize(context.Background(), eng, rm, pf, "AAPL", data["AAPL"], DefaultMinTrades, DefaultObjective, DefaultMinReturnPct)

	if result.Symbol != "AAPL" {
		t.Errorf("Symbol = %q, want AAPL", result.Symbol)
	}

	if result.Bars != 400 {
		t.Errorf("Bars = %d, want 400", result.Bars)
	}

	if result.WhatIf.NumTrades == 0 {
		t.Error("expected the what-if baseline to have traded against 400 synthetic bars")
	}

	// This engine has no storage-backed trade history at all (nil store),
	// so there is nothing real to compare against — exactly the "fresh
	// install" case the dual-baseline design exists to handle gracefully
	// rather than leave undefined.
	if result.HasReal {
		t.Errorf("HasReal = true with no trade history at all, want false: %+v", result.Real)
	}

	// Best must always be at least as good as the baseline — Refine only ever
	// adopts a variant that scores higher than what it started from, and the
	// search's own winner is only adopted when it beats the baseline trial.
	if result.Best.Score < 0 && result.Improved {
		t.Errorf("Improved=true but best score %v looks worse than a sane baseline", result.Best.Score)
	}

	if result.Improved {
		// At least detectors/combine/min_strength; sell-side/trend_gate lines
		// are appended only when Refine actually changed them from base, so
		// more than 3 is normal, not a bug — see
		// TestProposedOverrideIncludesTrendGateWhenRefineChangesIt for the
		// exact-content regression coverage of that logic.
		if len(result.Proposed) < 3 {
			t.Errorf("Proposed = %v, want at least 3 lines (detectors/combine/min_strength)", result.Proposed)
		}

		if len(result.Reasoning) < 2 {
			t.Errorf("Reasoning = %v, want at least the baseline/proposed summary lines", result.Reasoning)
		}
	}
}

// TestRunSymbolOptimizeUsesTheRequestedObjective checks a recognized
// objective name actually reaches the search (echoed back in Result so a
// caller can display what "Best" was judged by), and that an unrecognized
// one falls back to DefaultObjective ("blend") rather than silently ranking
// candidates by something nobody asked for or erroring — matching
// backtest.ScorerByName's own documented fallback behavior.
func TestRunSymbolOptimizeUsesTheRequestedObjective(t *testing.T) {
	eng, rm, pf := newTestEngine(t, nil)

	strat := config.Default().Strategy
	strat.Detectors = []string{"ema_cross", "rsi", "macd"}
	eng.SetStrategyConfig(strat)

	data := backtest.Generate([]string{"AAPL"}, 400)

	if got := RunSymbolOptimize(context.Background(), eng, rm, pf, "AAPL", data["AAPL"], DefaultMinTrades, "return", DefaultMinReturnPct).Objective; got != "return" {
		t.Errorf("Objective = %q, want the requested \"return\"", got)
	}

	if got := RunSymbolOptimize(context.Background(), eng, rm, pf, "AAPL", data["AAPL"], DefaultMinTrades, "not-a-real-objective", DefaultMinReturnPct).Objective; got != DefaultObjective {
		t.Errorf("Objective = %q, want the fallback %q for an unrecognized name", got, DefaultObjective)
	}

	if got := RunSymbolOptimize(context.Background(), eng, rm, pf, "AAPL", data["AAPL"], DefaultMinTrades, "", DefaultMinReturnPct).Objective; got != DefaultObjective {
		t.Errorf("Objective = %q, want the fallback %q for an empty name", got, DefaultObjective)
	}
}

// TestRunSymbolOptimizeNeverReportsImprovedWhenNotOverridable reproduces the
// AT-5 finding end to end: Improved used to be computed from beatsBothBaselines
// alone, so a winning trial whose edge depended on a setting Fields/Proposed
// cannot express (confirm_buy, exits being force-enabled during the search
// while the symbol's real config has them off, ...) could still be reported
// as an improvement and applied — a config change that was never actually
// backtested. newTestEngine's zero-value config has Exits.Enabled=false,
// which nonOverridableChanges now always flags (every search candidate is
// backtested with exits force-enabled — see runConfig), so this is a
// deterministic way to drive that gate regardless of what the search itself
// finds.
func TestRunSymbolOptimizeNeverReportsImprovedWhenNotOverridable(t *testing.T) {
	eng, rm, pf := newTestEngine(t, nil)

	strat := config.Default().Strategy
	strat.Detectors = []string{"ema_cross", "rsi", "macd"}
	eng.SetStrategyConfig(strat)
	// Exits stay at the engine's zero-value config (Enabled: false).

	data := backtest.Generate([]string{"AAPL"}, 400)

	result := RunSymbolOptimize(context.Background(), eng, rm, pf, "AAPL", data["AAPL"], DefaultMinTrades, DefaultObjective, DefaultMinReturnPct)

	if !slices.Contains(result.NotOverridable, "exits_enabled") {
		t.Fatalf("NotOverridable = %v, want it to include exits_enabled (base exits are disabled)", result.NotOverridable)
	}

	if result.Improved {
		t.Errorf("Improved = true with a non-empty NotOverridable (%v) — "+
			"a win that depends on a setting the proposal cannot express must not be reported as an improvement",
			result.NotOverridable)
	}
}

// TestProposedOverrideIncludesTrendGateWhenRefineChangesIt reproduces the
// bug a user caught by inspection: Refine has a dedicated axis
// (refine.go's trendGate method) that can flip Params.TrendGate on the
// winning trial, and unlike detectors/combine/min_strength, that change was
// silently dropped from the proposed strategy_overrides snippet — a
// strategy_overrides entry that doesn't name trend_gate INHERITS the shared
// config's value, not whatever the search found best, so the snippet did
// not actually reproduce the numbers shown next to it. trend_gate is a
// StrategyConfig field like any other, so it belongs in the proposal
// whenever it differs from the base — same treatment as detectors_sell/
// combine_sell/min_strength_sell, the other fields Refine can change that
// strategy_overrides can express.
func TestProposedOverrideIncludesTrendGateWhenRefineChangesIt(t *testing.T) {
	base := config.StrategyConfig{
		Detectors: []string{"ema_cross"}, Combine: "weighted", MinStrength: 0.3,
		TrendGate: true, MinStrengthSell: -1,
	}

	unchanged := backtest.Trial{
		Detectors: []string{"ema_cross", "rsi"}, Combine: "unanimous", MinStrength: 0.2,
		Params: base, // TrendGate/DetectorsSell/CombineSell/MinStrengthSell all inherited
	}

	if got := proposedOverride(base, unchanged); len(got) != 3 {
		t.Errorf(
			"proposedOverride with nothing else changed = %v, want exactly the 3 base lines",
			got,
		)
	}

	flipped := unchanged
	flipped.Params = base
	flipped.Params.TrendGate = false // this is what Refine's trendGate() axis does

	got := proposedOverride(base, flipped)

	found := false

	for _, line := range got {
		if line == "trend_gate: false" {
			found = true
		}
	}

	if !found {
		t.Errorf("proposedOverride = %v, want a \"trend_gate: false\" line when Refine flipped it", got)
	}
}

// TestOverrideFieldsMatchesTheStringFormatter checks overrideFields (the
// structured decision Schedule's live-apply path uses) and proposedOverride
// (the string formatter over it) agree on which fields differ from base —
// one decision, not two that could drift.
func TestOverrideFieldsMatchesTheStringFormatter(t *testing.T) {
	base := config.StrategyConfig{
		Detectors: []string{"ema_cross"}, Combine: "weighted", MinStrength: 0.3,
		TrendGate: true, MinStrengthSell: -1,
	}

	trial := backtest.Trial{
		Detectors: []string{"ema_cross", "rsi"}, Combine: "unanimous", MinStrength: 0.2,
		Params: base,
	}
	trial.Params.TrendGate = false
	trial.Params.DetectorsSell = []string{"rsi"}

	fields := overrideFields(base, trial)

	if !slices.Equal(fields.Detectors, trial.Detectors) || fields.Combine != trial.Combine {
		t.Errorf("Fields = %+v, want detectors/combine to match the trial", fields)
	}

	if fields.TrendGate == nil || *fields.TrendGate != false {
		t.Errorf("Fields.TrendGate = %v, want a set false", fields.TrendGate)
	}

	if fields.DetectorsSell == nil || !slices.Equal(*fields.DetectorsSell, []string{"rsi"}) {
		t.Errorf("Fields.DetectorsSell = %v, want [rsi]", fields.DetectorsSell)
	}

	rendered := formatOverride(fields)

	found := false

	for _, line := range rendered {
		if line == "trend_gate: false" {
			found = true
		}
	}

	if !found {
		t.Errorf("formatOverride(overrideFields(...)) = %v, want a trend_gate: false line matching proposedOverride", rendered)
	}
}

// TestOverrideFieldsDiffsIndicatorPeriods reproduces the AT-2 fix:
// overrideFields used to deliberately drop every indicator-period field
// (rsi_period, macd_fast, ...) as "sampling noise", even though they are
// real inputs to the Result a proposal is judged and displayed by — so the
// config actually written to strategy_overrides did not reproduce the one
// that was backtested and gated. They must now be diffed and rendered like
// every other field.
func TestOverrideFieldsDiffsIndicatorPeriods(t *testing.T) {
	base := config.StrategyConfig{
		Detectors: []string{"rsi", "macd"}, Combine: "weighted", MinStrength: 0.3,
		RSIPeriod: 14, MACDFast: 12, MACDSlow: 26, MACDSignal: 9,
	}

	trial := backtest.Trial{
		Detectors: []string{"rsi", "macd"}, Combine: "weighted", MinStrength: 0.3,
		Params: base,
	}
	// What a guided-search candidate actually looks like: it resampled
	// these axes independently of base.
	trial.Params.RSIPeriod = 7
	trial.Params.MACDFast, trial.Params.MACDSlow, trial.Params.MACDSignal = 5, 35, 5

	fields := overrideFields(base, trial)

	if fields.RSIPeriod == nil || *fields.RSIPeriod != 7 {
		t.Errorf("Fields.RSIPeriod = %v, want a set 7", fields.RSIPeriod)
	}

	if fields.MACDFast == nil || *fields.MACDFast != 5 ||
		fields.MACDSlow == nil || *fields.MACDSlow != 35 ||
		fields.MACDSignal == nil || *fields.MACDSignal != 5 {
		t.Errorf("Fields.MACD{Fast,Slow,Signal} = %v/%v/%v, want 5/35/5",
			fields.MACDFast, fields.MACDSlow, fields.MACDSignal)
	}

	// Applying the fields must reproduce exactly the periods the trial was
	// backtested and scored with — the whole point of the fix.
	applied, err := config.ApplyStrategyOverride(base, fields)
	if err != nil {
		t.Fatalf("ApplyStrategyOverride: %v", err)
	}

	if applied.RSIPeriod != trial.Params.RSIPeriod ||
		applied.MACDFast != trial.Params.MACDFast ||
		applied.MACDSlow != trial.Params.MACDSlow ||
		applied.MACDSignal != trial.Params.MACDSignal {
		t.Errorf("applied = %+v, want it to reproduce the backtested trial %+v", applied, trial.Params)
	}

	rendered := formatOverride(fields)
	if !slices.Contains(rendered, "rsi_period: 7") {
		t.Errorf("formatOverride = %v, want an rsi_period: 7 line", rendered)
	}

	// A field the trial did NOT change from base must stay absent — this
	// is still a diff, not "dump every field".
	if fields.BBPeriod != nil {
		t.Errorf("Fields.BBPeriod = %v, want nil (base and trial agree, nothing changed)", fields.BBPeriod)
	}
}

// TestNonOverridableChangesFlagsSizingAndExitLevels reproduces the AT-3/
// AT-6 findings: runTrial (internal/backtest/optimize.go) draws CapitalFrac
// and the three exit levels from the grid, and forces Exits.Enabled true,
// on EVERY candidate — not just Refine's own axes — so a winning trial
// routinely differs from base on these even though the old
// nonOverridableChanges never looked at them, silently under-reporting
// which numbers a pasted Proposed snippet would fail to reproduce.
func TestNonOverridableChangesFlagsSizingAndExitLevels(t *testing.T) {
	cfg := config.Config{
		Exits: config.ExitsConfig{
			Enabled: true, StopLossPct: 0.05, TrailingStopPct: 0.08, TakeProfitPct: 0.20,
		},
	}

	trial := backtest.Trial{
		CapitalFrac: 0.50, // baseFrac below is 0.025 — a legal grid pick, unequal
		Exits: config.ExitsConfig{
			Enabled: true, StopLossPct: 0.10, TrailingStopPct: 0, TakeProfitPct: 0,
		},
	}

	got := nonOverridableChanges(cfg, trial, 0.025)

	want := map[string]bool{
		"capital_frac": true, "stop_loss_pct": true,
		"trailing_stop_pct": true, "take_profit_pct": true,
	}
	for _, name := range got {
		if !want[name] {
			t.Errorf("unexpected name %q in %v", name, got)
		}

		delete(want, name)
	}

	if len(want) != 0 {
		t.Errorf("nonOverridableChanges = %v, missing %v", got, want)
	}

	// exits_enabled: the search always forces Enabled=true on a candidate,
	// so a base with exits actually disabled can never be a fair
	// comparison — this must be flagged too.
	cfgExitsOff := cfg
	cfgExitsOff.Exits.Enabled = false

	sameSizing := backtest.Trial{
		CapitalFrac: 0.025,
		Exits: config.ExitsConfig{
			Enabled: true, StopLossPct: 0.05, TrailingStopPct: 0.08, TakeProfitPct: 0.20,
		},
	}

	if got := nonOverridableChanges(cfgExitsOff, sameSizing, 0.025); !slices.Contains(got, "exits_enabled") {
		t.Errorf("nonOverridableChanges = %v, want it to include exits_enabled when base exits are disabled", got)
	}
}

// TestNonOverridableChangesNamesEngineAndExitsFields reproduces the other
// half of the same finding: confirm_buy/confirm_sell/keep_interval/
// risk_per_trade_pct (EngineConfig) and atr_stop_mult/atr_trail_mult
// (ExitsConfig) are NOT expressible in a strategy_overrides entry at all —
// there is no per-symbol override mechanism for those sections. When
// Refine's behaviour()/adaptiveExits() axes improve a trial's
// score via one of them, the caller must disclose that the proposed
// snippet alone won't reproduce the win, not silently claim it will.
func TestNonOverridableChangesNamesEngineAndExitsFields(t *testing.T) {
	cfg := config.Config{
		Engine: config.EngineConfig{ConfirmBuy: 1, ConfirmSell: 1},
		Exits:  config.ExitsConfig{Enabled: true, ATRStopMult: 0, ATRTrailMult: 0},
	}

	same := backtest.Trial{
		ConfirmBuy: 1, ConfirmSell: 1,
		Exits: config.ExitsConfig{Enabled: true, ATRStopMult: 0, ATRTrailMult: 0},
	}

	if got := nonOverridableChanges(cfg, same, 0); len(got) != 0 {
		t.Errorf("nonOverridableChanges with nothing changed = %v, want none", got)
	}

	changed := backtest.Trial{
		ConfirmBuy: 3, ConfirmSell: 1,
		Exits: config.ExitsConfig{Enabled: true, ATRStopMult: 2, ATRTrailMult: 0},
	}

	got := nonOverridableChanges(cfg, changed, 0)

	want := map[string]bool{"confirm_buy": true, "atr_stop_mult": true}
	for _, name := range got {
		if !want[name] {
			t.Errorf("unexpected name %q in %v", name, got)
		}

		delete(want, name)
	}

	if len(want) != 0 {
		t.Errorf("nonOverridableChanges = %v, missing %v", got, want)
	}
}

// TestBeatsBothBaselinesRequiresBeatingWhatIfAndReal reproduces the user's
// exact concern: judging a proposal only against the isolated what-if
// backtest (which hands the symbol the FULL account cash, alone, for the
// whole run — something real trading never does since capital is shared
// across the whole watchlist) can call it an improvement even though it
// never beat what the symbol actually, really did. A winning trial must
// beat BOTH: the what-if backtest, and — whenever the symbol has a real
// track record — that real track record too.
func TestBeatsBothBaselinesRequiresBeatingWhatIfAndReal(t *testing.T) {
	baseTrial := backtest.Trial{Score: 1.0}

	strongReal := engine.SymbolStats{Trades: 20, WinRatePct: 70, ExpectancyPct: 2.0}

	// Beats the what-if baseline (score), but not the strong real track
	// record (lower win rate AND lower expectancy) — must NOT be marked
	// improved, or the proposal would claim a win the account never
	// actually would have had over its real history.
	beatsWhatIfOnly := backtest.Trial{
		Score: 2.0, Result: backtest.Result{WinRatePct: 55, ExpectancyPct: 0.5},
	}

	if beatsBothBaselines(beatsWhatIfOnly, baseTrial, true, strongReal, DefaultMinReturnPct, false, engine.SymbolStats{}) {
		t.Error("beating only the what-if baseline (not the real one) must not count as improved")
	}

	// Beats both.
	beatsBoth := backtest.Trial{
		Score: 2.0, Result: backtest.Result{WinRatePct: 75, ExpectancyPct: 2.5},
	}

	if !beatsBothBaselines(beatsBoth, baseTrial, true, strongReal, DefaultMinReturnPct, false, engine.SymbolStats{}) {
		t.Error("beating both baselines must count as improved")
	}

	// Beats the real track record's numbers but not the what-if score —
	// must also not count; the config itself has to be sound too.
	loseOnScore := backtest.Trial{
		Score: 0.5, Result: backtest.Result{WinRatePct: 90, ExpectancyPct: 5.0},
	}

	if beatsBothBaselines(loseOnScore, baseTrial, true, strongReal, DefaultMinReturnPct, false, engine.SymbolStats{}) {
		t.Error("losing on score (vs. the what-if baseline) must not count as improved, real numbers or not")
	}

	// No real track record at all (fresh symbol / fresh install): judged on
	// the what-if score alone.
	if !beatsBothBaselines(beatsWhatIfOnly, baseTrial, false, engine.SymbolStats{}, DefaultMinReturnPct, false, engine.SymbolStats{}) {
		t.Error("with no real baseline, beating the what-if score alone must count as improved")
	}
}

// TestBeatsBothBaselinesRejectsALossmakingProposal reproduces a user-caught
// gap: an empty what-if baseline (0 trades, score ~0 — nothing traded, so
// nothing to beat) and no real track record in the window let a candidate
// with a NEGATIVE total return still get marked "best" and Improved, purely
// because blend's win-rate/drawdown terms were enough to edge out a
// baseline score of zero. A proposal that loses money is not an
// improvement over doing nothing, whatever its win rate says — MinReturnPct
// (0 by default) must reject it regardless of hasReal or the score gate.
func TestBeatsBothBaselinesRejectsALossmakingProposal(t *testing.T) {
	emptyBaseline := backtest.Trial{Score: 0, Result: backtest.Result{NumTrades: 0, TotalReturnPct: 0}}

	// 100% win rate, thin sample (3 trades), but the total return is
	// negative — exactly the shape from the reported screenshot (SIE.DE:
	// win 100%, trades 3, expectancy 0.19%, return -0.45%).
	lossmaking := backtest.Trial{
		Score: 1.5, // beats the empty baseline's ~0 score under blend
		Result: backtest.Result{
			NumTrades: 3, WinRatePct: 100, ExpectancyPct: 0.19, TotalReturnPct: -0.45,
		},
	}

	if beatsBothBaselines(lossmaking, emptyBaseline, false, engine.SymbolStats{}, 0, false, engine.SymbolStats{}) {
		t.Error("a candidate with a negative total return must not count as improved, whatever its win rate or score")
	}

	// A break-even or positive return, otherwise identical, is allowed
	// through (MinReturnPct's floor is 0 by default: profitable, not
	// merely score-positive).
	profitable := lossmaking
	profitable.Result.TotalReturnPct = 0.01

	if !beatsBothBaselines(profitable, emptyBaseline, false, engine.SymbolStats{}, 0, false, engine.SymbolStats{}) {
		t.Error("a candidate with a positive total return, otherwise identical, must be allowed through")
	}

	// A configured, deliberately negative floor (an operator explicitly
	// tolerating a small loss) lets the same lossmaking candidate through.
	if !beatsBothBaselines(lossmaking, emptyBaseline, false, engine.SymbolStats{}, -1.0, false, engine.SymbolStats{}) {
		t.Error("a negative MinReturnPct configured deliberately must be honored, not silently floored at 0")
	}
}

// TestBeatsBothBaselinesProtectsARecentWin covers the user-requested
// "prefer a config that's been winning, don't prefer one that's been
// losing" behavior: hasRecentWin adds an independent bar the candidate
// must ALSO clear (beat the recent record's own win rate and expectancy)
// — protecting a symbol whose LAST autotune-applied change is already
// winning in real trading from being readily replaced — while
// hasRecentWin=false (no recorded apply, no real trades since it, or a
// losing/breakeven record) adds no extra hurdle at all, so a losing recent
// record is exactly as easy to replace as having no history.
func TestBeatsBothBaselinesProtectsARecentWin(t *testing.T) {
	baseTrial := backtest.Trial{Score: 1.0}
	recentWin := engine.SymbolStats{Trades: 10, WinRatePct: 80, ExpectancyPct: 1.5}

	// Beats the what-if baseline decisively, but not the recent win's own
	// numbers — must be rejected specifically because of hasRecentWin.
	beatsBaselineOnly := backtest.Trial{
		Score: 2.0, Result: backtest.Result{WinRatePct: 60, ExpectancyPct: 0.5},
	}

	if beatsBothBaselines(beatsBaselineOnly, baseTrial, false, engine.SymbolStats{}, 0, true, recentWin) {
		t.Error("beating the what-if baseline but not the recent win must not count as improved")
	}

	// Beats everything, including the recent win — must be allowed through.
	beatsEverything := backtest.Trial{
		Score: 2.0, Result: backtest.Result{WinRatePct: 90, ExpectancyPct: 2.0},
	}

	if !beatsBothBaselines(beatsEverything, baseTrial, false, engine.SymbolStats{}, 0, true, recentWin) {
		t.Error("beating the what-if baseline AND the recent win must count as improved")
	}

	// The SAME beatsBaselineOnly candidate, but hasRecentWin=false (no
	// recorded apply, or a losing recent record) — nothing extra to beat,
	// so it must be allowed through this time.
	if !beatsBothBaselines(beatsBaselineOnly, baseTrial, false, engine.SymbolStats{}, 0, false, engine.SymbolStats{}) {
		t.Error("with hasRecentWin=false, the recent-win gate must add no hurdle at all")
	}
}

// TestBestClearingReturnFloorFallsBackToAProfitableAlternative reproduces
// the exact shape a user caught live: under blend, ScoreBlend's
// (winRate-50)*0.04 term can legitimately outweigh a real difference in
// return (a 50-point win-rate edge is worth +2.0, more than any realistic
// return swing on a handful of trades), so the guided search's own top
// scorer can be a thin, near-100%-win-rate LOSS even though a genuinely
// profitable, merely-decent-win-rate candidate was evaluated in the very
// same search and simply scored lower. RunSymbolOptimize must not give up
// when its own top pick fails MinReturnPct — it must fall back to the
// best-scoring alternative that clears the floor, since one was right
// there.
func TestBestClearingReturnFloorFallsBackToAProfitableAlternative(t *testing.T) {
	baseTrial := backtest.Trial{Score: 0.5}

	// Sorted best-first, as backtest.OptimizeGuided always returns it.
	guided := []backtest.Trial{
		// The formula's own favorite: outscores everything below despite
		// losing money — exactly the blend(-0.32%, 100%, 0.45%) = 1.635
		// case measured live.
		{Score: 1.635, Result: backtest.Result{TotalReturnPct: -0.32, WinRatePct: 100}},
		// A losing candidate that also beats baseline — must be skipped,
		// not just the top one.
		{Score: 0.9, Result: backtest.Result{TotalReturnPct: -0.10, WinRatePct: 80}},
		// The profitable alternative — ranked below both losing candidates
		// but still strictly above baseTrial's 0.5.
		{Score: 0.6, Result: backtest.Result{TotalReturnPct: 0.46, WinRatePct: 50}},
		// Below baseline's own score — must never be reached, even though
		// it's technically profitable, since guided is sorted and the
		// search stops at the first score that doesn't beat baseline.
		{Score: 0.4, Result: backtest.Result{TotalReturnPct: 5.0, WinRatePct: 90}},
	}

	got, ok := bestClearingReturnFloor(guided, baseTrial, 0)
	if !ok {
		t.Fatal("bestClearingReturnFloor found nothing, want the 0.6-score profitable candidate")
	}

	if got.Score != 0.6 || got.Result.TotalReturnPct != 0.46 {
		t.Errorf("got %+v, want the profitable 0.6-score candidate, not the higher-scoring loss or the below-baseline profit", got)
	}

	// No candidate at all clears the floor: nothing to fall back to.
	if _, ok := bestClearingReturnFloor(guided, baseTrial, 10.0); ok {
		t.Error("bestClearingReturnFloor returned ok=true with an unreachable floor")
	}

	// Nothing beats baseline at all: nothing to fall back to, regardless
	// of the floor.
	strongBaseline := backtest.Trial{Score: 2.0}
	if _, ok := bestClearingReturnFloor(guided, strongBaseline, 0); ok {
		t.Error("bestClearingReturnFloor returned ok=true when nothing in guided beats the baseline's score")
	}
}

// TestRunSymbolOptimizeReportsBelowReturnFloor reproduces a second,
// UI-level gap a user caught after MinReturnPct itself started rejecting a
// lossmaking proposal (see TestBeatsBothBaselinesRejectsALossmakingProposal):
// with no explanation, the "proposed" row's numbers looked identical
// whether MinReturnPct was 0 or 0.01, and the page gave no indication that
// a return floor was even the reason — retrying with a different floor
// value looked like nothing had happened. Result.BelowReturnFloor exists so
// a caller (the symbolOptimize template) can say specifically why, instead
// of falling back to the generic "nothing to propose" message that gives
// no hint that the winning candidate never changes with the floor.
func TestRunSymbolOptimizeReportsBelowReturnFloor(t *testing.T) {
	eng, rm, pf := newTestEngine(t, nil)

	strat := config.Default().Strategy
	strat.Detectors = []string{"ema_cross", "rsi", "macd"}
	eng.SetStrategyConfig(strat)

	data := backtest.Generate([]string{"AAPL"}, 400)

	// An unreachably high floor: whatever the search finds, its return
	// cannot possibly clear it, so BelowReturnFloor must be set and it
	// must echo back the floor that was actually used.
	result := RunSymbolOptimize(context.Background(), eng, rm, pf, "AAPL", data["AAPL"], DefaultMinTrades, DefaultObjective, 100000)

	if !result.BelowReturnFloor {
		t.Errorf("BelowReturnFloor = false with an unreachable 100000%% floor and Best.Result.TotalReturnPct = %v",
			result.Best.Result.TotalReturnPct)
	}

	if result.Improved {
		t.Error("Improved = true despite falling below the return floor")
	}

	if result.MinReturnPct != 100000 {
		t.Errorf("MinReturnPct = %v, want the 100000 that was passed in", result.MinReturnPct)
	}

	// The same run against the default (0) floor: BelowReturnFloor must
	// track that specific run's own floor, not some fixed value.
	result = RunSymbolOptimize(context.Background(), eng, rm, pf, "AAPL", data["AAPL"], DefaultMinTrades, DefaultObjective, DefaultMinReturnPct)
	if result.MinReturnPct != DefaultMinReturnPct {
		t.Errorf("MinReturnPct = %v, want the default %v", result.MinReturnPct, DefaultMinReturnPct)
	}
}

// TestRunSymbolOptimizeWindowsRealAgainstTheBacktestSpan reproduces a gap a
// user caught: comparing "the last ~400 cached bars" (what-if) against "this
// symbol's entire trade history" (real) is not actually the same question —
// a symbol's real history can span weeks under settings that have since
// changed. Real must be windowed to the SAME span what-if just backtested
// (candles[0].Time onward), with the true all-time picture kept separately
// as Total, shown for context but never fed into Improved. Ten strong wins
// are seeded: seven inside backtest.Generate(..., 400)'s ~400-minute window,
// three well before it — Real must see only the seven, Total must see all
// ten.
//
// This also ties the real gate to the full pipeline end to end (unlike the
// pure-function test above): a guided search over 400 bars of random-walk
// synthetic data has no realistic way to systematically beat a 100% win
// rate, so Improved staying false here is proof the gate is actually wired
// in, not just correct in isolation.
func TestRunSymbolOptimizeWindowsRealAgainstTheBacktestSpan(t *testing.T) {
	db, err := storage.OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	ptrF := func(v float64) *float64 { return &v }
	ptrB := func(v bool) *bool { return &v }

	now := time.Now().UTC()

	saveWin := func(minutesAgo int) {
		if err := db.SaveTrade(engine.TradeRecord{
			Time: now.Add(-time.Duration(minutesAgo) * time.Minute), Symbol: "AAPL", Side: broker.Sell,
			Qty: 1, Status: "filled", Reason: "ema_cross:SELL(0.4) => SELL by weighted",
			PnL: ptrF(50), PnLPct: ptrF(15), Win: ptrB(true), // every trade a strong win
		}); err != nil {
			t.Fatalf("save trade: %v", err)
		}
	}

	// Inside backtest.Generate(..., 400)'s ~400-minute window, with a
	// generous safety margin against Mock.Candles' exact bar-count/timing
	// (it doesn't guarantee precisely 400 candles at exactly this instant).
	for _, minutesAgo := range []int{10, 40, 70, 100, 130, 160, 190} {
		saveWin(minutesAgo)
	}

	// Well before it — real history, but not from the span being backtested.
	for _, minutesAgo := range []int{500, 600, 700} {
		saveWin(minutesAgo)
	}

	strat := config.Default().Strategy
	strat.Detectors = []string{"ema_cross", "rsi", "macd"}

	eng, rm, pf := newTestEngine(t, db)
	eng.SetStrategyConfig(strat)

	data := backtest.Generate([]string{"AAPL"}, 400)

	result := RunSymbolOptimize(context.Background(), eng, rm, pf, "AAPL", data["AAPL"], DefaultMinTrades, DefaultObjective, DefaultMinReturnPct)

	if !result.HasReal {
		t.Fatal("HasReal = false, want true — 7 real closed trades fall inside the backtest window")
	}

	if result.Real.Trades != 7 || result.Real.WinRatePct != 100 || result.Real.ExpectancyPct != 15 {
		t.Errorf(
			"Real = %+v, want {Trades:7 WinRatePct:100 ExpectancyPct:15 ...} — only the "+
				"windowed trades, not all 10",
			result.Real,
		)
	}

	if !result.HasTotal || result.Total.Trades != 10 {
		t.Errorf("Total = %+v (HasTotal=%v), want all 10 trades regardless of window",
			result.Total, result.HasTotal)
	}

	if result.Improved {
		t.Errorf(
			"Improved = true against a 100%% real win rate — a synthetic random-walk "+
				"search has no realistic way to systematically beat that; got Best.Result=%+v",
			result.Best.Result,
		)
	}
}

// TestRunSymbolOptimizeProtectsARecentAutotuneWin reproduces the
// user-requested "prefer configs that have only produced wins" behavior
// end to end: autotune's own last applied change to a symbol, recorded via
// Engine.RecordAutotuneApply, is what recentWinFor measures real trades
// against — a real winning record since THAT specific timestamp (not the
// backtest window, and not all-time) must make the symbol hard to
// replace, exactly like TestRunSymbolOptimizeWindowsRealAgainstTheBacktestSpan
// proves for the ordinary real-vs-what-if gate.
func TestRunSymbolOptimizeProtectsARecentAutotuneWin(t *testing.T) {
	db, err := storage.OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	strat := config.Default().Strategy
	strat.Detectors = []string{"ema_cross", "rsi", "macd"}

	eng, rm, pf := newTestEngine(t, db)
	eng.SetStrategyConfig(strat)

	ptrF := func(v float64) *float64 { return &v }
	ptrB := func(v bool) *bool { return &v }

	eng.RecordAutotuneApply("AAPL")

	lastApply, ok := eng.LastAutotuneApply("AAPL")
	if !ok {
		t.Fatal("RecordAutotuneApply did not record anything")
	}

	// A few strong wins, all strictly AFTER the recorded apply — a real
	// winning record for autotune's own last decision on this symbol.
	for i, secAfter := range []int{1, 2, 3} {
		if err := db.SaveTrade(engine.TradeRecord{
			Time: lastApply.Add(time.Duration(secAfter) * time.Second), Symbol: "AAPL", Side: broker.Sell,
			Qty: 1, Status: "filled", Reason: "ema_cross:SELL(0.4) => SELL by weighted",
			PnL: ptrF(50), PnLPct: ptrF(15), Win: ptrB(true),
		}); err != nil {
			t.Fatalf("save trade %d: %v", i, err)
		}
	}

	data := backtest.Generate([]string{"AAPL"}, 400)

	result := RunSymbolOptimize(context.Background(), eng, rm, pf, "AAPL", data["AAPL"], DefaultMinTrades, DefaultObjective, DefaultMinReturnPct)

	if !result.HasRecentWin {
		t.Fatal("HasRecentWin = false, want true — 3 real wins since the last autotune apply")
	}

	if result.RecentWin.Trades != 3 || result.RecentWin.WinRatePct != 100 {
		t.Errorf("RecentWin = %+v, want {Trades:3 WinRatePct:100 ...}", result.RecentWin)
	}

	if result.Improved {
		t.Errorf(
			"Improved = true despite a 100%% real win rate since the last autotune apply — a "+
				"synthetic random-walk search has no realistic way to systematically beat that; "+
				"got Best.Result=%+v", result.Best.Result,
		)
	}
}

// TestRecentWinForIgnoresALosingRecord checks the asymmetric half of the
// design: a symbol whose real trading has been NET NEGATIVE since
// autotune's own last change to it reports hasRecentWin=false — a losing
// recent record protects nothing and adds no extra hurdle, so autotune
// stays free to replace it.
func TestRecentWinForIgnoresALosingRecord(t *testing.T) {
	db, err := storage.OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	eng, _, _ := newTestEngine(t, db)

	ptrF := func(v float64) *float64 { return &v }
	ptrB := func(v bool) *bool { return &v }

	eng.RecordAutotuneApply("AAPL")

	lastApply, ok := eng.LastAutotuneApply("AAPL")
	if !ok {
		t.Fatal("RecordAutotuneApply did not record anything")
	}

	// A losing trade after the apply.
	if err := db.SaveTrade(engine.TradeRecord{
		Time: lastApply.Add(time.Second), Symbol: "AAPL", Side: broker.Sell,
		Qty: 1, Status: "filled", Reason: "stop_loss",
		PnL: ptrF(-50), PnLPct: ptrF(-15), Win: ptrB(false),
	}); err != nil {
		t.Fatalf("save trade: %v", err)
	}

	if _, ok := recentWinFor(eng, "AAPL"); ok {
		t.Error("recentWinFor reported a win for a net-negative record since the last apply")
	}

	// And a symbol with no recorded apply at all.
	if _, ok := recentWinFor(eng, "MSFT"); ok {
		t.Error("recentWinFor reported a win for a symbol with no recorded autotune apply")
	}
}

// TestUnsearchableDetectorsFlagsAIAndML reproduces the AT-12 finding:
// backtest.DefaultGrid's detector sets cover every registered detector except
// "ai" and "ml", so for a symbol configured to use either, every candidate the
// search generates omits it — the baseline is compared against a different
// detector family, and applying any proposal silently strips it (overrideFields
// always writes f.Detectors).
func TestUnsearchableDetectorsFlagsAIAndML(t *testing.T) {
	base := config.Default().Strategy

	base.Detectors = []string{"ema_cross", "rsi", "macd"}
	if got := unsearchableDetectors(base); len(got) != 0 {
		t.Errorf("a fully searchable config reported %v, want none", got)
	}

	base.Detectors = []string{"ema_cross", "ai"}
	base.DetectorsSell = []string{"ml"}

	got := unsearchableDetectors(base)
	if !slices.Equal(got, []string{"detectors:ai", "detectors:ml"}) {
		t.Errorf("unsearchableDetectors = %v, want [detectors:ai detectors:ml] "+
			"(sell-side detectors count too, sorted for a stable UI)", got)
	}

	// Aliases must canonicalise, or a config written as `detectors: [llm]`
	// would be reported as an unknown detector rather than as "ai" — and a
	// config using `ema`/`bb` for perfectly searchable detectors would be
	// falsely flagged, blocking autotune for no reason.
	base.Detectors = []string{"ema", "bb", "llm"}
	base.DetectorsSell = nil

	if got := unsearchableDetectors(base); !slices.Equal(got, []string{"detectors:ai"}) {
		t.Errorf("unsearchableDetectors with aliases = %v, want just [detectors:ai] — "+
			"\"ema\"/\"bb\" are aliases for searchable detectors", got)
	}
}

// TestRunSymbolOptimizeRefusesAnUnsearchableConfig is the end-to-end half:
// such a symbol must be refused BEFORE any backtest runs, both because a
// proposal would mangle the config and because running the baseline at all
// makes an "ai" detector read (and schedule a refresh of) the live per-symbol
// verdict cache — see unsearchableDetectors.
func TestRunSymbolOptimizeRefusesAnUnsearchableConfig(t *testing.T) {
	eng, rm, pf := newTestEngine(t, nil)

	strat := config.Default().Strategy
	strat.Detectors = []string{"ema_cross", "rsi", "ai"}
	eng.SetStrategyConfig(strat)

	data := backtest.Generate([]string{"AAPL"}, 400)
	result := RunSymbolOptimize(context.Background(), eng, rm, pf, "AAPL", data["AAPL"],
		DefaultMinTrades, DefaultObjective, DefaultMinReturnPct)

	if result.Improved {
		t.Error("Improved = true for a config the search space cannot represent")
	}

	if !slices.Contains(result.NotOverridable, "detectors:ai") {
		t.Errorf("NotOverridable = %v, want it to name detectors:ai", result.NotOverridable)
	}

	// Refused before backtesting: no baseline was run, so nothing consulted
	// the AI advisor.
	if result.WhatIf.NumTrades != 0 || len(result.Proposed) != 0 {
		t.Errorf("a refused symbol still produced a backtest/proposal: %+v", result)
	}

	if len(result.Reasoning) == 0 {
		t.Error("a refusal must explain itself; Reasoning is empty")
	}
}

// TestSearchNeverWinsOnAnAxisItCannotApply reproduces the AT-11 fix. AT-1
// pinned CapitalFrac so the search could not win on position size; the three
// exit levels were left free, and runTrial draws them from the grid on EVERY
// candidate — so a winner routinely differed from base on stop_loss_pct /
// trailing_stop_pct / take_profit_pct, and AT-5's gate then threw the whole
// proposal away. Measured before this fix, on config.Default() over eight
// synthetic symbols: five of eight blocked, four of them on exit levels alone.
//
// The invariant that fixes it: with a healthy config, a completed search must
// leave NotOverridable EMPTY — every axis it explored is one a
// strategy_overrides entry can express. Anything else means the search is
// spending its budget on proposals it cannot make.
func TestSearchNeverWinsOnAnAxisItCannotApply(t *testing.T) {
	cfg := config.Default()

	prices := engine.NewPriceCache()
	feed := market.NewMock(time.Hour)

	t.Cleanup(func() { _ = feed.Close() })

	rm := risk.New(cfg.Limits, cfg.Costs)
	pf := portfolio.New()
	pf.SetCash(10000)

	eng := engine.New(cfg.Engine, cfg.Exits, cfg.Safety, cfg.Strategy, feed,
		broker.NewSim(10000, prices.Get, cfg.Costs.Commission), rm, pf, prices, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = eng.Close() })

	data := backtest.Generate([]string{"AAPL"}, 900)

	result := RunSymbolOptimize(context.Background(), eng, rm, pf, "AAPL", data["AAPL"],
		DefaultMinTrades, DefaultObjective, DefaultMinReturnPct)

	if len(result.NotOverridable) != 0 {
		t.Errorf("NotOverridable = %v after a search on a healthy default config, want empty — "+
			"every axis the search explores must be one a strategy_overrides entry can express",
			result.NotOverridable)
	}

	// The winning trial's own deployment settings must equal the base config's,
	// which is what makes the numbers shown next to a proposal reproducible.
	if result.Best.Exits.StopLossPct != cfg.Exits.StopLossPct ||
		result.Best.Exits.TrailingStopPct != cfg.Exits.TrailingStopPct ||
		result.Best.Exits.TakeProfitPct != cfg.Exits.TakeProfitPct {
		t.Errorf("winning trial exits = %.2f/%.2f/%.2f, want the base config's %.2f/%.2f/%.2f",
			result.Best.Exits.StopLossPct, result.Best.Exits.TrailingStopPct, result.Best.Exits.TakeProfitPct,
			cfg.Exits.StopLossPct, cfg.Exits.TrailingStopPct, cfg.Exits.TakeProfitPct)
	}
}
