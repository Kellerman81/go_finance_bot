package main

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/errlog"
)

// TestRunSupervisedRecoversAPanic reproduces the "an unrecovered panic in
// the engine goroutine crashes the whole process" finding: nothing in this
// codebase called recover() anywhere, so a panic in Engine.Run took the
// entire process — HTTP API included — down with it, leaving an operator
// unable to even check open positions afterward. runSupervised must catch
// the panic (proven simply by this test completing instead of the test
// binary crashing), log it, record it to errlog, and invoke onPanic.
func TestRunSupervisedRecoversAPanic(t *testing.T) {
	errlog.Clear()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	var onPanicCalled bool

	runSupervised(log, "test-work", func() { onPanicCalled = true }, func() {
		panic("simulated engine failure")
	})

	// Reaching this line at all proves the panic did not escape runSupervised.
	if !onPanicCalled {
		t.Error("onPanic was not called after the panic was recovered")
	}

	found := false

	for _, e := range errlog.Recent() {
		if e.Source == "test-work" {
			found = true
		}
	}

	if !found {
		t.Error("the panic was not recorded to errlog")
	}
}

// Ordinary (non-panicking) work must run to completion normally, and
// onPanic must not fire.
func TestRunSupervisedRunsNormallyWithNoPanic(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	var (
		ran           bool
		onPanicCalled bool
	)

	runSupervised(log, "test-work", func() { onPanicCalled = true }, func() { ran = true })

	if !ran {
		t.Error("work did not run")
	}

	if onPanicCalled {
		t.Error("onPanic fired despite no panic")
	}
}

// onPanic may be nil — a caller with nothing extra to do on panic must not
// crash runSupervised itself.
func TestRunSupervisedToleratesANilOnPanic(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	runSupervised(log, "test-work", nil, func() { panic("boom") })
}

// TestAiInertWarningFlagsAConfiguredButInertDetector reproduces the "no
// warning when ai/ml is configured but inert" finding: cmd/backtest already
// warns when the strategy's detector set references "ai" but the advisor
// isn't actually enabled or failed to configure (see backtest.ConfigureAI)
// — cmd/bot did not, so a strategy validated in a backtest with the advisor
// enabled could deploy live strictly weaker (the detector always voting
// HOLD) with nothing pointing at why.
func TestAiInertWarningFlagsAConfiguredButInertDetector(t *testing.T) {
	usesAI := config.Config{Strategy: config.StrategyConfig{Detectors: []string{"rsi", "ai"}}}

	if got := aiInertWarning(usesAI, nil); got == "" {
		t.Error(`"ai" in the detector set with ai.enabled false: want a warning, got none`)
	}

	if got := aiInertWarning(usesAI, errors.New("connection refused")); got == "" {
		t.Error(`"ai" in the detector set with a configure error: want a warning, got none`)
	}

	enabled := usesAI
	enabled.AI.Enabled = true

	if got := aiInertWarning(enabled, nil); got != "" {
		t.Errorf(`"ai" enabled and configured cleanly: want no warning, got %q`, got)
	}

	noAI := config.Config{Strategy: config.StrategyConfig{Detectors: []string{"rsi"}}}
	if got := aiInertWarning(noAI, nil); got != "" {
		t.Errorf(`"ai" not in the detector set: want no warning, got %q`, got)
	}
}

// The same shape of finding, for the "ml" detector.
func TestMlInertWarningFlagsAConfiguredButInertDetector(t *testing.T) {
	usesML := config.Config{Strategy: config.StrategyConfig{Detectors: []string{"rsi", "ml"}}}

	if got := mlInertWarning(usesML); got == "" {
		t.Error(`"ml" in the detector set with ml.enabled false: want a warning, got none`)
	}

	enabled := usesML
	enabled.ML.Enabled = true

	if got := mlInertWarning(enabled); got != "" {
		t.Errorf(`"ml" enabled: want no warning, got %q`, got)
	}

	noML := config.Config{Strategy: config.StrategyConfig{Detectors: []string{"rsi"}}}
	if got := mlInertWarning(noML); got != "" {
		t.Errorf(`"ml" not in the detector set: want no warning, got %q`, got)
	}
}

// TestRunReturnsAnExitCodeInsteadOfCallingOSExit reproduces the "several
// os.Exit(1) startup paths skip deferred cleanup" finding at its root:
// main used to call os.Exit(1) directly at several points inside its own
// body, including after resources needing cleanup (the market feed) were
// already live — os.Exit never runs deferred functions, so every one of
// those cleanups was silently skipped. main's startup logic now lives in
// run() int, which returns an exit code instead of calling os.Exit itself;
// main is just `os.Exit(run())`. If run() still called os.Exit anywhere on
// this path, THIS TEST PROCESS would terminate right there instead of the
// assertion below ever running — the strongest possible regression check
// for this specific class of bug.
func TestRunReturnsAnExitCodeInsteadOfCallingOSExit(t *testing.T) {
	origArgs := os.Args
	t.Cleanup(func() { os.Args = origArgs })

	// Deliberately unparsable YAML: config.Load fails at the very first
	// step, before any resource (feed, broker, storage, engine) is
	// constructed — the cleanest single-branch way to exercise run()'s
	// early-return path without standing up a real HTTP server.
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("engine: [this is not valid yaml\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	os.Args = []string{"bot", "-config", path}

	if code := run(); code != 1 {
		t.Errorf("run() = %d, want 1 for an unparsable config file", code)
	}
}

func TestUsesDetectorChecksBothSides(t *testing.T) {
	s := config.StrategyConfig{
		Detectors:     []string{"rsi", "macd"},
		DetectorsSell: []string{"ai"},
	}

	if !usesDetector(s, "ai", "llm", "ollama") {
		t.Error("usesDetector missed a match in DetectorsSell")
	}

	if usesDetector(s, "ml", "model", "classifier") {
		t.Error("usesDetector false-matched a name that is not present")
	}
}
