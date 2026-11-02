package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/backtest"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/goccy/go-yaml"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns
// everything it wrote. printBest writes directly to os.Stdout (it is meant
// to be pasted from a terminal), so this is the only way to assert on its
// output without changing that.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	orig := os.Stdout
	os.Stdout = w

	fn()

	os.Stdout = orig

	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read: %v", err)
	}

	return buf.String()
}

// TestPrintBestIncludesEveryTunedDetector reproduces the "cmd/optimize: the
// paste-ready BEST CONFIG output omits ADX/Stochastic/Supertrend/Donchian
// parameters" finding: addIntSC in internal/backtest/optimize.go genuinely
// searches adx_period, stoch_k_period, supertrend_period and
// donchian_period (see optimize.go:725-742) and uses them to pick the
// winning trial, but printBest never printed them — so pasting the "BEST
// CONFIG" block both failed to reproduce the reported result (whichever of
// these detectors were in play) and deleted whatever values the operator
// already had configured for them, since a config.yaml key it never
// mentions keeps its old value from the file rather than being left alone
// (this block is meant to be pasted over the whole strategy: section).
func TestPrintBestIncludesEveryTunedDetector(t *testing.T) {
	trial := backtest.Trial{
		Detectors: []string{"adx", "stochastic", "supertrend", "donchian"},
		Combine:   "all",
		Params: config.StrategyConfig{
			ADXPeriod: 20, ADXThreshold: 30,
			StochKPeriod: 21, StochDPeriod: 5, StochOverbought: 85, StochOversold: 15,
			SupertrendPeriod: 14, SupertrendMult: 2.5,
			DonchianPeriod: 55,
		},
	}

	out := captureStdout(t, func() { printBest(trial, 10000, 0) })

	for _, want := range []string{
		"adx_period: 20",
		"adx_threshold: 30",
		"stoch_k_period: 21",
		"stoch_d_period: 5",
		"stoch_overbought: 85",
		"stoch_oversold: 15",
		"supertrend_period: 14",
		"supertrend_mult: 2.5",
		"donchian_period: 55",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("BEST CONFIG output is missing %q:\n%s", want, out)
		}
	}
}

// TestConfirmCountWarningStatesTheUnitMismatch reproduces the
// "confirm_buy/confirm_sell count bars in backtest, evaluation ticks live"
// finding: the search tunes a BAR count and emits it into paste-ready
// config, but the live engine counts EVALUATION TICKS against the same
// field — three days of daily-bar confirmation can become three 30-second
// ticks once deployed. printBest's YAML output already carried this as a
// comment; it is invisible in -json mode, which returns before ever calling
// printBest (see main's -json branch). confirmCountWarning is now a shared
// constant referenced by both, so the JSON path cannot silently drift out of
// sync with (or omit) what the human-readable path says.
// TestWriteCheckpointRoundTrips reproduces the "cmd/optimize: no
// cancellation, no incremental result persistence" finding's second half: a
// search's ranked trials + best config must be recoverable from disk if the
// process is interrupted or the human-readable print itself fails (a broken
// stdout pipe) — writeCheckpoint is what makes that possible, written
// before any printing is attempted.
func TestWriteCheckpointRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.json")

	out := searchOutput{
		Trials:      []backtest.Trial{{Combine: "majority", MinStrength: 0.2}},
		Best:        backtest.Trial{Combine: "majority", MinStrength: 0.2},
		Refinements: []backtest.Refinement{{Axis: "sell_side", From: "a", To: "b", Improved: 0.5}},
		Warnings:    []string{confirmCountWarning},
	}

	if err := writeCheckpoint(path, out); err != nil {
		t.Fatalf("writeCheckpoint: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read checkpoint: %v", err)
	}

	var got searchOutput
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal checkpoint: %v", err)
	}

	if len(got.Trials) != 1 || got.Trials[0].Combine != "majority" {
		t.Errorf("checkpoint trials = %+v, want the original round-tripped", got.Trials)
	}

	if got.Best.MinStrength != 0.2 {
		t.Errorf("checkpoint best = %+v, want MinStrength 0.2", got.Best)
	}

	if len(got.Refinements) != 1 || got.Refinements[0].Axis != "sell_side" {
		t.Errorf("checkpoint refinements = %+v, want the original round-tripped", got.Refinements)
	}

	if len(got.Warnings) != 1 || got.Warnings[0] != confirmCountWarning {
		t.Errorf("checkpoint warnings = %v, want [%q]", got.Warnings, confirmCountWarning)
	}
}

// TestWriteCheckpointReportsAnUnwritablePath confirms a bad path fails
// cleanly (the caller logs a warning and continues) rather than panicking —
// the checkpoint is a safety net, not something that should itself crash a
// run that was otherwise about to succeed.
func TestWriteCheckpointReportsAnUnwritablePath(t *testing.T) {
	// A directory that does not exist, so os.Create must fail.
	path := filepath.Join(t.TempDir(), "no-such-dir", "checkpoint.json")

	if err := writeCheckpoint(path, searchOutput{}); err == nil {
		t.Error("writeCheckpoint into a non-existent directory succeeded, want an error")
	}
}

func TestConfirmCountWarningStatesTheUnitMismatch(t *testing.T) {
	if !strings.Contains(confirmCountWarning, "BARS") ||
		!strings.Contains(confirmCountWarning, "EVALUATIONS") {
		t.Errorf("confirmCountWarning does not state the bars-vs-evaluations unit mismatch: %q",
			confirmCountWarning)
	}

	out := captureStdout(t, func() { printBest(backtest.Trial{}, 10000, 0) })

	if !strings.Contains(out, confirmCountWarning) {
		t.Errorf("BEST CONFIG output does not include the confirm-count warning:\n%s", out)
	}
}

// CM2-5: the paste-ready block emitted engine.order_size_usd, a key the sizing
// rework removed. config.Load drops unknown keys silently, so pasting it did
// nothing and the reported deployment was never reproduced. Every key the
// engine block prints must be one the config actually has — decoded strictly
// here so an unknown key fails rather than vanishing.
func TestPrintedEngineBlockLoadsStrictly(t *testing.T) {
	trial := backtest.Trial{OrderSize: 2500, CapitalFrac: 0.25, ConfirmBuy: 2, ConfirmSell: 1}

	out := captureStdout(t, func() { printBestEngine(trial, 10000, 0) })

	var doc struct {
		Engine config.EngineConfig `yaml:"engine"`
	}

	if err := yaml.UnmarshalWithOptions([]byte(out), &doc, yaml.Strict()); err != nil {
		t.Fatalf("the printed engine block does not load strictly: %v\n%s", err, out)
	}

	if doc.Engine.MaxRiskSizeUSD != 2500 {
		t.Errorf("max_risk_size_usd = %v, want the trial's 2500 per-trade cap", doc.Engine.MaxRiskSizeUSD)
	}
}

// CM2-7: a flag the chosen mode ignores was dropped without a word —
// "-walkforward 5 -random 2000" ran the default grid per fold and the banner
// never mentioned random. Every such combination must be reported.
func TestConflictingModeFlagsAreReported(t *testing.T) {
	cases := []struct {
		set  []string
		want int
	}{
		{[]string{"walkforward", "random"}, 1},
		{[]string{"sensitivity", "walkforward", "json"}, 2},
		{[]string{"generations"}, 1},    // without -random
		{[]string{"random", "seed"}, 0}, // a valid search
		{[]string{"walkforward", "json"}, 0},
	}

	for _, c := range cases {
		set := map[string]bool{}
		for _, n := range c.set {
			set[n] = true
		}

		if got := flagConflicts(set); len(got) != c.want {
			t.Errorf("flags %v: %d conflicts %v, want %d", c.set, len(got), got, c.want)
		}
	}
}

// CM2-6: after Ctrl+C the search returned its partial trials and Refine —
// which takes no context — swept them in full before anything was written,
// so the interrupt appeared to do nothing for the length of the sweep.
func TestRefineIsSkippedOnceInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ran := false
	best := backtest.Trial{Score: 1}

	got, changes := refineUnlessCancelled(ctx, func() (backtest.Trial, []backtest.Refinement) {
		ran = true

		return backtest.Trial{Score: 2}, nil
	}, best)

	if ran || got.Score != 1 || changes != nil {
		t.Errorf("refine ran after an interrupt (ran=%v, score %v)", ran, got.Score)
	}
}

// CM3-3: every trial ran under min(OrderSize, the config's own
// max_risk_size_usd), and the paste-ready block printed OrderSize alone — a
// trial capped at 500 by the base config came out as max_risk_size_usd 12500,
// 25 times the per-trade size whose results were reported.
func TestThePrintedCapIsTheOneTheTrialRanUnder(t *testing.T) {
	trial := backtest.Trial{OrderSize: 12500, CapitalFrac: 0.25}

	out := captureStdout(t, func() { printBestEngine(trial, 50000, 500) })

	var doc struct {
		Engine config.EngineConfig `yaml:"engine"`
	}

	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("parse: %v\n%s", err, out)
	}

	if doc.Engine.MaxRiskSizeUSD != 500 {
		t.Errorf("max_risk_size_usd = %v, want the 500 every trial was capped at", doc.Engine.MaxRiskSizeUSD)
	}
}

// CM3-5: flagConflicts asked "was the flag typed", not "is the mode on": a
// mode switched off explicitly still claimed the other flags, and -random 0
// counted as a random search so -seed was ignored silently.
func TestAModeSwitchedOffIsNotAMode(t *testing.T) {
	cases := []struct {
		args []string
		want int
	}{
		{[]string{"-walkforward", "0", "-random", "500"}, 0},
		{[]string{"-sensitivity=false", "-walkforward", "4"}, 0},
		{[]string{"-random", "0", "-seed", "7"}, 1},
	}

	for _, c := range cases {
		fs := flag.NewFlagSet("optimize", flag.ContinueOnError)
		fs.Bool("sensitivity", false, "")
		fs.Int("walkforward", 0, "")
		fs.Int("random", 0, "")
		fs.Int64("seed", 0, "")

		if err := fs.Parse(c.args); err != nil {
			t.Fatal(err)
		}

		if got := flagConflicts(activeFlags(fs)); len(got) != c.want {
			t.Errorf("%v: conflicts %v, want %d", c.args, got, c.want)
		}
	}
}
