package autotune

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/engine"
	"github.com/Kellerman81/go_finance_bot/internal/errlog"
	"github.com/Kellerman81/go_finance_bot/internal/notify"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
)

// defaultInterval/defaultBars/defaultMinTrades are AutotuneConfig's own
// documented zero-value fallbacks (see config.AutotuneConfig's doc comment)
// — applied here, not in config.Default(), so a config loaded before this
// field existed (yaml omits it entirely, decoding to the zero value) behaves
// identically to one that explicitly writes 0.
const (
	defaultInterval  = 24 * time.Hour
	defaultBars      = 1440
	defaultMinTrades = 10
)

// RunOnce runs a single autotune pass immediately, evaluating every
// eligible symbol under cfg (bars/min_trades/objective/min_return_pct/
// run_only_for_symbols/exclude_symbols) and applying any that improve —
// exactly what Schedule performs on its own clock, triggered on demand
// instead of waiting for the next tick. Unlike Schedule, this runs
// regardless of cfg.Enabled: it's a deliberate, operator-initiated action
// (the Settings tab's "Run autotune now" button), not the unsupervised
// background job — a natural way to see what a full pass across the
// watchlist would actually do before trusting the scheduler to do it
// unattended. The writes themselves are exactly as unsupervised either
// way: RunOnce applies a winning proposal directly, with no separate
// confirmation step of its own — that responsibility sits at the caller
// (the button's own confirm dialog).
//
// It reports false when the run slot was already taken and nothing ran.
func RunOnce(
	ctx context.Context, eng *engine.Engine, rm *risk.Manager, pf *portfolio.Portfolio,
	cfg config.AutotuneConfig, configPath string, log *slog.Logger,
) bool {
	return runPass(ctx, eng, rm, pf, cfg, configPath, log)
}

// StartOnce is RunOnce in the background for an HTTP handler: it claims the
// run slot SYNCHRONOUSLY and reports whether it got it, then runs the pass on
// its own goroutine and releases the slot when done. Checking Busy and then
// starting a goroutine that tried for the slot itself answered "started" for
// a pass that, having lost the slot in between, never ran (AT2-7).
func StartOnce(
	ctx context.Context, eng *engine.Engine, rm *risk.Manager, pf *portfolio.Portfolio,
	cfg config.AutotuneConfig, configPath string, log *slog.Logger,
) bool {
	if !TryAcquire() {
		return false
	}

	go func() {
		defer Release()
		// The scheduled pass runs under cmd/bot's runSupervised and gin's
		// Recovery covers only the handler goroutine: a panic anywhere in the
		// backtester or optimizer during a pass started here killed the live
		// process, leaving the book unmanaged (AT3-7). Recovered and
		// reported; the slot is still released by the defer above.
		defer func() {
			if r := recover(); r != nil {
				log.Error("autotune: the manual pass panicked", "panic", r)
				errlog.Recordf("autotune", "manual pass panicked: %v", r)
			}
		}()

		passHeld(ctx, eng, rm, pf, cfg, configPath, log)
	}()

	return true
}

// Busy reports whether a symbol search (the manual per-symbol optimizer
// button, RunOnce, or Schedule's own ticker) currently holds the shared run
// slot, without claiming it itself — a non-blocking hint for a caller (an
// HTTP handler) that wants to tell an operator "already running" up front,
// rather than silently deferring to runPass's own internal TryAcquire and
// leaving a button click that quietly did nothing.
//
// A length check, not a TryAcquire/Release pair: claiming the slot to ask the
// question made a click on "Run autotune now" able to make a scheduled tick
// that fired in that instant lose its pass (AT2-7). It is a hint either way —
// the caller's own TryAcquire stays the real gate.
func Busy() bool {
	return len(sem) == cap(sem)
}

// busyRetry is how soon a scheduled pass that found the run slot busy tries
// again. Without it the pass waited a whole interval — 24 h by default —
// because a screener probe or a manual button held the slot for a minute
// (AT2-7).
const busyRetry = 5 * time.Minute

// Schedule runs the auto-optimizer on its own clock for as long as ctx is
// alive. It is a no-op (returns immediately) when cfg.Enabled is false —
// callers still start the goroutine unconditionally (see cmd/bot/main.go's
// runSupervised wiring) rather than branching at the call site, matching how
// every other optional background job in this codebase is gated.
//
// cfg is only the STARTUP snapshot, used to decide whether to build a ticker
// at all and at what interval — exactly like internal/screener's own
// Interval (see Engine.SetScreenerConfig's doc comment), the ticker cannot
// be rebuilt without a restart, so enabling this for the first time or
// changing how often it runs both need one. Every OTHER field is re-read
// live from eng.AutotuneConfig() on each tick (kept in sync with the API's
// PUT /api/settings/autotune via Engine.SetAutotuneConfig), including
// Enabled itself — so turning the job back OFF, or editing
// bars/min_trades/run_only_for_symbols/exclude_symbols, applies without a
// restart as long as it was already running.
//
// Alerts go out via eng.Notify (nil-safe whether or not a notifier is
// installed) — there is no separate notifier parameter, since the engine
// already owns the process's one outbound alert sink and every other
// engine-adjacent caller in this codebase reaches it the same way.
//
// Deliberately no initial pass at startup, unlike internal/screener's
// Schedule. This one persists to disk (config.PatchSymbolOverride) and
// mutates live trading parameters (Engine.SetSymbolStrategies) completely
// unsupervised — there is no data-freshness reason to fire seconds into a
// fresh deploy before the machinery has a single successful tick behind it,
// so it waits for the first configured interval to elapse.
func Schedule(
	ctx context.Context, eng *engine.Engine, rm *risk.Manager, pf *portfolio.Portfolio,
	cfg config.AutotuneConfig, configPath string, log *slog.Logger,
) {
	if !cfg.Enabled {
		return
	}

	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// The reactive trigger shares this goroutine rather than getting one of
	// its own: both paths run the same search, and only one search may run
	// at a time anyway (TryAcquire), so a single select is both simpler and
	// makes the mutual exclusion obvious rather than incidental.
	losses := eng.LossTuneRequests()

	// retry fires once after a tick that found the slot busy; nil (never
	// fires) otherwise.
	var retry <-chan time.Time

	scheduled := func() {
		retry = nil

		live := eng.AutotuneConfig()
		if !live.Enabled {
			log.Info("autotune: disabled live, skipping this tick")

			return
		}

		if !runPass(ctx, eng, rm, pf, live, configPath, log) {
			retry = time.After(busyRetry)
		}
	}

	lossRetry := newLossRetries(time.After)
	lossPass := func(symbol string) bool {
		return runLossPass(ctx, eng, rm, pf, eng.AutotuneConfig(), configPath, symbol, log)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			scheduled()
		case <-retry:
			scheduled()
		case symbol := <-losses:
			lossRetry.serve(symbol, lossPass)
		case <-lossRetry.due:
			lossRetry.retry(lossPass)
		}
	}
}

// lossRetries holds the loss re-tunes that found the run slot busy, one entry
// per symbol, retried together after busyRetry. A streak publishes only every
// after_losses-th loss in a row, so a dropped request waited for N MORE
// losses — and a screener probe holds the slot routinely (AT3-6). A set, not
// a queue: one symbol's streak is one request however often it lost the
// race, and runLossPass re-checks enablement and the cooldown when served.
type lossRetries struct {
	pending map[string]bool
	due     <-chan time.Time // nil (never fires) while nothing is pending
	after   func(time.Duration) <-chan time.Time
}

func newLossRetries(after func(time.Duration) <-chan time.Time) *lossRetries {
	return &lossRetries{pending: map[string]bool{}, after: after}
}

// serve runs symbol's re-tune, keeping it pending when run reports the slot
// busy.
func (l *lossRetries) serve(symbol string, run func(string) bool) {
	if run(symbol) {
		delete(l.pending, symbol)

		return
	}

	l.pending[symbol] = true
	if l.due == nil {
		l.due = l.after(busyRetry)
	}
}

// retry serves every pending symbol again; one still busy re-arms the timer.
func (l *lossRetries) retry(run func(string) bool) {
	l.due = nil

	for _, symbol := range slices.Sorted(maps.Keys(l.pending)) {
		l.serve(symbol, run)
	}
}

// runLossPass re-tunes exactly one symbol, in response to its losing streak.
//
// It applies the same eligibility rules a scheduled pass does — a symbol
// excluded by RunOnlyForSymbols/ExcludeSymbols is not reachable through a loss
// streak either — and then runs the identical search with the identical gates,
// so a reactive run can only ever apply a config a scheduled run would also
// have accepted. The streak decides WHEN to look, never WHAT is allowed to
// win.
//
// It reports false only when the run slot was busy and the request should be
// retried; every other outcome — ran, ineligible, disabled, in cooldown — is
// final.
func runLossPass(
	ctx context.Context, eng *engine.Engine, rm *risk.Manager, pf *portfolio.Portfolio,
	cfg config.AutotuneConfig, configPath, symbol string, log *slog.Logger,
) bool {
	// Re-checked against the LIVE config, not only where the streak was
	// counted: the request may have been queued before the operator turned
	// either switch off, and a tune that lands after "autotune: off" would
	// break exactly the guarantee that switch is there to make.
	if !cfg.Enabled || !cfg.OnLoss.Enabled {
		log.Info("autotune on_loss: disabled live, dropping request", "symbol", symbol)

		return true
	}

	if len(selectSymbols([]string{symbol}, cfg.RunOnlyForSymbols, cfg.ExcludeSymbols)) == 0 {
		log.Info("autotune on_loss: symbol is not eligible, skipping",
			"symbol", symbol)

		return true
	}

	// The cooldown again, now: it was judged when the streak was counted,
	// but a request that waited in the channel behind a scheduled pass
	// (this goroutine runs both) may find that pass has just applied a new
	// config to this very symbol. Re-tuning it seconds later — on a config
	// with zero trades since — is the stale backlog on_loss.cooldown exists
	// to prevent (AT2-5).
	if !eng.LossTuneCooldownPassed(symbol, cfg.OnLoss.Cooldown) {
		log.Info("autotune on_loss: symbol was tuned within the cooldown, dropping request",
			"symbol", symbol, "cooldown", cfg.OnLoss.Cooldown)

		return true
	}

	if !TryAcquire() {
		// Retried after busyRetry, not dropped (AT3-6): Schedule keeps one
		// pending entry per symbol, so this is never a stale backlog, and
		// the checks above run again when it is served. Requests that
		// arrive DURING a scheduled pass wait in the channel — this
		// goroutine is busy running it — and the cooldown check keeps
		// those honest too.
		log.Info("autotune on_loss: a search is already running, retrying later",
			"symbol", symbol)

		return false
	}
	defer Release()

	log.Info("autotune on_loss: re-tuning after a losing streak", "symbol", symbol)

	if !tuneSymbol(ctx, eng, rm, pf, cfg, configPath, symbol, log) {
		log.Info("autotune on_loss: no candidate cleared the gates, config unchanged",
			"symbol", symbol)
	}

	return true
}

// runPass evaluates every eligible watchlist symbol once, strictly
// sequentially. Sequential, not per-symbol goroutines: SetSymbolStrategies
// replaces the engine's whole per-symbol map, so applying one symbol's
// result is a read-modify-write (OverriddenSymbols+StrategyConfigFor to
// reconstruct the current map, merge in the change, SetSymbolStrategies the
// merged map) — concurrent applies within one pass could race and silently
// drop each other's write. Sequential sidesteps that without a new lock.
//
// It reports false only when the run slot was busy and nothing ran.
func runPass(
	ctx context.Context, eng *engine.Engine, rm *risk.Manager, pf *portfolio.Portfolio,
	cfg config.AutotuneConfig, configPath string, log *slog.Logger,
) bool {
	if !TryAcquire() {
		log.Warn("autotune: skipping this pass, a search is already running")

		return false
	}
	defer Release()

	passHeld(ctx, eng, rm, pf, cfg, configPath, log)

	return true
}

// passHeld is runPass's body, for a caller that already holds the run slot.
func passHeld(
	ctx context.Context, eng *engine.Engine, rm *risk.Manager, pf *portfolio.Portfolio,
	cfg config.AutotuneConfig, configPath string, log *slog.Logger,
) {
	symbols := selectSymbols(eng.Watchlist(), cfg.RunOnlyForSymbols, cfg.ExcludeSymbols)

	for _, symbol := range symbols {
		if ctx.Err() != nil {
			return
		}

		tuneSymbol(ctx, eng, rm, pf, cfg, configPath, symbol, log)
	}
}

// tuneSymbol runs one symbol's search and applies the result if it improved,
// reporting whether anything was actually changed. Shared by the scheduled
// pass and the loss-triggered one so the two cannot drift apart — the gates,
// the bar window and the apply path are defined once.
//
// The caller owns the run slot (TryAcquire/Release); this does not claim it.
func tuneSymbol(
	ctx context.Context, eng *engine.Engine, rm *risk.Manager, pf *portfolio.Portfolio,
	cfg config.AutotuneConfig, configPath, symbol string, log *slog.Logger,
) bool {
	if ctx.Err() != nil {
		return false
	}

	bars := cfg.Bars
	if bars <= 0 {
		bars = defaultBars
	}

	minTrades := cfg.MinTrades
	if minTrades <= 0 {
		minTrades = defaultMinTrades
	}

	objective := cfg.Objective
	if objective == "" {
		objective = DefaultObjective
	}

	candles := eng.Series(symbol)
	if len(candles) > bars {
		candles = candles[len(candles)-bars:]
	}

	if len(candles) < MinBars {
		return false
	}

	result := RunSymbolOptimize(ctx, eng, rm, pf, symbol, candles, minTrades, objective, cfg.MinReturnPct)
	if !result.Improved {
		return false
	}

	// Shutdown began while the search ran: do not write config.yaml and
	// re-wire the live engine on the way out (AT2-8). The next pass will
	// find the same winner if it is real.
	if ctx.Err() != nil {
		log.Info("autotune: shutting down, discarding a finished search", "symbol", symbol)

		return false
	}

	applyResult(eng, configPath, symbol, result, log)

	return true
}

// selectSymbols narrows watchlist to the set a pass should evaluate:
// runOnly, when non-empty, restricts to exactly that allow-list; exclude is
// then subtracted regardless — a symbol named in both ends up excluded, per
// config.AutotuneConfig.ExcludeSymbols's documented precedence. Comparisons
// are case-insensitive, matching how symbols are matched everywhere else in
// this codebase (config.PerSymbolStrategy, engine.OverriddenSymbols'
// callers).
func selectSymbols(watchlist, runOnly, exclude []string) []string {
	allow := toUpperSet(runOnly)
	deny := toUpperSet(exclude)

	out := make([]string, 0, len(watchlist))

	for _, sym := range watchlist {
		key := strings.ToUpper(strings.TrimSpace(sym))

		if len(allow) > 0 && !allow[key] {
			continue
		}

		if deny[key] {
			continue
		}

		out = append(out, sym)
	}

	return out
}

func toUpperSet(symbols []string) map[string]bool {
	set := make(map[string]bool, len(symbols))
	for _, sym := range symbols {
		set[strings.ToUpper(strings.TrimSpace(sym))] = true
	}

	return set
}

// applyResult writes result's winning fields into configPath's
// strategy_overrides entry for symbol, then — only once that write has
// succeeded — updates the live engine to match, so the file on disk and the
// running process never disagree about which config a symbol is trading
// under.
//
// On a PatchSymbolOverride failure, the live engine is deliberately left
// untouched: applying the change live while the file write failed would
// mean a restart reverts to the OLD config while the process keeps running
// the NEW one until then — exactly the kind of file/process divergence this
// order avoids.
func applyResult(eng *engine.Engine, configPath, symbol string, result Result, log *slog.Logger) {
	fields := result.Fields

	// The search diffed its winner against the symbol's config as it was
	// when the search began. A strategy save during the 20-40 s search
	// moved that base, and the same diff on the new one is a config no
	// trial ran — refused, the next pass searches from the new base (AT3-2).
	if live := eng.StrategyConfigFor(symbol); !reflect.DeepEqual(live, result.Base) {
		log.Warn("autotune: the strategy changed while the search ran; discarding its result",
			"symbol", symbol)
		errlog.Recordf("autotune", "%s: strategy changed during the search; result discarded, "+
			"the next pass searches the new one", symbol)

		return
	}

	if err := config.PatchSymbolOverride(configPath, symbol, fields); err != nil {
		log.Error("autotune: failed to write strategy_overrides", "symbol", symbol, "err", err)
		errlog.Recordf("autotune", "write strategy_overrides for %s: %v", symbol, err)

		eng.Notify(notify.Event{
			Kind: notify.KindError, Symbol: symbol,
			Title: "autotune: failed to write strategy_overrides",
			Text:  fmt.Sprintf("%s: %v", symbol, err),
		})

		return
	}

	merged := make(map[string]config.StrategyConfig, len(eng.OverriddenSymbols())+1)
	for _, sym := range eng.OverriddenSymbols() {
		merged[sym] = eng.StrategyConfigFor(sym)
	}

	if newCfg, err := config.ApplyStrategyOverride(eng.StrategyConfigFor(symbol), fields); err == nil {
		merged[symbol] = newCfg
	} else {
		log.Error("autotune: wrote strategy_overrides but could not resolve it live", "symbol", symbol, "err", err)
		errlog.Recordf("autotune", "resolve strategy_overrides for %s: %v", symbol, err)

		return
	}

	eng.SetSymbolStrategies(merged)

	// Only after both the file write and the live apply have succeeded —
	// this timestamp is what recentWinFor judges the NEXT pass's proposal
	// against, so it must mark the moment this exact config actually took
	// effect, not the moment the search merely decided to try it.
	eng.RecordAutotuneApply(symbol)

	changes := formatOverride(fields)
	log.Info("autotune: applied a new strategy_overrides entry", "symbol", symbol, "changes", changes)

	eng.Notify(notify.Event{
		Kind: notify.KindAudit, Symbol: symbol,
		Title: "autotune: applied a new strategy_overrides entry",
		Text:  fmt.Sprintf("%s: %s", symbol, strings.Join(changes, "; ")),
	})
}
