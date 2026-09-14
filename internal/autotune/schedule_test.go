package autotune

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestSelectSymbolsFiltersByAllowAndDenyLists covers the plan's symbol
// selection spec directly: RunOnlyForSymbols (allow-list) restricts to
// exactly that set, ExcludeSymbols removes symbols regardless of
// RunOnlyForSymbols, a symbol named in both ends up excluded, and both
// empty means the full watchlist. Case-insensitive throughout, matching
// every other symbol comparison in this codebase.
func TestSelectSymbolsFiltersByAllowAndDenyLists(t *testing.T) {
	watchlist := []string{"AAPL", "MSFT", "GOOG", "AMZN"}

	if got := selectSymbols(watchlist, nil, nil); !reflect.DeepEqual(got, watchlist) {
		t.Errorf("with no filters = %v, want the full watchlist %v", got, watchlist)
	}

	if got := selectSymbols(watchlist, []string{"msft", " aapl "}, nil); !reflect.DeepEqual(got, []string{"AAPL", "MSFT"}) {
		t.Errorf("RunOnlyForSymbols = %v, want exactly [AAPL MSFT]", got)
	}

	if got := selectSymbols(watchlist, nil, []string{"GOOG"}); !reflect.DeepEqual(got, []string{"AAPL", "MSFT", "AMZN"}) {
		t.Errorf("ExcludeSymbols = %v, want the watchlist minus GOOG", got)
	}

	// A symbol in both lists is excluded — ExcludeSymbols applies after
	// RunOnlyForSymbols, per config.AutotuneConfig's documented precedence.
	got := selectSymbols(watchlist, []string{"AAPL", "MSFT"}, []string{"AAPL"})
	if !reflect.DeepEqual(got, []string{"MSFT"}) {
		t.Errorf("overlapping lists = %v, want [MSFT] (AAPL excluded despite being allow-listed)", got)
	}
}

// symbolFields is a small helper fields value, distinct enough from a
// symbol's base config to be unambiguous in a diff.
func symbolFields() config.SymbolOverrideFields {
	ms := 0.42

	return config.SymbolOverrideFields{
		Detectors: []string{"ema_cross", "rsi"}, Combine: "unanimous", MinStrength: &ms,
	}
}

// TestApplyResultWritesTheFileAndUpdatesTheLiveEngine checks the
// success path end to end: the file gains the new strategy_overrides
// entry, AND the running engine resolves the symbol to the same config
// immediately, without needing a restart to re-read the file it just wrote.
func TestApplyResultWritesTheFileAndUpdatesTheLiveEngine(t *testing.T) {
	eng, _, _ := newTestEngine(t, nil)
	if err := eng.AddSymbols("AAPL"); err != nil {
		t.Fatalf("AddSymbols: %v", err)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("strategy:\n  fast_ma: 12\n  slow_ma: 26\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	fields := symbolFields()
	result := Result{Symbol: "AAPL", Fields: fields}

	applyResult(eng, path, "AAPL", result, discardLog())

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if got := string(body); !strings.Contains(got, "AAPL:") || !strings.Contains(got, "unanimous") {
		t.Errorf("config file = %q, want a written AAPL strategy_overrides entry", got)
	}

	live := eng.StrategyConfigFor("AAPL")
	if live.Combine != "unanimous" || live.MinStrength != 0.42 {
		t.Errorf("live StrategyConfigFor(AAPL) = %+v, want the applied override reflected immediately", live)
	}
}

// TestApplyResultLeavesTheLiveEngineUntouchedOnAWriteFailure is the
// package's core safety property: if PatchSymbolOverride fails (the file
// disappeared, is unwritable, whatever), the live engine must NOT be
// updated to match a config that was never actually persisted — otherwise
// a restart would silently revert to the old config while the running
// process kept trading under the new one until then.
func TestApplyResultLeavesTheLiveEngineUntouchedOnAWriteFailure(t *testing.T) {
	eng, _, _ := newTestEngine(t, nil)
	if err := eng.AddSymbols("AAPL"); err != nil {
		t.Fatalf("AddSymbols: %v", err)
	}

	before := eng.StrategyConfigFor("AAPL")

	// A path inside a directory that doesn't exist: PatchSymbolOverride's
	// initial os.ReadFile fails immediately.
	badPath := filepath.Join(t.TempDir(), "does-not-exist", "config.yaml")

	applyResult(eng, badPath, "AAPL", Result{Symbol: "AAPL", Fields: symbolFields()}, discardLog())

	after := eng.StrategyConfigFor("AAPL")
	if !reflect.DeepEqual(before, after) {
		t.Errorf("live config changed after a failed write: before=%+v after=%+v", before, after)
	}

	if slices.Contains(eng.OverriddenSymbols(), "AAPL") {
		t.Error("engine reports an override installed despite the write failing")
	}
}

// TestRunPassSkipsATickWhenASearchIsAlreadyRunning mirrors the manual
// button's own busy behavior: a scheduled pass must never overlap another
// search — manual or scheduled — sharing the same semaphore.
func TestRunPassSkipsATickWhenASearchIsAlreadyRunning(t *testing.T) {
	eng, rm, pf := newTestEngine(t, nil)
	if err := eng.AddSymbols("AAPL"); err != nil {
		t.Fatalf("AddSymbols: %v", err)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("strategy:\n  fast_ma: 12\n  slow_ma: 26\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if !TryAcquire() {
		t.Fatal("TryAcquire failed on an otherwise-idle semaphore")
	}
	defer Release()

	runPass(context.Background(), eng, rm, pf, config.AutotuneConfig{Enabled: true}, path, discardLog())

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(body), "strategy_overrides") {
		t.Errorf("config file was written to despite the semaphore already being held: %s", body)
	}
}

// TestRunPassSkipsASymbolUnderTheBarsFloor checks a symbol with too little
// cached history (fewer than MinBars) is skipped for that run rather than
// fed to a search that can't produce a meaningful result from it.
func TestRunPassSkipsASymbolUnderTheBarsFloor(t *testing.T) {
	eng, rm, pf := newTestEngine(t, nil)
	if err := eng.AddSymbols("AAPL"); err != nil {
		t.Fatalf("AddSymbols: %v", err)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("strategy:\n  fast_ma: 12\n  slow_ma: 26\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// AddSymbols' warmup backfills far more than MinBars of synthetic
	// history from the mock feed, so the floor is exercised the way the
	// real code path hits it: Bars smaller than MinBars tail-slices the
	// cached history down below the floor, not "a symbol with literally
	// nothing cached" (which AddSymbols' warmup makes hard to produce at
	// all against this feed).
	runPass(context.Background(), eng, rm, pf,
		config.AutotuneConfig{Enabled: true, MinTrades: 1, Bars: MinBars - 1}, path, discardLog())

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(body), "strategy_overrides") {
		t.Errorf("config file was written to for a symbol with no cached history: %s", body)
	}
}

// TestScheduleIsANoOpWhenDisabled checks the documented gate: Schedule
// returns immediately (no ticker, no pass, ever) when cfg.Enabled is
// false, so callers can start the goroutine unconditionally and let this
// single field control it.
func TestScheduleIsANoOpWhenDisabled(t *testing.T) {
	eng, rm, pf := newTestEngine(t, nil)

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("strategy:\n  fast_ma: 12\n  slow_ma: 26\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})

	go func() {
		Schedule(context.Background(), eng, rm, pf, config.AutotuneConfig{Enabled: false}, path, discardLog())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Schedule with Enabled=false did not return promptly")
	}
}

// TestScheduleTicksAndRunsAPass is a smoke test of the loop mechanics: a
// very short interval and a symbol under the bars floor (so the pass
// completes instantly without running a real search), then a context
// cancellation stops it — proving the ticker fires runPass and the
// goroutine exits cleanly on ctx.Done rather than leaking.
func TestScheduleTicksAndRunsAPass(t *testing.T) {
	eng, rm, pf := newTestEngine(t, nil)
	if err := eng.AddSymbols("AAPL"); err != nil {
		t.Fatalf("AddSymbols: %v", err)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("strategy:\n  fast_ma: 12\n  slow_ma: 26\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Bars under MinBars (see TestRunPassSkipsASymbolUnderTheBarsFloor)
	// keeps each tick's pass a fast skip rather than a real 20s-bounded
	// search, so the ticker can fire and the loop can observe ctx.Done
	// well inside this test's own timeout.
	cfg := config.AutotuneConfig{Enabled: true, Interval: 20 * time.Millisecond, Bars: MinBars - 1}

	// Schedule only reads its own cfg param to decide whether to start and
	// at what interval; every tick re-reads eng.AutotuneConfig() instead
	// (see Schedule's doc comment) — so the engine needs the same value
	// installed, exactly as cmd/bot/main.go does before starting the
	// goroutine.
	eng.SetAutotuneConfig(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	done := make(chan struct{})

	go func() {
		Schedule(ctx, eng, rm, pf, cfg, path, discardLog())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Schedule did not return after its context was cancelled")
	}
}

// TestScheduleSkipsATickWhenDisabledLive checks the live-reconfigurability
// Engine.SetAutotuneConfig exists for: turning the job off through the API
// (PUT /api/settings/autotune with enabled:false) must stop it from
// starting a new pass on the very next tick, without needing a restart —
// even though the goroutine itself, and its ticker's interval, were fixed
// when Schedule started.
func TestScheduleSkipsATickWhenDisabledLive(t *testing.T) {
	eng, rm, pf := newTestEngine(t, nil)
	if err := eng.AddSymbols("AAPL"); err != nil {
		t.Fatalf("AddSymbols: %v", err)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("strategy:\n  fast_ma: 12\n  slow_ma: 26\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	startCfg := config.AutotuneConfig{Enabled: true, Interval: 20 * time.Millisecond, Bars: MinBars - 1}
	eng.SetAutotuneConfig(startCfg)

	// Immediately turn it back off live, before Schedule's first tick could
	// possibly fire (the ticker's own interval is 20ms).
	eng.SetAutotuneConfig(config.AutotuneConfig{Enabled: false})

	var logBuf bytes.Buffer

	log := slog.New(slog.NewTextHandler(&logBuf, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	done := make(chan struct{})

	go func() {
		Schedule(ctx, eng, rm, pf, startCfg, path, log)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Schedule did not return after its context was cancelled")
	}

	// A precise signal that the disabled-live check itself fired, not just
	// a side effect (like the bars floor below) that would also leave the
	// file untouched for an unrelated reason.
	if !strings.Contains(logBuf.String(), "disabled live") {
		t.Errorf("log = %q, want a \"disabled live, skipping this tick\" line", logBuf.String())
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(body), "strategy_overrides") {
		t.Errorf("config file was written to despite autotune being disabled live: %s", body)
	}
}
