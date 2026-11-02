// Package ai adds an LLM-backed opinion to the strategy: it shows a local
// (Ollama) or remote model a compact snapshot of the same normalised indicator
// readings the rule-based detectors vote on, and turns the model's answer into
// a BUY/SELL/HOLD vote with a confidence.
//
// Three properties make that safe to put in a trading loop:
//
//   - It never blocks the hot path. Verdicts are cached per symbol and refreshed
//     in the background, so an evaluation tick (or a backtest bar) costs a map
//     lookup, not a model round-trip.
//   - It fails to HOLD. A timeout, a dead server, a malformed answer or a stale
//     verdict all produce HOLD — the advisor can decline to have an opinion, but
//     it cannot invent a trade.
//   - It is bounded. At most MaxConcurrent requests are in flight, at most one
//     per symbol per RefreshInterval, and its strength contribution is capped by
//     MaxStrength so the model can tilt a decision without carrying one alone.
package ai

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/errlog"
)

// errSource is the errlog source name for advisor failures, surfaced in the web
// UI's error panel alongside broker and feed errors.
const errSource = "ai"

// Advisor caches and refreshes model verdicts, one per symbol.
type Advisor struct {
	cfg  config.AIConfig
	cl   completer
	sys  string
	sem  chan struct{} // bounds in-flight requests
	now  func() time.Time
	call func(ctx context.Context, f Features) (Verdict, error)

	mu    sync.Mutex
	cache map[string]*entry
	stats Stats
}

// entry is one symbol's cached verdict plus its refresh bookkeeping.
type entry struct {
	verdict  Verdict
	have     bool
	inflight bool
	asked    time.Time // when the last refresh was started (not when it landed)
	mark     string    // feature fingerprint the cached verdict was formed on
}

// Stats is the advisor's operational counters, exposed at GET /api/ai so a
// misconfigured endpoint is visible without reading logs.
type Stats struct {
	Enabled   bool      `json:"enabled"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	BaseURL   string    `json:"base_url"`
	Calls     int64     `json:"calls"`
	Errors    int64     `json:"errors"`
	Cached    int       `json:"cached_symbols"`
	LastError string    `json:"last_error,omitempty"`
	LastAt    time.Time `json:"last_verdict_at,omitempty"`
}

// New builds an Advisor from config. It returns (nil, nil) when the advisor is
// disabled, so callers can treat "off" and "absent" identically.
func New(cfg config.AIConfig) (*Advisor, error) {
	if !cfg.Enabled {
		return nil, nil //nolint:nilnil // disabled is not an error; nil Advisor is the "off" state
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	cfg = withAIDefaults(cfg)

	hc := &http.Client{Timeout: cfg.Timeout}

	cl, err := newCompleter(cfg.Provider, cfg.BaseURL, cfg.Model, cfg.APIKey, cfg.Temperature, hc)
	if err != nil {
		return nil, err
	}

	sys := DefaultSystemPrompt
	if s := strings.TrimSpace(cfg.SystemPrompt); s != "" {
		sys = s
	}

	a := &Advisor{
		cfg:   cfg,
		cl:    cl,
		sys:   sys,
		sem:   make(chan struct{}, cfg.MaxConcurrent),
		now:   time.Now,
		cache: make(map[string]*entry),
	}

	a.call = a.ask

	return a, nil
}

// withAIDefaults fills in any unset advisor setting, so a config that only names
// a model still behaves sensibly.
func withAIDefaults(c config.AIConfig) config.AIConfig {
	if c.Timeout <= 0 {
		c.Timeout = 20 * time.Second
	}

	if c.RefreshInterval <= 0 {
		c.RefreshInterval = config.DefaultAIRefreshInterval
	}

	if c.MaxAge <= 0 {
		c.MaxAge = 4 * c.RefreshInterval
	}

	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 2
	}

	if c.MaxStrength <= 0 || c.MaxStrength > 1 {
		c.MaxStrength = 1
	}

	return c
}

// Verdict returns the current opinion for symbol, kicking off a background
// refresh when the cached one is due. ok is false when there is no usable
// verdict — the caller must vote HOLD in that case.
//
// Blocking mode waits for the model instead, which is what a deliberate,
// slow backtest of the advisor wants; the live engine should leave it off.
func (a *Advisor) Verdict(symbol string, f Features) (Verdict, bool) {
	if a == nil {
		return Verdict{}, false
	}

	mark := f.Fingerprint()
	now := a.clockFor(f)

	verdict, have, due := a.claim(symbol, mark, now)
	if !due {
		return verdict, have
	}

	if !a.cfg.Blocking {
		go a.refresh(symbol, f, mark, now)

		return verdict, have
	}

	a.refresh(symbol, f, mark, now)

	return a.cached(symbol, now)
}

// clockFor is the time a call is judged at. Live (non-blocking) that is the
// wall clock. A blocking advisor — the deliberate backtest of it — runs on the
// BAR's time when the caller supplied one: refresh_interval and max_age are
// durations of market time there, and on the wall clock a replay racing
// through years of bars in minutes reused its first verdict for every bar of
// the next 15 real minutes — 1 model call, 1000 bars, the same BUY on every
// one — so the backtest the docs prescribe never measured the advisor (AI2-1).
func (a *Advisor) clockFor(f Features) time.Time {
	if a.cfg.Blocking && !f.At.IsZero() {
		return f.At
	}

	return a.now()
}

// claim reads the cached verdict for symbol and, when a refresh is due, marks
// the entry in flight so exactly one caller performs it. It returns the verdict
// usable right now (which may be the previous one) and whether the caller owns
// the refresh.
func (a *Advisor) claim(symbol, mark string, now time.Time) (verdict Verdict, have, due bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	e, seen := a.cache[symbol]

	// A clock that went BACKWARDS — a second replay starting at an earlier
	// bar — made both ages negative: never due, always fresh. The previous
	// run's last verdict, formed on the end of the data, answered every bar of
	// the next one with no model call (AI3-1). An entry from the future is no
	// entry.
	if !seen || e.fromTheFuture(now) {
		e = &entry{}
		a.cache[symbol] = e
	}

	// A refresh is due once the interval has elapsed, unless nothing moved: an
	// unchanged fingerprint would only buy the same answer again — until that
	// answer EXPIRES. Without the age test a quiet market let the verdict
	// pass max_age with nothing ever asking again, and the detector voted
	// "awaiting model" until some reading happened to move (AI2-5).
	stale := e.have && now.Sub(e.verdict.At) > a.cfg.MaxAge

	due = !e.inflight &&
		now.Sub(e.asked) >= a.cfg.RefreshInterval &&
		(!e.have || e.mark != mark || stale)

	if due {
		e.inflight = true
		e.asked = now
	}

	return e.verdict, e.have && now.Sub(e.verdict.At) <= a.cfg.MaxAge, due
}

// fromTheFuture reports whether e was asked, or answered, after now — the
// clock went backwards (AI3-1). An entry with a call in flight is kept: its
// answer is on its way.
func (e *entry) fromTheFuture(now time.Time) bool {
	return !e.inflight && (now.Before(e.asked) || (e.have && now.Before(e.verdict.At)))
}

// cached returns symbol's stored verdict if it is still within max_age.
func (a *Advisor) cached(symbol string, now time.Time) (Verdict, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	e, ok := a.cache[symbol]
	if !ok {
		return Verdict{}, false
	}

	return e.verdict, e.have && now.Sub(e.verdict.At) <= a.cfg.MaxAge
}

// refresh performs one model call and stores its result. It always clears the
// in-flight flag, so a failed call cannot wedge a symbol permanently.
func (a *Advisor) refresh(symbol string, f Features, mark string, now time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.Timeout)
	defer cancel()

	// Bound concurrency: a large watchlist must not open one connection per
	// symbol against a single local GPU. The semaphore acquisition itself is
	// bounded by the same ctx/timeout as the model call below — it used to
	// be acquired before this timeout context even existed, so an
	// unbounded queue wait for a free slot preceded the (correctly bounded)
	// call. That's invisible in non-blocking (live) mode, where refresh
	// runs in its own background goroutine — but Blocking mode (the
	// deliberate, slow backtest-of-the-advisor use case this package's doc
	// comment describes) calls refresh synchronously in the caller's own
	// evaluation goroutine: a watchlist that goes due for refresh all at
	// once could stall that goroutine for however long every other
	// in-flight symbol took to clear, with nothing bounding the wait. See
	// CODE_REVIEW_FINDINGS.md's "Verdict not bounded by the HTTP timeout in
	// blocking mode".
	select {
	case a.sem <- struct{}{}:
		defer func() { <-a.sem }()
	case <-ctx.Done():
		a.recordFailure(symbol, ctx.Err())

		return
	}

	v, err := a.call(ctx, f)

	a.mu.Lock()
	defer a.mu.Unlock()

	e := a.cache[symbol]
	if e == nil { // symbol dropped from the watchlist mid-flight
		return
	}

	e.inflight = false
	a.stats.Calls++

	if err != nil {
		a.stats.Errors++

		a.stats.LastError = err.Error()

		errlog.Recordf(errSource, "%s: %v", symbol, err)

		return
	}

	if a.cfg.Blocking && !f.At.IsZero() {
		v.At = now // judged on the bar's clock, like claim and cached
	}

	e.verdict, e.have, e.mark = v, true, mark
	a.stats.LastAt = v.At
}

// recordFailure marks symbol's in-flight refresh as failed without a model
// call ever happening — e.g. the wait for a free concurrency slot itself
// timed out. The entry must still be released from "in flight" or it would
// never be eligible to refresh again.
func (a *Advisor) recordFailure(symbol string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if e := a.cache[symbol]; e != nil {
		e.inflight = false
	}

	a.stats.Errors++
	a.stats.LastError = err.Error()

	errlog.Recordf(errSource, "%s: %v", symbol, err)
}

// ask sends the snapshot to the model and parses its answer. It is the
// production implementation of Advisor.call, replaced in tests.
func (a *Advisor) ask(ctx context.Context, f Features) (Verdict, error) {
	raw, err := a.cl.Complete(ctx, a.sys, f.Prompt())
	if err != nil {
		return Verdict{}, err
	}

	v, err := parseVerdict(raw)
	if err != nil {
		return Verdict{}, err
	}

	// A verdict the model is not sure enough about is not worth a vote.
	if v.Confidence < a.cfg.MinConfidence {
		v.Action, v.Confidence = Hold, 0
	}

	v.Model = a.cfg.Model
	v.At = a.now()

	if v.Reason == "" {
		v.Reason = string(v.Action)
	}

	return v, nil
}

// MaxStrength is the ceiling on the strength a verdict may contribute.
func (a *Advisor) MaxStrength() float64 {
	if a == nil {
		return 0
	}

	return a.cfg.MaxStrength
}

// Model reports the configured model name (empty when disabled).
func (a *Advisor) Model() string {
	if a == nil {
		return ""
	}

	return a.cfg.Model
}

// Stats returns a snapshot of the advisor's counters.
func (a *Advisor) Stats() Stats {
	if a == nil {
		return Stats{}
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	s := a.stats

	s.Enabled = true
	s.Provider = a.cl.Name()
	s.Model = a.cfg.Model
	s.BaseURL = a.cfg.BaseURL
	s.Cached = len(a.cache)

	return s
}

// Forget drops a symbol's cached verdict (called when it leaves the watchlist).
func (a *Advisor) Forget(symbol string) {
	if a == nil {
		return
	}

	a.mu.Lock()

	delete(a.cache, symbol)
	a.mu.Unlock()
}

// ----- process-wide default advisor -----

var (
	defaultMu  sync.RWMutex
	defaultAd  *Advisor
	defaultCfg config.AIConfig
)

// Configure builds the process-wide advisor from config and installs it. It is
// called once at startup; the strategy can then be rebuilt live (which
// reconstructs detectors) without losing the verdict cache. A disabled config
// installs a nil advisor, which every consumer treats as "always HOLD" — but
// the submitted config itself (including any api_key) is retained regardless,
// so toggling ai.enabled off and back on does not lose settings that were
// never re-entered. See Config.
func Configure(cfg config.AIConfig) error {
	a, err := New(cfg)
	if err != nil {
		return err
	}

	defaultMu.Lock()

	defaultAd = a
	defaultCfg = cfg
	defaultMu.Unlock()

	return nil
}

// Fresh returns an advisor with a's model client and settings but its own
// empty verdict cache, request slots and counters — for a replay, which must
// never read or write the live per-symbol cache. Every in-process backtest
// used the process-wide advisor: a replayed bar got TODAY's verdict
// (lookahead), and a due refresh fed historical features overwrote the
// verdict the live loop trades on (ST3-1, AI3-2). Two replays also shared one
// cache across a clock that went back (AI3-1). Nil-safe: a nil a returns nil.
func (a *Advisor) Fresh() *Advisor {
	if a == nil {
		return nil
	}

	f := &Advisor{
		cfg:   a.cfg,
		cl:    a.cl,
		sys:   a.sys,
		sem:   make(chan struct{}, cap(a.sem)),
		now:   a.now,
		cache: make(map[string]*entry),
	}

	f.call = f.ask

	return f
}

// Default returns the process-wide advisor, or nil when none is configured.
func Default() *Advisor {
	defaultMu.RLock()
	defer defaultMu.RUnlock()

	return defaultAd
}

// Config returns the most recently submitted advisor settings, regardless of
// whether ai.enabled is currently true. The API reads it back so a disabled
// advisor's api_key (and other fields) survive being read and re-saved, and
// so the UI shows what was last configured rather than the zero value.
func Config() config.AIConfig {
	defaultMu.RLock()
	defer defaultMu.RUnlock()

	return defaultCfg
}
