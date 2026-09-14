package autotune

import (
	"context"
	"fmt"
	"log/slog"
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
func RunOnce(
	ctx context.Context, eng *engine.Engine, rm *risk.Manager, pf *portfolio.Portfolio,
	cfg config.AutotuneConfig, configPath string, log *slog.Logger,
) {
	runPass(ctx, eng, rm, pf, cfg, configPath, log)
}

// Busy reports whether a symbol search (the manual per-symbol optimizer
// button, RunOnce, or Schedule's own ticker) currently holds the shared run
// slot, without claiming it itself — a non-blocking hint for a caller (an
// HTTP handler) that wants to tell an operator "already running" up front,
// rather than silently deferring to runPass's own internal TryAcquire and
// leaving a button click that quietly did nothing.
func Busy() bool {
	if TryAcquire() {
		Release()

		return false
	}

	return true
}

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

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			live := eng.AutotuneConfig()
			if !live.Enabled {
				log.Info("autotune: disabled live, skipping this tick")

				continue
			}

			runPass(ctx, eng, rm, pf, live, configPath, log)
		case symbol := <-losses:
			runLossPass(ctx, eng, rm, pf, eng.AutotuneConfig(), configPath, symbol, log)
		}
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
func runLossPass(
	ctx context.Context, eng *engine.Engine, rm *risk.Manager, pf *portfolio.Portfolio,
	cfg config.AutotuneConfig, configPath, symbol string, log *slog.Logger,
) {
	// Re-checked against the LIVE config, not only where the streak was
	// counted: the request may have been queued before the operator turned
	// either switch off, and a tune that lands after "autotune: off" would
	// break exactly the guarantee that switch is there to make.
	if !cfg.Enabled || !cfg.OnLoss.Enabled {
		log.Info("autotune on_loss: disabled live, dropping request", "symbol", symbol)

		return
	}

	if len(selectSymbols([]string{symbol}, cfg.RunOnlyForSymbols, cfg.ExcludeSymbols)) == 0 {
		log.Info("autotune on_loss: symbol is not eligible, skipping",
			"symbol", symbol)

		return
	}

	if !TryAcquire() {
		// Dropped rather than queued: by the time a long scheduled pass
		// finishes, this symbol has very likely been re-evaluated by that
		// pass anyway, and a stale backlog of reactive requests would fire
		// searches for streaks that are no longer current.
		log.Warn("autotune on_loss: skipping, a search is already running",
			"symbol", symbol)

		return
	}
	defer Release()

	log.Info("autotune on_loss: re-tuning after a losing streak", "symbol", symbol)

	if !tuneSymbol(ctx, eng, rm, pf, cfg, configPath, symbol, log) {
		log.Info("autotune on_loss: no candidate cleared the gates, config unchanged",
			"symbol", symbol)
	}
}

// runPass evaluates every eligible watchlist symbol once, strictly
// sequentially. Sequential, not per-symbol goroutines: SetSymbolStrategies
// replaces the engine's whole per-symbol map, so applying one symbol's
// result is a read-modify-write (OverriddenSymbols+StrategyConfigFor to
// reconstruct the current map, merge in the change, SetSymbolStrategies the
// merged map) — concurrent applies within one pass could race and silently
// drop each other's write. Sequential sidesteps that without a new lock.
func runPass(
	ctx context.Context, eng *engine.Engine, rm *risk.Manager, pf *portfolio.Portfolio,
	cfg config.AutotuneConfig, configPath string, log *slog.Logger,
) {
	if !TryAcquire() {
		log.Warn("autotune: skipping this pass, a search is already running")

		return
	}
	defer Release()

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

	result := RunSymbolOptimize(eng, rm, pf, symbol, candles, minTrades, objective, cfg.MinReturnPct)
	if !result.Improved {
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
