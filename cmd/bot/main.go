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
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // bundle the IANA tz database so exchange-hours lookups work without OS zoneinfo (e.g. Windows)

	"github.com/Kellerman81/go_finance_bot/internal/api"
	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/engine"
	"github.com/Kellerman81/go_finance_bot/internal/market"
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

// main loads the config, wires feed → strategy → risk → broker → engine → API,
// and runs until the process is interrupted.
//
//nolint:cyclop,funlen // sequential start-up wiring; one step per dependency
func main() {
	cfgPath := flag.String("config", "data/config.yaml", "path to YAML config file")

	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("config error", "err", err)
		os.Exit(1)
	}

	prices := engine.NewPriceCache()

	// ----- market data provider -----
	var provider market.DataProvider

	if cfg.Finnhub.APIKey != "" {
		provider = market.NewFinnhub(
			cfg.Finnhub.APIKey,
			cfg.Finnhub.RESTBase,
			cfg.Finnhub.WSBase,
			log,
		)
		log.Info("using Finnhub live data feed")
	} else {
		provider = market.NewMock(time.Second)

		log.Warn("no FINNHUB_API_KEY set — using offline mock data feed")
	}

	defer provider.Close()

	// ----- broker -----
	var (
		brk     broker.Broker
		history engine.HistorySource
	)

	switch cfg.BrokerProvider() {
	case "saxo":
		tokens, err := saxoTokenSource(cfg)
		if err != nil {
			log.Error("init Saxo auth", "err", err)
			os.Exit(1) //nolint:gocritic // fatal start-up; feed cleanup is moot
		}

		sx, err := broker.NewSaxo(
			cfg.Saxo.BaseURL,
			tokens,
			cfg.Saxo.AccountKey,
			cfg.Saxo.AssetTypes,
		)
		if err != nil {
			log.Error("init Saxo broker", "err", err)
			os.Exit(1)
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
		brk = broker.NewAlpaca(cfg.Alpaca.BaseURL, cfg.Alpaca.KeyID, cfg.Alpaca.SecretKey)
		log.Info("using Alpaca broker", "base_url", cfg.Alpaca.BaseURL, "paper", cfg.Alpaca.Paper)

	default:
		brk = broker.NewSim(cfg.Alpaca.SimCash, prices.Get, cfg.Costs.Commission)
		log.Info("using simulated broker", "starting_cash", cfg.Alpaca.SimCash)
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
			os.Exit(1)
		}

		store = sq

		log.Info("persistence enabled", "driver", "sqlite", "path", cfg.Storage.Path)
	} else {
		log.Info("persistence disabled (in-memory only)")
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

	eng.SetHistorySource(history) // must precede AddSymbols so warmup uses it

	// Restore persisted state and merge the persisted watchlist with config.
	persisted, err := eng.Restore()
	if err != nil {
		log.Warn("restore from storage failed", "err", err)
	}

	symbols := mergeSymbols(cfg.Watchlist, persisted)
	if err := eng.AddSymbols(symbols...); err != nil {
		log.Error("failed to add watchlist", "err", err)
		os.Exit(1)
	}

	log.Info("watchlist loaded", "count", len(symbols), "restored_trades", len(eng.Trades()))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go eng.Run(ctx)

	srv := api.New(eng, rm, pf, prices, cfg.Server.AuthToken)
	log.Info("HTTP API listening", "addr", cfg.Server.Addr,
		"trading_enabled", cfg.Engine.TradingEnabled)

	if err := srv.Run(ctx, cfg.Server.Addr); err != nil {
		log.Error("server error", "err", err)
		os.Exit(1)
	}

	log.Info("shutdown complete")
}
