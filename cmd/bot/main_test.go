package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/errlog"
	"github.com/Kellerman81/go_finance_bot/internal/market"
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

// CM2-2: with feed: binance and a broker with no history of its own, the bot
// wired Yahoo alone — which has no BTCUSDT — as the source the venue-price
// check and the screener read. Every crypto entry was refused "venue price
// unavailable", so the documented binance + sim mode never bought. The feed
// itself must answer first.
func TestBinanceFeedServesItsOwnHistory(t *testing.T) {
	feed := market.NewMock(time.Hour)
	defer feed.Close()

	h := defaultHistory("binance", feed)

	got, err := h.Candles(context.Background(), "BTCUSDT", market.Res1Min,
		time.Now().Add(-time.Hour), time.Now())
	if err != nil || len(got) == 0 {
		t.Fatalf("no history for BTCUSDT from the binance feed: %d bars, err %v", len(got), err)
	}

	if _, isYahoo := defaultHistory("finnhub", feed).(*market.YahooHistory); !isYahoo {
		t.Error("a non-binance feed no longer gets Yahoo as its history source")
	}
}

// A smoke test of run's shutdown path, which had no test at all: a SIGINT
// stops the engine through its shutdown arm, the protection-state save
// succeeds, and run returns. It does NOT pin CM3-1's ordering on its own —
// that race is narrow on an idle engine and passes either way;
// TestWaitBoundedJoinsTheWorkers pins the join run now performs.
func TestRunShutsDownCleanlyOnSIGINT(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	yaml := "server:\n  addr: \"127.0.0.1:0\"\nbroker: \"sim\"\n" +
		"storage:\n  driver: \"sqlite\"\n  path: \"" + filepath.Join(dir, "bot.db") + "\"\n" +
		"protections:\n  enabled: true\n  max_drawdown_pct: 0.5\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	origArgs, origStdout := os.Args, os.Stdout
	t.Cleanup(func() { os.Args, os.Stdout = origArgs, origStdout })

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	os.Stdout = w
	os.Args = []string{"bot", "-config", path}

	// run registers its flags on the global set; another test calls it too.
	origFlags := flag.CommandLine
	flag.CommandLine = flag.NewFlagSet("bot", flag.ContinueOnError)
	t.Cleanup(func() { flag.CommandLine = origFlags })

	var (
		mu  sync.Mutex
		out strings.Builder
	)

	go func() {
		buf := make([]byte, 4096)

		for {
			n, err := r.Read(buf)

			mu.Lock()
			out.Write(buf[:n])
			mu.Unlock()

			if err != nil {
				return
			}
		}
	}()

	logged := func(s string) bool {
		mu.Lock()
		defer mu.Unlock()

		return strings.Contains(out.String(), s)
	}

	done := make(chan int, 1)

	go func() { done <- run() }()

	deadline := time.Now().Add(10 * time.Second)
	for !logged("engine started") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(40 * time.Second):
		t.Fatal("run did not return after SIGINT")
	}

	_ = w.Close()

	time.Sleep(50 * time.Millisecond)

	if logged("database is closed") {
		t.Error("the engine's shutdown save ran against a closed store")
	}

	if !logged("engine stopping") {
		t.Error("run returned before the engine reached its shutdown path")
	}
}

// CM3-1: run started the engine and autotune goroutines and never joined
// them, so SIGTERM ran its defers — closing the store — while the engine was
// still on its way to the shutdown save of the protection state, the only
// save of a peak reached in the last five minutes. run now waits for them,
// bounded, before returning.
func TestWaitBoundedJoinsTheWorkers(t *testing.T) {
	var (
		wg   sync.WaitGroup
		done atomic.Bool
	)

	wg.Go(func() {
		time.Sleep(100 * time.Millisecond) // the engine finishing its final save
		done.Store(true)
	})

	waitBounded(&wg, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if !done.Load() {
		t.Error("waitBounded returned before the worker finished")
	}
}

// The bound: a worker stuck in a hung broker call must not hold a container
// stop forever.
func TestWaitBoundedGivesUpAfterTheGrace(t *testing.T) {
	var wg sync.WaitGroup

	block := make(chan struct{})
	defer close(block)

	wg.Go(func() { <-block })

	start := time.Now()
	waitBounded(&wg, 100*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if time.Since(start) > time.Second {
		t.Error("waitBounded did not give up after its grace")
	}
}
