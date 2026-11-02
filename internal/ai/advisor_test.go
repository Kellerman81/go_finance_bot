package ai

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// testAdvisor builds an advisor with the model call replaced by fn, so the
// caching, staleness and fail-safe behaviour can be tested without a model.
func testAdvisor(
	t *testing.T,
	cfg config.AIConfig,
	fn func(context.Context, Features) (Verdict, error),
) *Advisor {
	t.Helper()

	cfg.Enabled = true

	if cfg.Model == "" {
		cfg.Model = "test-model"
	}

	if cfg.BaseURL == "" {
		cfg.BaseURL = "http://localhost:11434"
	}

	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	a.call = fn

	return a
}

// feats returns a snapshot with the given 5-bar return, which is enough to make
// two snapshots differ in fingerprint.
func feats(symbol string, ret5 float64) Features {
	return Features{Symbol: symbol, Price: 100, Bars: 300, Ret5: ret5}
}

// waitFor polls cond until it holds or the deadline passes, so tests can await
// a background refresh without sleeping for a fixed guess.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", what)
}

// TestVerdictIsAsyncAndCached checks the core contract: the first call never
// blocks (it returns "no verdict" and refreshes in the background), and once the
// answer lands it is served from cache without further model calls.
func TestVerdictIsAsyncAndCached(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	a := testAdvisor(t, config.AIConfig{RefreshInterval: time.Hour},
		func(context.Context, Features) (Verdict, error) {
			calls.Add(1)

			return Verdict{Action: Buy, Confidence: 0.8, Reason: "up", At: time.Now()}, nil
		})

	if _, ok := a.Verdict("AAPL", feats("AAPL", 1)); ok {
		t.Fatal("first call should have no verdict yet (it must not block on the model)")
	}

	waitFor(t, "background refresh", func() bool {
		_, ok := a.Verdict("AAPL", feats("AAPL", 1))

		return ok
	})

	v, ok := a.Verdict("AAPL", feats("AAPL", 1))
	if !ok || v.Action != Buy {
		t.Fatalf("verdict = %+v, ok = %v; want BUY", v, ok)
	}

	// Within the refresh interval and with an unchanged snapshot, no more calls.
	for range 5 {
		a.Verdict("AAPL", feats("AAPL", 1))
	}

	if n := calls.Load(); n != 1 {
		t.Errorf("model calls = %d, want 1 (cached within refresh_interval)", n)
	}
}

// TestVerdictBlockingReturnsImmediately checks blocking mode: the caller waits
// for the model and gets the verdict on the very first call.
func TestVerdictBlockingReturnsImmediately(t *testing.T) {
	t.Parallel()

	a := testAdvisor(t, config.AIConfig{Blocking: true, RefreshInterval: time.Hour},
		func(context.Context, Features) (Verdict, error) {
			return Verdict{Action: Sell, Confidence: 0.6, At: time.Now()}, nil
		})

	v, ok := a.Verdict("MSFT", feats("MSFT", -1))
	if !ok || v.Action != Sell {
		t.Fatalf("verdict = %+v, ok = %v; want SELL on the first blocking call", v, ok)
	}
}

// TestVerdictFailsSafe checks that a model error yields no verdict — the
// detector must vote HOLD, never guess a direction.
func TestVerdictFailsSafe(t *testing.T) {
	t.Parallel()

	a := testAdvisor(t, config.AIConfig{Blocking: true, RefreshInterval: time.Hour},
		func(context.Context, Features) (Verdict, error) {
			return Verdict{}, errors.New("connection refused")
		})

	if v, ok := a.Verdict("NVDA", feats("NVDA", 2)); ok {
		t.Fatalf("verdict = %+v, ok = true; a failed call must yield no verdict", v)
	}

	if s := a.Stats(); s.Errors != 1 || s.LastError == "" {
		t.Errorf("stats = %+v, want one recorded error", s)
	}
}

// TestVerdictErrorBacksOff checks a failing endpoint is retried at the refresh
// interval rather than on every evaluation tick.
func TestVerdictErrorBacksOff(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	a := testAdvisor(t, config.AIConfig{Blocking: true, RefreshInterval: time.Hour},
		func(context.Context, Features) (Verdict, error) {
			calls.Add(1)

			return Verdict{}, errors.New("down")
		})

	for range 5 {
		a.Verdict("AMD", feats("AMD", 1))
	}

	if n := calls.Load(); n != 1 {
		t.Errorf("model calls = %d, want 1 (errors back off by refresh_interval)", n)
	}
}

// TestVerdictExpires checks a verdict older than max_age is not served: better
// to abstain than to trade on what the model thought an hour ago.
func TestVerdictExpires(t *testing.T) {
	t.Parallel()

	// A clock the test can advance. Installed before any call so the advisor
	// only ever reads it, never races with the test writing it.
	var offset atomic.Int64

	start := time.Now()
	clock := func() time.Time { return start.Add(time.Duration(offset.Load())) }

	a := testAdvisor(t, config.AIConfig{
		Blocking: true, RefreshInterval: 30 * time.Minute, MaxAge: 30 * time.Minute,
	}, func(context.Context, Features) (Verdict, error) {
		return Verdict{Action: Buy, Confidence: 0.9, At: clock()}, nil
	})
	a.now = clock
	// A refresh interval longer than max_age is refused at load (AI3-4); set
	// on the advisor directly it isolates the staleness rule, which still
	// matters on its own — a refresh can fail, and the old verdict must then
	// expire rather than vote forever.
	a.cfg.RefreshInterval = time.Hour

	if _, ok := a.Verdict("TSLA", feats("TSLA", 1)); !ok {
		t.Fatal("want a fresh verdict")
	}

	// Jump past max_age but not past refresh_interval, so no refresh is due and
	// only staleness can decide the outcome.
	offset.Store(int64(45 * time.Minute))

	if v, ok := a.Verdict("TSLA", feats("TSLA", 1)); ok {
		t.Errorf("verdict = %+v, ok = true; a verdict past max_age must not be served", v)
	}
}

// TestVerdictDemotesLowConfidence checks the model's own uncertainty gate: an
// action below min_confidence becomes HOLD.
func TestVerdictDemotesLowConfidence(t *testing.T) {
	t.Parallel()

	a := testAdvisor(t, config.AIConfig{
		Blocking: true, RefreshInterval: time.Hour, MinConfidence: 0.5,
	}, nil)
	// Exercise the real ask() path (which applies the gate) over a stub model.
	a.cl = fixedCompleter{answer: `{"action":"BUY","confidence":0.2,"reason":"weak"}`}
	a.call = a.ask

	v, ok := a.Verdict("INTC", feats("INTC", 1))
	if !ok {
		t.Fatal("want a verdict")
	}

	if v.Action != Hold || v.Confidence != 0 {
		t.Errorf("verdict = %+v, want HOLD/0 below min_confidence", v)
	}
}

// TestVerdictConcurrentSymbolsBounded checks that many symbols evaluated at once
// never exceed max_concurrent in-flight model calls — one local GPU must not be
// asked 300 questions simultaneously.
func TestVerdictConcurrentSymbolsBounded(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		inflight int
		peak     int
		done     atomic.Int64
	)

	a := testAdvisor(t, config.AIConfig{RefreshInterval: time.Hour, MaxConcurrent: 2},
		func(context.Context, Features) (Verdict, error) {
			mu.Lock()

			inflight++
			if inflight > peak {
				peak = inflight
			}

			mu.Unlock()

			time.Sleep(2 * time.Millisecond)

			mu.Lock()

			inflight--
			mu.Unlock()

			done.Add(1)

			return Verdict{Action: Hold, At: time.Now()}, nil
		})

	symbols := []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J"}
	for _, s := range symbols {
		a.Verdict(s, feats(s, 1))
	}

	waitFor(t, "all refreshes", func() bool { return done.Load() == int64(len(symbols)) })

	mu.Lock()
	defer mu.Unlock()

	if peak > 2 {
		t.Errorf("peak in-flight calls = %d, want <= max_concurrent (2)", peak)
	}
}

// TestVerdictBlockingRespectsTimeoutWhileWaitingForASlot reproduces the
// "Verdict not bounded by the HTTP timeout in blocking mode" finding: the
// concurrency-slot wait used to be acquired before the timeout context even
// existed, so a Blocking-mode call (run synchronously in the caller's own
// evaluation goroutine, per the package doc's "deliberate, slow backtest of
// the advisor" use case) could stall indefinitely behind other in-flight
// symbols instead of giving up once Timeout elapsed.
//
// One symbol's call occupies the sole concurrency slot and never returns
// until the test releases it (deliberately ignoring its own ctx, to isolate
// this from the call's own timeout enforcement — simulating "a call already
// in flight, still running"). A second symbol's Blocking call then competes
// for that slot and must give up near Timeout, not whenever the first call
// happens to finish.
func TestVerdictBlockingRespectsTimeoutWhileWaitingForASlot(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})

	var once sync.Once

	a := testAdvisor(t, config.AIConfig{
		Blocking: true, RefreshInterval: time.Hour, MaxConcurrent: 1, Timeout: 50 * time.Millisecond,
	}, func(_ context.Context, _ Features) (Verdict, error) {
		once.Do(func() { close(started) })
		<-release // simulates a call still running well past Timeout

		return Verdict{Action: Buy, Confidence: 0.9, At: time.Now()}, nil
	})

	go a.Verdict("HOLDER", feats("HOLDER", 1))
	<-started // the holder now definitely occupies the one concurrency slot

	type result struct {
		ok      bool
		elapsed time.Duration
	}

	done := make(chan result, 1)

	go func() {
		start := time.Now()
		_, ok := a.Verdict("WAITER", feats("WAITER", 2))
		done <- result{ok, time.Since(start)}
	}()

	select {
	case r := <-done:
		close(release)

		if r.ok {
			t.Error("verdict ok=true; a call that never got a free concurrency slot must not succeed")
		}

		if r.elapsed > 500*time.Millisecond {
			t.Errorf("blocking Verdict took %v waiting for a free slot, want it bounded near Timeout (50ms)",
				r.elapsed)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("blocking Verdict never returned — the wait for a free concurrency slot is unbounded")
	}
}

// TestNilAdvisorIsSafe checks the "disabled" state: a nil advisor answers every
// method without panicking, so callers need no special case.
func TestNilAdvisorIsSafe(t *testing.T) {
	t.Parallel()

	var a *Advisor

	if _, ok := a.Verdict("AAPL", feats("AAPL", 1)); ok {
		t.Error("nil advisor must not return a verdict")
	}

	if a.Model() != "" || a.MaxStrength() != 0 {
		t.Error("nil advisor must report no model and no strength")
	}

	if s := a.Stats(); s.Enabled {
		t.Error("nil advisor must report enabled=false")
	}

	a.Forget("AAPL") // must not panic
}

// TestNewDisabledReturnsNil checks a disabled config yields the nil "off"
// advisor rather than an error.
func TestNewDisabledReturnsNil(t *testing.T) {
	t.Parallel()

	a, err := New(config.AIConfig{Enabled: false})
	if err != nil || a != nil {
		t.Errorf("New(disabled) = (%v, %v), want (nil, nil)", a, err)
	}
}

// TestNewRejectsBadConfig checks an enabled-but-incomplete config is refused at
// startup instead of failing silently per bar.
func TestNewRejectsBadConfig(t *testing.T) {
	t.Parallel()

	for _, cfg := range []config.AIConfig{
		{Enabled: true, Provider: "ollama", BaseURL: "http://x"},             // no model
		{Enabled: true, Provider: "ollama", Model: "m"},                      // no base_url
		{Enabled: true, Provider: "gemini", Model: "m", BaseURL: "http://x"}, // unknown provider
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("New(%+v) = nil error, want a configuration error", cfg)
		}
	}
}

// TestConfigSurvivesDisabling reproduces the "disabling AI wipes its api_key"
// finding: New(disabled) installs a nil Advisor, and the old (*Advisor).Config
// returned the zero value for a nil receiver — so toggling ai.enabled off
// (without touching api_key) and reading the config back used to return an
// empty key, permanently losing it from the caller's point of view (the API
// only ever shows a redacted marker, never the real value, so there was
// nothing to restore from). Package-level Config must keep returning the last
// submitted settings regardless of Enabled.
//
// Does not run in parallel: it exercises the package-level Configure/Config
// state, which no other test in this package touches.
func TestConfigSurvivesDisabling(t *testing.T) {
	t.Cleanup(func() { _ = Configure(config.AIConfig{}) })

	if err := Configure(config.AIConfig{
		Enabled:  true,
		Provider: "ollama",
		Model:    "m",
		BaseURL:  "http://x",
		APIKey:   "secret-key",
	}); err != nil {
		t.Fatalf("Configure(enabled): %v", err)
	}

	if got := Config().APIKey; got != "secret-key" {
		t.Fatalf("api_key = %q while enabled, want %q", got, "secret-key")
	}

	if err := Configure(config.AIConfig{Enabled: false, APIKey: "secret-key"}); err != nil {
		t.Fatalf("Configure(disabled): %v", err)
	}

	if Default() != nil {
		t.Fatal("Default() is non-nil after a disabled Configure")
	}

	if got := Config().APIKey; got != "secret-key" {
		t.Fatalf("api_key = %q after disabling, want it preserved as %q", got, "secret-key")
	}
}

// TestForgetDropsCache checks a symbol removed from the watchlist stops being
// cached.
func TestForgetDropsCache(t *testing.T) {
	t.Parallel()

	a := testAdvisor(t, config.AIConfig{Blocking: true, RefreshInterval: time.Hour},
		func(context.Context, Features) (Verdict, error) {
			return Verdict{Action: Buy, Confidence: 0.5, At: time.Now()}, nil
		})

	a.Verdict("AAPL", feats("AAPL", 1))

	if s := a.Stats(); s.Cached != 1 {
		t.Fatalf("cached = %d, want 1", s.Cached)
	}

	a.Forget("AAPL")

	if s := a.Stats(); s.Cached != 0 {
		t.Errorf("cached = %d after Forget, want 0", s.Cached)
	}
}

// TestAdvisorEndToEndOverHTTP exercises the whole path — config → HTTP request
// to an Ollama-shaped server → JSON extraction → cached verdict — with nothing
// stubbed but the model itself.
func TestAdvisorEndToEndOverHTTP(t *testing.T) {
	t.Parallel()

	var got capture

	srv := stubServer(
		t,
		&got,
		http.StatusOK,
		`{"message":{"content":"{\"action\":\"BUY\",\"confidence\":0.72,\"reason\":\"trend and momentum agree\"}"}}`,
	)

	a, err := New(config.AIConfig{
		Enabled: true, Provider: "ollama", BaseURL: srv.URL, Model: "llama3.2",
		Blocking: true, RefreshInterval: time.Hour, Timeout: 5 * time.Second,
		MaxStrength: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	h, l, c, v := series(300)

	f, ok := BuildFeatures("AAPL", h, l, c, v, Params{}.WithDefaults())
	if !ok {
		t.Fatal("BuildFeatures: not ok")
	}

	verdict, ok := a.Verdict("AAPL", f)
	if !ok {
		t.Fatal("want a verdict")
	}

	if verdict.Action != Buy || verdict.Confidence != 0.72 {
		t.Errorf("verdict = %+v, want BUY/0.72", verdict)
	}

	if verdict.Model != "llama3.2" || verdict.At.IsZero() {
		t.Errorf("verdict metadata = %+v, want the model name and a timestamp", verdict)
	}

	// The model must actually have been shown the readings, not just the symbol.
	msgs, _ := got.body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v, want a system and a user message", got.body["messages"])
	}

	user, _ := msgs[1].(map[string]any)
	if content, _ := user["content"].(string); !strings.Contains(content, "rsi:") ||
		!strings.Contains(content, "symbol: AAPL") {
		t.Errorf("user message did not carry the snapshot: %q", user["content"])
	}

	if s := a.Stats(); s.Calls != 1 || s.Errors != 0 || !s.Enabled {
		t.Errorf("stats = %+v, want one clean call", s)
	}
}

// fixedCompleter is a completer that always returns the same answer.
type fixedCompleter struct{ answer string }

// Name identifies the stub.
func (fixedCompleter) Name() string { return "stub" }

// Complete returns the canned answer.
func (c fixedCompleter) Complete(context.Context, string, string) (string, error) {
	return c.answer, nil
}

// TestBlockingAdvisorRunsOnTheBarClock is the regression for AI2-1. A
// blocking advisor — the backtest of it config.example.yaml tells operators to
// run before trusting "ai" — timed refresh_interval and max_age on the WALL
// clock, while a replay races through bars in milliseconds. After the first
// bar every bar replayed in the next 15 real minutes got that first verdict
// back: 1 model call for 1000 bars. On the bar's own clock, bars 20 simulated
// minutes apart must each get asked.
func TestBlockingAdvisorRunsOnTheBarClock(t *testing.T) {
	t.Parallel()

	calls := 0
	a := testAdvisor(t, config.AIConfig{Blocking: true, RefreshInterval: 15 * time.Minute},
		func(context.Context, Features) (Verdict, error) {
			calls++

			return Verdict{Action: Buy, Confidence: 0.9}, nil
		})

	base := time.Date(2026, 1, 5, 14, 30, 0, 0, time.UTC)

	for i := range 5 {
		f := Features{Symbol: "X", RSI: float64(30 + 10*i), At: base.Add(time.Duration(i) * 20 * time.Minute)}
		if _, ok := a.Verdict("X", f); !ok {
			t.Fatalf("bar %d: no usable verdict on the bar clock", i)
		}
	}

	if calls != 5 {
		t.Errorf("5 bars 20 simulated minutes apart made %d model calls, want 5 (one per refresh interval of market time)", calls)
	}
}

// TestUnchangedReadingStillRefreshesOnceExpired is the regression for AI2-5:
// a refresh was only due when the fingerprint changed, so in a quiet market
// the verdict aged past max_age with nothing ever asking again and the
// detector voted "awaiting model" until some reading moved.
func TestUnchangedReadingStillRefreshesOnceExpired(t *testing.T) {
	t.Parallel()

	calls := 0
	a := testAdvisor(t, config.AIConfig{
		Blocking: true, RefreshInterval: 15 * time.Minute, MaxAge: 30 * time.Minute,
	}, func(context.Context, Features) (Verdict, error) {
		calls++

		return Verdict{Action: Buy, Confidence: 0.9}, nil
	})

	base := time.Date(2026, 1, 5, 14, 30, 0, 0, time.UTC)
	f := Features{Symbol: "X", RSI: 50, At: base}

	a.Verdict("X", f)

	f.At = base.Add(45 * time.Minute) // same reading, verdict now past max_age
	if _, ok := a.Verdict("X", f); !ok {
		t.Error("an expired verdict on an unchanged reading was not refreshed: no usable verdict")
	}

	if calls != 2 {
		t.Errorf("model calls = %d, want 2", calls)
	}
}
