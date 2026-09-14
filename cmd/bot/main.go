// Command bot runs the finance trading bot: it streams prices, evaluates a
// technical strategy across the watchlist, enforces money limits, optionally
// places paper trades via Alpaca (or an internal simulator), and serves a gin
// HTTP API for tracking and control.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // bundle the IANA tz database so exchange-hours lookups work without OS zoneinfo (e.g. Windows)

	"github.com/Kellerman81/go_finance_bot/data"
	"github.com/Kellerman81/go_finance_bot/internal/ai"
	"github.com/Kellerman81/go_finance_bot/internal/api"
	"github.com/Kellerman81/go_finance_bot/internal/autotune"
	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/engine"
	"github.com/Kellerman81/go_finance_bot/internal/errlog"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/ml"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
	"github.com/Kellerman81/go_finance_bot/internal/storage"
)

// saxoAuthMode returns the effective Saxo auth mode ("certificate" or "token").
func saxoAuthMode(cfg config.Config) string {
	if cfg.Saxo.AuthMode == "certificate" {
		return "certificate"
	}

	return "token"
}

// saxoTokenSource builds the Saxo TokenSource for the configured auth mode:
// a static bearer token, or certificate-based auth with background refresh.
func saxoTokenSource(cfg config.Config) (broker.TokenSource, error) {
	if saxoAuthMode(cfg) == "certificate" {
		return broker.NewCertTokenSource(broker.CertConfig{
			AppKey:       cfg.Saxo.AppKey,
			AppSecret:    cfg.Saxo.AppSecret,
			AppURL:       cfg.Saxo.AppURL,
			UserID:       cfg.Saxo.UserID,
			AuthURL:      cfg.Saxo.SaxoAuthURL(),
			CertPath:     cfg.Saxo.CertPath,
			CertPassword: cfg.Saxo.CertPassword,
		})
	}

	return broker.NewStaticTokenSource(cfg.Saxo.Token), nil
}

// mergeSymbols unions config and persisted watchlists, upper-casing and
// de-duplicating while preserving order (config first).
func mergeSymbols(lists ...[]string) []string {
	seen := make(map[string]struct{})

	var out []string

	for _, list := range lists {
		for _, s := range list {
			s = strings.ToUpper(strings.TrimSpace(s))
			if s == "" {
				continue
			}

			if _, ok := seen[s]; ok {
				continue
			}

			seen[s] = struct{}{}
			out = append(out, s)
		}
	}

	return out
}

// main hands off to run and exits with its status code.
//
// The split matters for exactly one reason: os.Exit does not run deferred
// functions. Several startup failures inside run happen after resources
// that need cleanup — the market feed, storage, the engine's notify worker
// — are already live (see the defers throughout run); calling os.Exit
// directly at any of those points used to skip every one of them. Now every
// early exit is an ordinary return, so this function's own defers (there
// are none besides the ones inside run, which is the point) — rather,
// run's own defers — get to fire on the way out, and only THEN does the
// process actually exit. See CODE_REVIEW_FINDINGS.md's "several os.Exit(1)
// startup paths skip deferred cleanup".
func main() {
	os.Exit(run())
}

// run loads the config, wires feed → strategy → risk → broker → engine →
// API, and runs until the process is interrupted. It returns the process
// exit code rather than calling os.Exit itself, so its defers always run
// first — see main.
//
//nolint:cyclop,funlen // sequential start-up wiring; one step per dependency
func run() int {
	cfgPath := flag.String("config", "data/config.yaml", "path to YAML config file")

	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	if created, err := config.EnsureFile(*cfgPath, data.ExampleConfigYAML); err != nil {
		log.Error("config error", "err", err)
		return 1
	} else if created {
		log.Info("no config file found — wrote a starting config.yaml", "path", *cfgPath)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("config error", "err", err)
		return 1
	}

	prices := engine.NewPriceCache()

	// ----- market data provider -----
	var provider market.DataProvider

	switch {
	case cfg.Feed == "binance":
		provider = market.NewBinance(cfg.BinanceBaseURL)

		log.Info("using Binance public crypto data feed", "base_url", cfg.BinanceBaseURL)
		log.Warn("crypto data is read-only: orders still go to the configured broker " +
			"(use broker: sim to paper-trade crypto)")

	case cfg.Finnhub.APIKey != "":
		provider = market.NewFinnhub(
			cfg.Finnhub.APIKey,
			cfg.Finnhub.RESTBase,
			cfg.Finnhub.WSBase,
			log,
		)
		log.Info("using Finnhub live data feed")

	default:
		provider = market.NewMock(time.Second)

		log.Warn("no FINNHUB_API_KEY set — using offline mock data feed")
	}

	defer provider.Close()

	// ----- broker -----
	var (
		brk     broker.Broker
		history engine.HistorySource
		// paperSim is the simulated broker when one is in use, kept so its
		// price oracle can be upgraded once the engine exists (see below).
		paperSim *broker.Sim
	)

	switch cfg.BrokerProvider() {
	case "saxo":
		tokens, err := saxoTokenSource(cfg)
		if err != nil {
			log.Error("init Saxo auth", "err", err)
			return 1
		}

		sx, err := broker.NewSaxo(
			cfg.Saxo.BaseURL,
			tokens,
			cfg.Saxo.AccountKey,
			cfg.Saxo.AssetTypes,
			cfg.Saxo.PreferredCurrency,
		)
		if err != nil {
			log.Error("init Saxo broker", "err", err)
			return 1
		}

		sx.SetMinOrderInterval(cfg.Safety.MinOrderInterval)

		brk = sx
		history = sx // Saxo charts seed indicator warmup, even off-hours

		log.Info("using Saxo broker", "base_url", cfg.Saxo.BaseURL, "auth_mode", saxoAuthMode(cfg))

	case "trading212":
		brk = broker.NewTrading212(
			cfg.Trading212.BaseURL,
			cfg.Trading212.APIKey,
			cfg.Trading212.Secret,
			cfg.Trading212.ISINs,
			cfg.Trading212.PreferredCurrency,
		)
		log.Info("using Trading 212 broker", "base_url", cfg.Trading212.BaseURL)

	case "alpaca":
		brk = broker.NewAlpaca(cfg.Alpaca.BaseURL, cfg.Alpaca.KeyID, cfg.Alpaca.SecretKey, cfg.Alpaca.PreferredCurrency)
		log.Info("using Alpaca broker", "base_url", cfg.Alpaca.BaseURL, "paper", cfg.Alpaca.Paper)

	default:
		sim := broker.NewSim(cfg.Alpaca.SimCash, prices.Get, cfg.Costs.Commission)
		// Same friction the backtester models, so paper results and backtest
		// results are produced under the same assumptions.
		sim.SetSlippage(func(_ string, buy bool, price float64) float64 {
			return cfg.Costs.FillPrice(buy, price)
		})

		brk = sim
		paperSim = sim

		log.Info("using simulated broker",
			"starting_cash", cfg.Alpaca.SimCash,
			"spread_pct", cfg.Costs.SpreadPct, "slippage_pct", cfg.Costs.SlippagePct)
	}

	// Warmup history: Finnhub's free tier has no historical candles, so unless
	// the broker already provides history (Saxo charts), use Yahoo Finance — a
	// free, keyless source — to seed indicators regardless of the execution
	// broker. Non-fatal: warmup falls back to live ticks if it's unavailable.
	if history == nil {
		history = market.NewYahooHistory()

		log.Info("using Yahoo Finance for indicator warmup (free, no key)")
	}

	// Wrap in a short-TTL cache so repeated warmups and the per-order venue-price
	// lookups reuse a recent fetch instead of hitting the network each time.
	history = market.NewCachingHistory(history, 15*time.Second)

	// ----- persistence -----
	var store engine.Store

	if cfg.Storage.Driver == "sqlite" {
		sq, err := storage.OpenSQLite(cfg.Storage.Path)
		if err != nil {
			log.Error("open storage", "err", err)
			return 1
		}

		store = sq

		// Found alongside the os.Exit-skips-cleanup findings nearby: the
		// SQLite handle was never closed on ANY exit path, graceful
		// shutdown included — nothing deferred it. Scoped to this branch
		// deliberately: the in-memory-only branch below leaves store a nil
		// engine.Store interface (nopStore is unexported, so main cannot
		// assign one itself), and calling Close on that would panic.
		defer func() {
			if err := sq.Close(); err != nil {
				log.Warn("close storage", "err", err)
			}
		}()

		log.Info("persistence enabled", "driver", "sqlite", "path", cfg.Storage.Path)
	} else {
		log.Info("persistence disabled (in-memory only)")
	}

	// ----- AI advisor (optional LLM detector) -----
	// Installed before the engine so the "ai" detector finds it when the
	// strategy is built. A disabled or misconfigured advisor must never stop the
	// bot: the detector falls back to HOLD.
	aiErr := ai.Configure(cfg.AI)

	switch {
	case aiErr != nil:
		log.Error("ai advisor disabled", "err", aiErr)
	case cfg.AI.Enabled:
		log.Info("ai advisor enabled",
			"provider", cfg.AI.Provider, "model", cfg.AI.Model, "base_url", cfg.AI.BaseURL)
	}

	// cmd/backtest warns about the strategy referencing a detector that can
	// never actually vote (see backtest.ConfigureAI) — a strategy validated
	// there with the advisor enabled can otherwise deploy live strictly
	// weaker with nothing pointing at why. See CODE_REVIEW_FINDINGS.md's
	// "no warning when ai/ml is configured but inert".
	if w := aiInertWarning(cfg, aiErr); w != "" {
		log.Warn(w)
	}

	// The learned "ml" detector. Installed before the engine for the same reason
	// as the advisor: the strategy is built during engine construction.
	ml.Configure(cfg.ML, ml.ParamsFor(cfg.Strategy))

	if cfg.ML.Enabled {
		log.Info("ml detector enabled",
			"horizon", cfg.ML.Horizon, "threshold", cfg.ML.Threshold,
			"train_bars", cfg.ML.TrainBars, "retrain_every", cfg.ML.RetrainEvery,
			"min_skill", cfg.ML.MinSkill)
	}

	if w := mlInertWarning(cfg); w != "" {
		log.Warn(w)
	}

	// ----- wiring -----
	pf := portfolio.New()
	rm := risk.New(cfg.Limits, cfg.Costs)
	eng := engine.New(
		cfg.Engine,
		cfg.Exits,
		cfg.Safety,
		cfg.Strategy,
		provider,
		brk,
		rm,
		pf,
		prices,
		store,
		log,
	)

	defer eng.Close()

	// The paper broker is built before the engine exists, so it starts with the
	// bare tick cache as its price oracle. Upgrade it now to the engine's own
	// ReferencePrice, which also sees the bar history warmup seeded — otherwise
	// every order for a symbol whose live feed never ticks is rejected with
	// "sim: no price for X", once per evaluation, forever. See
	// CODE_REVIEW_FINDINGS.md's TM-1 finding.
	if paperSim != nil {
		paperSim.SetPriceOracle(eng.ReferencePrice)
	}

	// Per-symbol parameter overrides, resolved at config load so each already
	// inherits every field it did not name.
	if len(cfg.SymbolStrategy) > 0 {
		eng.SetSymbolStrategies(cfg.SymbolStrategy)
		log.Info("per-symbol strategy overrides",
			"symbols", cfg.OverriddenSymbols())
	}

	eng.SetHistorySource(history) // must precede the screener and AddSymbols

	// The live-editable blocks go through the same setters the API uses, so the
	// engine retains them and /api/settings reads back what is actually in
	// force rather than what the file said at startup.
	eng.SetProtectionsConfig(cfg.Protections)
	eng.RestoreProtectionState() // an active halt from a previous run must not silently clear
	// Tell the engine which file to write protection changes back to, before
	// any config is applied: drop_after_losses comments a dropped symbol out
	// of watchlist:, and a drop that only changed the running watchlist would
	// be undone by this very startup path on the next restart.
	eng.SetConfigPath(*cfgPath)
	eng.SetPortfolioConfig(cfg.Portfolio, cfg.Sectors, cfg.AssetClasses)

	// Install the screener's probe stage before the screener itself: a probe
	// needs the backtester and the optimizer, which internal/engine cannot
	// reach (internal/autotune imports it, not the other way round). Setting
	// it is harmless when screener.probe is off — the stage reads its own
	// switch live on every call.
	eng.SetScreenProber(autotune.NewScreenProbe(eng, rm, pf, log))
	eng.SetScreenerConfig(cfg.Screener)
	eng.SetAutotuneConfig(cfg.Autotune) // must precede autotune.Schedule below
	eng.SetNotifyConfig(cfg.Notify)

	defer func() { eng.SetNotifyConfig(config.NotifyConfig{}) }() // stop the delivery worker

	// Restore persisted state and merge the persisted watchlist with config.
	persisted, err := eng.Restore()
	if err != nil {
		log.Warn("restore from storage failed", "err", err)
	}

	// Subscribe and warm up everything in one pass, without claiming any of it
	// was hand-picked: Restore has already loaded the pinned set, and a symbol
	// the screener found must stay prunable across a restart.
	symbols := mergeSymbols(cfg.Watchlist, persisted)
	if err := eng.AddScreened(symbols...); err != nil {
		log.Error("failed to add watchlist", "err", err)
		return 1
	}

	// Then pin what config names. Already tracked, so this only records the
	// choice — config is by definition the operator's own list.
	if err := eng.AddSymbols(cfg.Watchlist...); err != nil {
		log.Error("failed to pin configured watchlist", "err", err)
		return 1
	}

	log.Info("watchlist loaded",
		"count", len(symbols), "pinned", len(eng.Pinned()),
		"restored_trades", len(eng.Trades()))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Test mode is on by default: a freshly deployed or reconfigured bot
	// simulates its decisions against the real market for a training window
	// before it can place a real order. Activated after Restore/AddScreened/
	// AddSymbols above, so the seeded book reflects the fully-loaded
	// watchlist/pinned state, and skipped if RestoreProtectionState already
	// found an active halt (EnableTestMode refuses in that case).
	eng.SetTestModeConfig(cfg.TestMode)

	if cfg.TestMode.Enabled {
		dur := cfg.TestMode.Duration
		if dur <= 0 {
			dur = 48 * time.Hour
		}

		if err := eng.EnableTestMode(ctx, dur); err != nil {
			log.Warn("test mode not activated", "err", err)
		} else {
			log.Warn("TEST MODE ACTIVE — all orders are simulated until it ends", "duration", dur)
		}
	}

	// A panic anywhere in the engine's run loop used to crash the entire
	// process — nothing in this codebase called recover() — taking the HTTP
	// API down with it, so an operator couldn't even check open positions
	// after the fact without digging through the process log for a stack
	// trace. This does not (and cannot) make the situation safe: trading has
	// genuinely stopped and positions are genuinely unmanaged either way.
	// What it buys is that the API server survives to say so, rather than
	// the whole process silently vanishing. See CODE_REVIEW_FINDINGS.md's
	// "an unrecovered panic in the engine goroutine crashes the whole
	// process".
	go runSupervised(log, "engine", func() { eng.SetTradingEnabled(false) }, func() { eng.Run(ctx) })

	// Optional, off by default (see config.AutotuneConfig): a scheduled
	// background job that periodically re-runs the Strategy tab's per-symbol
	// optimizer against every eligible watchlist symbol and, unlike that
	// manual button, writes a winning config straight into
	// strategy_overrides and the live engine, fully unsupervised.
	// autotune.Schedule itself is a no-op when cfg.Autotune.Enabled is
	// false, so this is started unconditionally like every other
	// runSupervised goroutine rather than branching at the call site.
	go runSupervised(log, "autotune", nil, func() {
		autotune.Schedule(ctx, eng, rm, pf, cfg.Autotune, *cfgPath, log)
	})

	// No config.Validate() check enforces this (see CODE_REVIEW_FINDINGS.md
	// CRIT-11 — deliberately not made a startup failure, to avoid breaking
	// existing deployments that run this way today): with a live broker, real
	// money trading enabled, and no auth_token, EVERY control endpoint —
	// place orders, disable trading, zero out the money limits — is reachable
	// by anyone who can reach cfg.Server.Addr with no credential at all. Make
	// that impossible to miss in the startup log.
	if cfg.Server.AuthToken == "" && cfg.Engine.TradingEnabled && cfg.BrokerProvider() != "sim" {
		log.Warn(
			"SECURITY: server.auth_token is empty with trading_enabled=true and a live broker — " +
				"every API endpoint (place orders, disable trading, change money limits) is reachable " +
				"by anyone who can reach " + cfg.Server.Addr + " with no credential at all. " +
				"Set server.auth_token (or BOT_AUTH_TOKEN) unless this address is genuinely " +
				"unreachable from anywhere untrusted.",
		)
	}

	srv := api.New(eng, rm, pf, prices, cfg.Server.AuthToken)
	// Settings changed in the web interface are written back to the file they
	// came from, in place: only the edited values change and every comment
	// survives. See config.Patch.
	srv.SetConfigPath(*cfgPath)
	log.Info("HTTP API listening", "addr", cfg.Server.Addr,
		"trading_enabled", cfg.Engine.TradingEnabled,
		"auth_required", cfg.Server.AuthToken != "")

	if err := srv.Run(ctx, cfg.Server.Addr); err != nil {
		log.Error("server error", "err", err)
		return 1
	}

	log.Info("shutdown complete")

	return 0
}

// usesDetector reports whether any of the given canonical/alias names
// appears in either of a strategy's detector sets. Mirrors
// internal/backtest's unexported helper of the same name (see
// backtest/ai.go) — duplicated in a handful of lines rather than
// depending on the backtest package from the live binary, which is not
// otherwise a dependency cmd/bot has any reason to carry.
func usesDetector(s config.StrategyConfig, names ...string) bool {
	match := func(n string) bool {
		return slices.Contains(names, strings.ToLower(strings.TrimSpace(n)))
	}

	return slices.ContainsFunc(s.Detectors, match) ||
		slices.ContainsFunc(s.DetectorsSell, match)
}

// aiInertWarning returns a warning when the strategy's detector set
// references the AI advisor but it cannot actually vote — either it failed
// to configure, or ai.enabled is false — or "" when there is nothing to
// warn about. configureErr is ai.Configure's result.
func aiInertWarning(cfg config.Config, configureErr error) string {
	if !usesDetector(cfg.Strategy, "ai", "llm", "ollama") {
		return ""
	}

	switch {
	case configureErr != nil:
		return `"ai" is in the detector set but the advisor failed to configure` +
			` — it will always vote HOLD`
	case !cfg.AI.Enabled:
		return `"ai" is in the detector set but ai.enabled is false — it will always vote HOLD`
	}

	return ""
}

// mlInertWarning is aiInertWarning's counterpart for the "ml" detector. ML
// has no configure-failure mode to report (ml.Configure cannot fail), only
// the enabled/disabled mismatch.
func mlInertWarning(cfg config.Config) string {
	if !cfg.ML.Enabled && usesDetector(cfg.Strategy, "ml", "model", "classifier") {
		return `"ml" is in the detector set but ml.enabled is false — it will always vote HOLD`
	}

	return ""
}

// runSupervised runs work, recovering any panic so it cannot crash the
// process — logging it, recording it to errlog (surfaced at GET
// /api/errors and the diagnostics panel, so it's visible without reading
// the process log), and calling onPanic (nil-safe) before returning. name
// identifies the supervised work in both the log line and the errlog
// source.
//
// The caller is expected to run this in its own goroutine; the recover has
// to be in the same goroutine as the panic to catch it, which is why this
// takes work as a closure rather than being a defer at the call site.
func runSupervised(log *slog.Logger, name string, onPanic func(), work func()) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}

		log.Error(name+" panicked",
			"panic", r, "stack", string(debug.Stack()))
		errlog.Recordf(name, "panic: %v", r)

		if onPanic != nil {
			onPanic()
		}
	}()

	work()
}
