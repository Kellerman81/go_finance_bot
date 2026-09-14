// Package api exposes the bot over a gin HTTP server: read endpoints for live
// tracking (quotes, signals, positions, trades) and control endpoints for the
// watchlist, money limits and trading toggle.
package api

import (
	"context"
	"embed"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/ai"
	"github.com/Kellerman81/go_finance_bot/internal/autotune"
	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/engine"
	"github.com/Kellerman81/go_finance_bot/internal/errlog"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/ml"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
	"github.com/gin-gonic/gin"
)

//go:embed web/index.html
var webFS embed.FS

// Server holds dependencies for the HTTP API.
type Server struct {
	eng       *engine.Engine
	rm        *risk.Manager
	pf        *portfolio.Portfolio
	prices    *engine.PriceCache
	authToken string // when non-empty, /api requires this bearer token
	// configPath, when set, is the YAML file settings changes are written back
	// to. Empty means changes apply to the running process only.
	configPath string

	// settingsMu serializes every settings-writing handler's apply-then-persist
	// sequence (putSettings, putLimits, putStrategy, putExits, and via noteSave,
	// putSizing/putBehavior). config.Patch's own patchMu (see CRIT-10) already
	// prevents the FILE write from tearing or losing an update, but two
	// concurrent requests to the SAME section can still interleave in memory:
	// each persistSection call reads back "the live value" from the engine
	// AFTER applying, so request A's response and file write can end up
	// reflecting request B's value instead of its own, silently — not file
	// corruption, but a caller told "applied" for settings that are not
	// actually what it asked for. Settings writes are rare and never on a hot
	// path, so serializing them here costs nothing that matters. See
	// CODE_REVIEW_FINDINGS.md's "concurrent PUT /api/settings/:section
	// requests have no serialization at the handler level".
	settingsMu sync.Mutex
}

// New builds an API server. authToken, when non-empty, protects the /api routes.
func New(
	eng *engine.Engine,
	rm *risk.Manager,
	pf *portfolio.Portfolio,
	prices *engine.PriceCache,
	authToken string,
) *Server {
	return &Server{eng: eng, rm: rm, pf: pf, prices: prices, authToken: authToken}
}

// SetConfigPath enables writing settings changes back to the YAML file.
//
// Without it a change applies to the running process only and is lost on
// restart. With it, each applied section is patched into the file in place —
// comments, ordering and everything the settings screen does not own are
// preserved. See config.Patch.
func (s *Server) SetConfigPath(path string) { s.configPath = path }

// persist writes an applied section back to config.yaml, if a path is set.
//
// It runs AFTER the change has been applied and validated in memory, so the
// file can only ever receive settings the engine already accepted. A failure
// here is reported to the operator but does not undo the live change: the bot
// is running the new settings either way, and silently pretending otherwise
// would be worse than saying "applied, but not saved".
func (s *Server) persist(section string, value any) error {
	if s.configPath == "" {
		return nil
	}

	return config.Patch(s.configPath, section, value)
}

// Router builds the gin engine with all routes registered.
func (s *Server) Router() *gin.Engine {
	// gin defaults to DebugMode until something calls SetMode, and nothing in
	// cmd/bot ever did. In debug mode, gin.Recovery dumps the raw request —
	// method, URI and headers — on every panic, and the request URI is where
	// the API token lives: EventSource cannot set an Authorization header, so
	// requireToken also accepts ?token= for the SSE route (only — see
	// sseTokenPaths). One panic during an SSE request used to write the live
	// token to stderr. Guarded on the current mode still
	// being the unset default so it never overrides a caller that explicitly
	// chose a mode (the test suite sets gin.TestMode itself).
	if gin.Mode() == gin.DebugMode {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()
	r.Use(gin.RecoveryWithWriter(tokenScrubbingWriter{gin.DefaultErrorWriter}))

	// Liveness, plus the one fact the UI must know BEFORE it can authenticate:
	// whether a token is required at all. Reporting it is not a disclosure — an
	// unauthenticated caller learns the same thing from any /api request — and
	// withholding it would leave the new UI unable to tell "no token needed"
	// from "token missing", which are opposite situations.
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "auth_required": s.authToken != ""})
	})

	// Web UI (single embedded page, precompressed + ETag-cached).
	r.GET("/", indexAsset.serve)

	s.registerV2(r)

	// /api: gzip JSON responses, cap request body size, block simple
	// cross-origin writes, and require the auth token when one is set.
	api := r.Group("/api")
	api.Use(gzipJSON())
	api.Use(limitRequestBody())
	api.Use(blockSimpleCrossOriginWrites())

	if s.authToken != "" {
		api.Use(requireToken(s.authToken))
	}

	{
		api.GET("/dashboard", s.getDashboard)
		api.GET("/status", s.getStatus)
		api.GET("/quotes", s.getQuotes)
		api.GET("/positions", s.getPositions)
		api.GET("/open", s.getOpenPositions)
		api.DELETE("/open/:symbol", s.forgetOpenPosition)
		api.POST("/open/:symbol/adopt", s.adoptOpenPosition)
		api.GET("/signals", s.getSignals)
		api.GET("/trades", s.getTrades)
		api.GET("/stats", s.getStats)
		api.GET("/metrics", s.getMetrics)
		api.GET("/events", s.streamEvents)

		api.GET("/watchlist", s.getWatchlist)
		api.POST("/watchlist", s.addWatchlist)
		api.DELETE("/watchlist/:symbol", s.removeWatchlist)
		api.POST("/autotune/exclude/:symbol", s.setAutotuneExclusion)
		api.POST("/autotune/run-only/:symbol", s.setAutotuneRunOnly)
		api.POST("/autotune/run", s.runAutotuneNow)

		api.GET("/limits", s.getLimits)
		api.PUT("/limits", s.putLimits)

		api.GET("/strategy", s.getStrategy)
		api.PUT("/strategy", s.putStrategy)
		api.GET("/exits", s.getExits)
		api.PUT("/exits", s.putExits)
		api.GET("/sizing", s.getSizing)
		api.PUT("/sizing", s.putSizing)
		api.GET("/behavior", s.getBehavior)
		api.PUT("/behavior", s.putBehavior)
		api.GET("/detectors", s.getDetectors)
		api.GET("/ai", s.getAI)
		api.GET("/ml", s.getML)
		api.GET("/errors", s.getErrors)
		api.GET("/unconfirmed", s.getUnconfirmed)
		api.DELETE("/unconfirmed/:id", s.clearUnconfirmed)

		api.GET("/chart/:symbol", s.getChart)
		api.GET("/screener", s.getScreener)
		api.POST("/screener/rescreen", s.rescreenNow)
		api.GET("/notify", s.getNotify)

		// Every remaining live-editable block, read in one round-trip and
		// applied one section at a time.
		api.GET("/settings", s.getSettings)
		api.PUT("/settings/:section", s.putSettings)

		api.GET("/protections", s.getProtections)
		api.POST("/protections/resume", s.resumeProtections)
		api.DELETE("/protections/lock/:symbol", s.unlockSymbol)

		api.POST("/trading", s.setTrading)
		api.POST("/testmode/start", s.startTestMode)
		api.POST("/testmode/stop", s.stopTestMode)
		api.POST("/orders", s.postOrder)

		// Native broker orders (bracket / modify / cancel / list).
		api.GET("/orderable", s.getOrderable)
		api.GET("/orders/open", s.getOpenOrders)
		api.POST("/orders/bracket", s.postBracket)
		api.PATCH("/orders/:id", s.modifyOrder)
		api.DELETE("/orders/:id", s.cancelOrders)
	}

	return r
}

// registerV2 mounts the server-rendered interface.
//
// It keeps "/" for the original page, so a regression here never leaves the bot
// unmanageable. /v2/ is the shell, /v2/f/:name renders one screen, /v2/form/*
// accepts the posts, and only the stylesheet and the client script are files.
//
// The shell is deliberately unauthenticated: it carries no data and the client
// needs it to draw the login screen. Everything with data in it sits behind the
// same token as /api.
func (s *Server) registerV2(r *gin.Engine) {
	r.GET("/v2", func(c *gin.Context) { c.Redirect(http.StatusMovedPermanently, v2Prefix) })
	r.GET("/v2/", s.serveV2Shell)
	r.GET("/v2/static/*filepath", s.serveV2Static)

	// The login fragment is chrome, not data — like the shell, it must be
	// reachable with no token. Without this exemption a client that lacks a
	// token 401s fetching it, whose handler shows the login screen by
	// fetching this very fragment, which 401s again: an unbounded retry loop
	// against the same process that has to stay responsive enough to fire
	// exits. It renders nothing but a static form (see the "login" case
	// serveV2Fragment used to hold, now dead code and removed).
	r.GET("/v2/f/login", s.serveV2Login)

	v2 := r.Group("/v2")
	v2.Use(limitRequestBody())

	if s.authToken != "" {
		v2.Use(requireToken(s.authToken))
	}

	v2.GET("/f/:name", s.serveV2Fragment)
	v2.POST("/form/*name", s.handleV2Form)
}

// getDashboard bundles the frequently-polled slices into one response so the UI
// makes a single round-trip per refresh instead of several.
func (s *Server) getDashboard(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":    s.eng.Status(),
		"positions": s.pf.Snapshot(),
		"open":      s.eng.OpenPositions(),
		"signals":   s.eng.Signals(),
		"metrics":   s.eng.Metrics(),
	})
}

// getStatus serves GET /api/status: engine state, broker and spend today.
func (s *Server) getStatus(c *gin.Context) {
	c.JSON(http.StatusOK, s.eng.Status())
}

// getQuotes serves GET /api/quotes: the latest price per symbol.
func (s *Server) getQuotes(c *gin.Context) {
	c.JSON(http.StatusOK, s.prices.Snapshot())
}

// getPositions serves GET /api/positions: the portfolio snapshot.
func (s *Server) getPositions(c *gin.Context) {
	c.JSON(http.StatusOK, s.pf.Snapshot())
}

// getOpenPositions serves GET /api/open: durable open-position records.
func (s *Server) getOpenPositions(c *gin.Context) {
	c.JSON(http.StatusOK, s.eng.OpenPositions())
}

// forgetOpenPosition serves DELETE /api/open/:symbol: drops the bot's record of
// a position that was closed outside the bot, without placing any order.
//
// This is bookkeeping, not trading — it is the counterpart to selling by hand in
// the broker's own app. No realized trade is booked, because the fill price of a
// manual sale is unknown here.
func (s *Server) forgetOpenPosition(c *gin.Context) {
	sym := strings.ToUpper(strings.TrimSpace(c.Param("symbol")))

	if !s.eng.ForgetOpenPosition(sym) {
		c.JSON(http.StatusNotFound, gin.H{"error": "no open-position record for " + sym})

		return
	}

	c.JSON(http.StatusOK, gin.H{"forgot": sym, "open": s.eng.OpenPositions()})
}

// adoptOpenPosition serves POST /api/open/:symbol/adopt: the mirror of
// forgetOpenPosition, for the Reconciliation panel's other kind of drift —
// a symbol the broker holds that the bot has no record of at all ("held,
// not tracked"). See Engine.AdoptHeldPosition for why this matters beyond
// bookkeeping: an untracked holding only gets a percentage stop-loss, not
// the scale-out ladder, the max_hold barrier, or correctly-recorded
// realized P&L.
func (s *Server) adoptOpenPosition(c *gin.Context) {
	sym := strings.ToUpper(strings.TrimSpace(c.Param("symbol")))

	if !s.eng.AdoptHeldPosition(sym) {
		c.JSON(http.StatusNotFound, gin.H{
			"error": sym + " is not held at the broker, or is already tracked",
		})

		return
	}

	c.JSON(http.StatusOK, gin.H{"adopted": sym, "open": s.eng.OpenPositions()})
}

// getSignals serves GET /api/signals: the latest signal per symbol.
func (s *Server) getSignals(c *gin.Context) {
	c.JSON(http.StatusOK, s.eng.Signals())
}

// getTrades serves GET /api/trades: recent trade decisions.
func (s *Server) getTrades(c *gin.Context) {
	c.JSON(http.StatusOK, s.eng.Trades())
}

// getMetrics serves GET /api/metrics: runtime engine counters.
func (s *Server) getMetrics(c *gin.Context) {
	c.JSON(http.StatusOK, s.eng.Metrics())
}

// streamEvents pushes engine events (signal/trade) to the client as
// Server-Sent Events until the client disconnects. Headers are flushed on
// connect so the browser's EventSource fires onopen immediately, and a periodic
// ping keeps the connection alive through idle markets and proxies.
func (s *Server) streamEvents(c *gin.Context) {
	ch, cancel := s.eng.Subscribe(64)
	defer cancel()

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no") // don't let a reverse proxy buffer the stream
	c.Writer.Flush()                    // send headers now, before the first event

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()

	c.Stream(func(io.Writer) bool {
		select {
		case ev, ok := <-ch:
			if !ok {
				return false
			}

			c.SSEvent(string(ev.Type), ev)

			return true

		case <-ping.C:
			c.SSEvent("ping", gin.H{"t": time.Now().Unix()})

			return true

		case <-c.Request.Context().Done():
			return false
		}
	})
}

// getStats serves GET /api/stats: realized win/loss performance for ?period=.
func (s *Server) getStats(c *gin.Context) {
	period := c.DefaultQuery("period", "week")

	stats, err := s.eng.Stats(period)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})

		return
	}

	c.JSON(http.StatusOK, stats)
}

// getWatchlist serves GET /api/watchlist: the tracked symbols.
func (s *Server) getWatchlist(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"symbols": s.eng.Watchlist()})
}

// addWatchlist serves POST /api/watchlist: starts tracking the given symbols.
func (s *Server) addWatchlist(c *gin.Context) {
	var req struct {
		Symbols []string `json:"symbols"`
	}

	if err := c.ShouldBindJSON(&req); err != nil || len(req.Symbols) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provide a non-empty 'symbols' array"})

		return
	}

	syms := make([]string, 0, len(req.Symbols))
	for _, s := range req.Symbols {
		if s = strings.ToUpper(strings.TrimSpace(s)); s != "" {
			syms = append(syms, s)
		}
	}

	if err := s.eng.AddSymbols(syms...); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})

		return
	}

	c.JSON(http.StatusOK, gin.H{"symbols": s.eng.Watchlist()})
}

// removeWatchlist serves DELETE /api/watchlist/:symbol.
func (s *Server) removeWatchlist(c *gin.Context) {
	sym := strings.ToUpper(strings.TrimSpace(c.Param("symbol")))
	s.eng.RemoveSymbol(sym)
	c.JSON(http.StatusOK, gin.H{"symbols": s.eng.Watchlist()})
}

// setAutotuneExclusion serves POST /api/autotune/exclude/:symbol: adds or
// removes one symbol from AutotuneConfig.ExcludeSymbols — the per-symbol
// "exclude from autotune" / "include in autotune" toggle on the Watchlist
// tab, so keeping a hand-tuned symbol (or one currently mid-review) out of
// the scheduler's reach doesn't require opening the Settings tab and
// hand-editing a comma list.
//
// Applies live via the same Engine.SetAutotuneConfig the settings form
// uses, and persists through the same s.noteSave("autotune") path, so this
// button and the Settings tab's own exclude_symbols field can never
// disagree about what is actually in force.
func (s *Server) setAutotuneExclusion(c *gin.Context) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()

	sym := strings.ToUpper(strings.TrimSpace(c.Param("symbol")))

	var req struct {
		Excluded bool `json:"excluded"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	cfg := s.eng.AutotuneConfig()
	cfg.ExcludeSymbols = toggleSymbol(cfg.ExcludeSymbols, sym, req.Excluded)

	// A symbol named in both ExcludeSymbols and RunOnlyForSymbols while
	// autotune is enabled is a config.Load-time error (see
	// AutotuneConfig.Validate) — reject it here too, rather than writing a
	// config.yaml that fails to load on the next restart. Validate is a
	// no-op while disabled, so this can only ever fire for a symbol an
	// operator has ALSO restricted the scheduler to via Settings, which
	// excluding it here would leave self-contradictory either way.
	if err := cfg.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	s.eng.SetAutotuneConfig(cfg)

	note := s.noteSave(c, "autotune")
	c.JSON(http.StatusOK, gin.H{"symbol": sym, "excluded": req.Excluded, "note": note})
}

// setAutotuneRunOnly serves POST /api/autotune/run-only/:symbol: adds or
// removes one symbol from AutotuneConfig.RunOnlyForSymbols — the Watchlist
// tab's per-symbol counterpart to setAutotuneExclusion.
//
// Unlike ExcludeSymbols, this is an ALLOW-list: as soon as it holds even
// one symbol, every OTHER watchlist symbol becomes ineligible too — adding
// the first one is not a "just this one" action the way excluding a symbol
// is. The response reports HowManyRunOnly so the client can warn about that
// side effect rather than the button looking like a harmless per-row flip.
func (s *Server) setAutotuneRunOnly(c *gin.Context) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()

	sym := strings.ToUpper(strings.TrimSpace(c.Param("symbol")))

	var req struct {
		Included bool `json:"included"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	cfg := s.eng.AutotuneConfig()
	cfg.RunOnlyForSymbols = toggleSymbol(cfg.RunOnlyForSymbols, sym, req.Included)

	// Mirrors setAutotuneExclusion's own check: reject a write that would
	// fail AutotuneConfig.Validate() (a symbol in both lists while
	// enabled) rather than produce a config.yaml that breaks the next
	// restart.
	if err := cfg.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	s.eng.SetAutotuneConfig(cfg)

	note := s.noteSave(c, "autotune")
	c.JSON(http.StatusOK, gin.H{
		"symbol": sym, "included": req.Included, "note": note,
		"run_only_count": len(cfg.RunOnlyForSymbols),
	})
}

// runAutotuneNow serves POST /api/autotune/run: triggers one autotune pass
// immediately, using the live AutotuneConfig (bars/min_trades/objective/
// min_return_pct/run_only_for_symbols/exclude_symbols — whatever is
// currently set, live-editable via the Settings tab and the Watchlist
// tab's per-symbol buttons) — regardless of whether autotune.enabled is
// on, since this is a deliberate, operator-initiated run, not the
// unsupervised background job itself.
//
// Runs in the background: a full pass can take up to ~20s per eligible
// symbol, far past any reasonable HTTP timeout, so this returns as soon as
// the run has started, not once it's finished. Progress/results surface
// the same way a scheduled pass's do — errlog on a write failure, a
// KindAudit notification per symbol actually changed — not in this
// response.
func (s *Server) runAutotuneNow(c *gin.Context) {
	if autotune.Busy() {
		c.JSON(http.StatusConflict, gin.H{"error": "a search is already running"})

		return
	}

	cfg := s.eng.AutotuneConfig()

	go func() {
		autotune.RunOnce(context.Background(), s.eng, s.rm, s.pf, cfg, s.configPath, slog.Default())
	}()

	c.JSON(http.StatusOK, gin.H{"started": true})
}

// toggleSymbol adds sym to list (if include and not already present) or
// removes it (if !include and present), case-insensitively, leaving list
// unchanged otherwise. Never mutates the slice backing list — a caller may
// still hold a reference to the config it came from.
func toggleSymbol(list []string, sym string, include bool) []string {
	idx := -1

	for i, s := range list {
		if strings.EqualFold(s, sym) {
			idx = i

			break
		}
	}

	switch {
	case include && idx < 0:
		out := make([]string, 0, len(list)+1)
		out = append(out, list...)

		return append(out, sym)

	case !include && idx >= 0:
		out := make([]string, 0, len(list)-1)
		out = append(out, list[:idx]...)

		return append(out, list[idx+1:]...)

	default:
		return list
	}
}

// getLimits serves GET /api/limits: the money limits plus spend today.
func (s *Server) getLimits(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"limits":      s.rm.Limits(),
		"spent_today": s.rm.SpentToday(),
	})
}

// putLimits serves PUT /api/limits: replaces the money limits live.
func (s *Server) putLimits(c *gin.Context) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()

	var l config.Limits

	if err := c.ShouldBindJSON(&l); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	if err := l.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	// Trading is already live: a zeroed max_order_value/max_total_invested
	// would otherwise disable those caps entirely (risk.Manager treats a
	// cap of 0 as "not enabled", not "unlimited"). A config that ships with
	// trading_enabled: false is validated when it is later turned on (see
	// setTrading); this is the other way limits can change out from under a
	// running bot.
	if s.eng.Status().TradingEnabled {
		if err := l.ValidateForTrading(); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

			return
		}
	}

	s.rm.SetLimits(l)
	note := s.noteSave(c, "limits")
	c.JSON(http.StatusOK, gin.H{"limits": s.rm.Limits(), "note": note})
}

// setTrading serves POST /api/trading: toggles automated trading.
func (s *Server) setTrading(c *gin.Context) {
	var req struct {
		Enabled bool `json:"enabled"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	// Fail closed, live: config.Validate only enforces money ceilings at
	// load time when trading_enabled: true is already in the file. A config
	// shipped with trading_enabled: false (which skips that check) and
	// max_order_value: 0 would otherwise become genuinely unbounded the
	// moment this endpoint turns trading on.
	if req.Enabled {
		if err := s.rm.Limits().ValidateForTrading(); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

			return
		}
	}

	s.eng.SetTradingEnabled(req.Enabled)
	c.JSON(http.StatusOK, gin.H{"trading_enabled": req.Enabled})
}

// startTestMode serves POST /api/testmode/start: begins (or restarts) a
// simulated training window, seeded from the real broker's current account
// snapshot. An optional JSON body {"duration": "6h"} overrides the
// configured default duration for this window only; omitting it (or
// posting no body) falls back to the configured default.
func (s *Server) startTestMode(c *gin.Context) {
	var req struct {
		Duration string `json:"duration"`
	}

	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

			return
		}
	}

	dur := s.eng.TestModeConfig().Duration
	if req.Duration != "" {
		parsed, err := parseDur("duration", req.Duration)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

			return
		}

		dur = parsed
	}

	if err := s.eng.EnableTestMode(c.Request.Context(), dur); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	c.JSON(http.StatusOK, s.eng.TestModeStatus())
}

// stopTestMode serves POST /api/testmode/stop: ends an active training
// window immediately, so every following order reaches the real broker
// again. A no-op (still 200) if no window is active.
func (s *Server) stopTestMode(c *gin.Context) {
	s.eng.EndTestMode("operator stopped")
	c.JSON(http.StatusOK, s.eng.TestModeStatus())
}

// postOrder serves POST /api/orders: a manual, risk-checked market order.
func (s *Server) postOrder(c *gin.Context) {
	var req struct {
		Symbol string  `json:"symbol"`
		Side   string  `json:"side"`
		Qty    float64 `json:"qty"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	sym := strings.ToUpper(strings.TrimSpace(req.Symbol))
	side := broker.Side(strings.ToLower(strings.TrimSpace(req.Side)))

	if sym == "" || (side != broker.Buy && side != broker.Sell) || req.Qty <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "require symbol, side (buy|sell), qty>0"})

		return
	}

	rec, err := s.eng.ManualOrder(c.Request.Context(), sym, side, req.Qty)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "trade": rec})

		return
	}

	c.JSON(http.StatusOK, rec)
}

// getStrategy serves GET /api/strategy: the live strategy configuration.
func (s *Server) getStrategy(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"strategy":  s.eng.StrategyConfig(),
		"available": strings.Join(strategy.AvailableDetectors(), ","),
	})
}

// putStrategy serves PUT /api/strategy: applies a new strategy live.
func (s *Server) putStrategy(c *gin.Context) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()

	var cfg config.StrategyConfig

	if err := c.ShouldBindJSON(&cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	if cfg.FastMA >= cfg.SlowMA && cfg.FastMA > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "fast_ma must be < slow_ma"})

		return
	}

	s.eng.SetStrategyConfig(cfg)
	s.noteSave(c, "strategy") // header only: the response body is the bare struct, unwrapped
	c.JSON(http.StatusOK, s.eng.StrategyConfig())
}

// getExits serves GET /api/exits: the protective-exit levels.
func (s *Server) getExits(c *gin.Context) {
	c.JSON(http.StatusOK, s.eng.ExitsConfig())
}

// getSizing serves GET /api/sizing: order sizing.
func (s *Server) getSizing(c *gin.Context) {
	minPos, maxRisk, minOrderSize := s.eng.Sizing()
	c.JSON(http.StatusOK, gin.H{
		"min_positions":      minPos,
		"max_risk_size_usd":  maxRisk,
		"min_order_size_usd": minOrderSize,
	})
}

// putSizing serves PUT /api/sizing: applies new position sizing live.
func (s *Server) putSizing(c *gin.Context) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()

	var req struct {
		MinPositions    int     `json:"min_positions"`
		MaxRiskSizeUSD  float64 `json:"max_risk_size_usd"`
		MinOrderSizeUSD float64 `json:"min_order_size_usd"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	if req.MinPositions < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "min_positions must be >= 0"})

		return
	}

	if req.MaxRiskSizeUSD < 0 || req.MinOrderSizeUSD < 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "max_risk_size_usd and min_order_size_usd must be >= 0",
		})

		return
	}

	s.eng.SetSizing(req.MinPositions, req.MaxRiskSizeUSD, req.MinOrderSizeUSD)
	note := s.noteSave(c, "sizing")

	minPos, maxRisk, minOrderSize := s.eng.Sizing()
	c.JSON(http.StatusOK, gin.H{
		"min_positions":      minPos,
		"max_risk_size_usd":  maxRisk,
		"min_order_size_usd": minOrderSize,
		"note":               note,
	})
}

// getBehavior serves GET /api/behavior: holding & confirmation settings.
func (s *Server) getBehavior(c *gin.Context) {
	keep, cb, cs := s.eng.Behavior()
	c.JSON(
		http.StatusOK,
		gin.H{"keep_interval": keep.String(), "confirm_buy": cb, "confirm_sell": cs},
	)
}

// putBehavior serves PUT /api/behavior: applies holding & confirmation live.
func (s *Server) putBehavior(c *gin.Context) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()

	var req struct {
		KeepInterval string `json:"keep_interval"`
		ConfirmBuy   int    `json:"confirm_buy"`
		ConfirmSell  int    `json:"confirm_sell"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	keep := time.Duration(0)
	if strings.TrimSpace(req.KeepInterval) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(req.KeepInterval))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "keep_interval: " + err.Error()})

			return
		}

		keep = d
	}

	if keep < 0 || req.ConfirmBuy < 1 || req.ConfirmSell < 1 {
		c.JSON(
			http.StatusBadRequest,
			gin.H{"error": "keep_interval >= 0 and confirm_buy/confirm_sell >= 1 required"},
		)

		return
	}

	s.eng.SetBehavior(keep, req.ConfirmBuy, req.ConfirmSell)
	note := s.noteSave(c, "behavior")

	k, cb, cs := s.eng.Behavior()
	c.JSON(http.StatusOK, gin.H{
		"keep_interval": k.String(), "confirm_buy": cb, "confirm_sell": cs, "note": note,
	})
}

// putExits serves PUT /api/exits: applies new protective-exit levels live.
func (s *Server) putExits(c *gin.Context) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()

	var x config.ExitsConfig

	if err := c.ShouldBindJSON(&x); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	if err := x.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	s.eng.SetExitsConfig(x)
	s.noteSave(c, "exits") // header only: the response body is the bare struct, unwrapped
	c.JSON(http.StatusOK, s.eng.ExitsConfig())
}

// getDetectors serves GET /api/detectors: every detector's signal per symbol.
func (s *Server) getDetectors(c *gin.Context) {
	c.JSON(http.StatusOK, s.eng.DetectorSignals())
}

// getAI serves GET /api/ai: the LLM advisor's configuration and counters, so a
// wrong model name or an unreachable Ollama host is visible without digging
// through logs. Reports enabled:false when no advisor is configured.
func (*Server) getAI(c *gin.Context) {
	c.JSON(http.StatusOK, ai.Default().Stats())
}

// chartTrade is one execution marked on the price chart.
type chartTrade struct {
	Time   time.Time `json:"time"`
	Side   string    `json:"side"`
	Price  float64   `json:"price"`
	Qty    float64   `json:"qty"`
	Status string    `json:"status"`
	Reason string    `json:"reason"`
}

// getChart serves GET /api/chart/:symbol: the recent candles for a symbol plus
// the bot's own executions on it.
//
// Numbers in a table say a strategy sold at 231.40; the chart says whether that
// was the top of a run or the bottom of a dip. Optional ?bars=N bounds how much
// history is returned (default 240, max 1000) and ?res= resamples it (1, 5, 15,
// 60, D) so a longer view fits on one screen.
//
//nolint:cyclop // parse params → fetch → filter → respond; one flat handler
func (s *Server) getChart(c *gin.Context) {
	symbol := strings.ToUpper(strings.TrimSpace(c.Param("symbol")))
	if symbol == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "symbol required"})

		return
	}

	bars := 240
	if v := c.Query("bars"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			bars = min(n, 1000)
		}
	}

	candles := s.eng.SeriesResampled(symbol, market.Resolution(c.DefaultQuery("res", "1")))
	if len(candles) > bars {
		candles = candles[len(candles)-bars:]
	}

	// Only mark trades inside the window on show.
	var from time.Time

	if len(candles) > 0 {
		from = candles[0].Time
	}

	trades := make([]chartTrade, 0)

	for _, t := range s.eng.Trades() {
		if t.Symbol != symbol || t.Price <= 0 || t.Time.Before(from) {
			continue
		}

		trades = append(trades, chartTrade{
			Time: t.Time, Side: string(t.Side), Price: t.Price,
			Qty: t.Qty, Status: t.Status, Reason: t.Reason,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"symbol": symbol, "candles": candles, "trades": trades,
	})
}

// getScreener serves GET /api/screener: the last universe-selection pass,
// including why each rejected candidate was rejected.
func (s *Server) getScreener(c *gin.Context) {
	c.JSON(http.StatusOK, s.eng.LastScreen())
}

// rescreenNow serves POST /api/screener/rescreen: runs one screening pass
// immediately instead of waiting out the interval, so an edited filter can be
// checked at once. It is synchronous — the response carries the resulting
// watchlist rather than an acknowledgement the caller has to poll behind.
func (s *Server) rescreenNow(c *gin.Context) {
	if !s.eng.ScreenerConfig().Enabled {
		c.JSON(http.StatusBadRequest, gin.H{"error": "the screener is disabled"})

		return
	}

	s.eng.RescreenNow(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"screen": s.eng.LastScreen(), "watchlist": s.eng.Watchlist()})
}

// getNotify serves GET /api/notify: which alert sinks are configured and how
// many deliveries have succeeded, failed or been dropped.
func (s *Server) getNotify(c *gin.Context) {
	c.JSON(http.StatusOK, s.eng.NotifyStats())
}

// getProtections serves GET /api/protections: halt state, drawdown against the
// equity peak, and any symbols currently locked out.
func (s *Server) getProtections(c *gin.Context) {
	c.JSON(http.StatusOK, s.eng.Protections().Status())
}

// resumeProtections serves POST /api/protections/resume: clears a halt and
// re-bases the drawdown peak to current equity.
//
// This is deliberately a human action. A protection that timed out on its own
// would let an unattended bot resume into the conditions that tripped it —
// the whole point is that someone looks first.
func (s *Server) resumeProtections(c *gin.Context) {
	p := s.eng.Protections()
	if p == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "protections are not enabled"})

		return
	}

	// A protections halt always turns trading off itself (see
	// Engine.haltFromProtections), so clearing it is the one action an
	// operator expects to bring trading back too — without this, resuming
	// left trading_enabled=false and required a second, separate click on
	// the trading toggle to actually resume anything.
	wasHalted, _ := p.Halted()

	p.Resume(s.pf.Snapshot().Equity)
	s.eng.PersistProtectionState() // so the cleared halt survives a restart, not just the clear itself

	if wasHalted {
		s.eng.SetTradingEnabled(true)
	}

	c.JSON(http.StatusOK, gin.H{
		"resumed": true, "protections": p.Status(),
		"trading_enabled": s.eng.Status().TradingEnabled,
	})
}

// unlockSymbol serves DELETE /api/protections/lock/:symbol: clears one symbol's
// cooldown or stop-guard lock.
func (s *Server) unlockSymbol(c *gin.Context) {
	sym := strings.ToUpper(strings.TrimSpace(c.Param("symbol")))

	s.eng.Protections().Unlock(sym)
	c.JSON(http.StatusOK, gin.H{"unlocked": sym})
}

// getML serves GET /api/ml: the learned detector's state — how many symbols
// have models, how many refits and failures, and the mean skill above the
// majority-class base rate. A mean skill at or below zero means the models have
// found nothing, which is the number to watch.
func (*Server) getML(c *gin.Context) {
	c.JSON(http.StatusOK, ml.Default().Stats())
}

// getErrors serves GET /api/errors: recent upstream API failures.
func (*Server) getErrors(c *gin.Context) {
	c.JSON(http.StatusOK, errlog.Recent())
}

// getUnconfirmed serves GET /api/unconfirmed: orders whose fill outcome the
// broker never confirmed. The engine deliberately does not book these into
// positions or realized P&L — an operator must check the broker directly
// (order history / current positions) and clear each entry once reconciled.
func (s *Server) getUnconfirmed(c *gin.Context) {
	c.JSON(http.StatusOK, s.eng.Unconfirmed())
}

// clearUnconfirmed serves DELETE /api/unconfirmed/:id: marks one unconfirmed
// order as reconciled by hand and removes it from the list.
func (s *Server) clearUnconfirmed(c *gin.Context) {
	id := c.Param("id")
	if !s.eng.ClearUnconfirmed(id) {
		c.JSON(http.StatusNotFound, gin.H{"error": "no unconfirmed order with that id"})

		return
	}

	c.JSON(http.StatusOK, gin.H{"cleared": id})
}

// getOrderable serves GET /api/orderable: per-symbol broker tradability.
func (s *Server) getOrderable(c *gin.Context) {
	res, err := s.eng.CheckOrderable(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	c.JSON(http.StatusOK, res)
}

// getOpenOrders serves GET /api/orders/open: working (unfilled) orders.
func (s *Server) getOpenOrders(c *gin.Context) {
	orders, err := s.eng.OpenOrders(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	c.JSON(http.StatusOK, orders)
}

// postBracket serves POST /api/orders/bracket: a native entry + stop-loss +
// take-profit order (brokers that support it).
func (s *Server) postBracket(c *gin.Context) {
	var req struct {
		Symbol     string  `json:"symbol"`
		Side       string  `json:"side"`
		Qty        float64 `json:"qty"`
		EntryType  string  `json:"entry_type"` // "market" | "limit"
		EntryPrice float64 `json:"entry_price"`
		StopLoss   float64 `json:"stop_loss"`
		TakeProfit float64 `json:"take_profit"`
		Duration   string  `json:"duration"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	side := broker.Side(strings.ToLower(strings.TrimSpace(req.Side)))
	if (side != broker.Buy && side != broker.Sell) || req.Qty <= 0 ||
		strings.TrimSpace(req.Symbol) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "require symbol, side (buy|sell), qty>0"})

		return
	}

	entryType := broker.Market
	if strings.ToLower(req.EntryType) == "limit" {
		entryType = broker.Limit
	}

	res, err := s.eng.PlaceBracket(c.Request.Context(), broker.BracketOrder{
		Symbol:     strings.ToUpper(strings.TrimSpace(req.Symbol)),
		Side:       side,
		Qty:        req.Qty,
		EntryType:  entryType,
		EntryPrice: req.EntryPrice,
		StopLoss:   req.StopLoss,
		TakeProfit: req.TakeProfit,
		Duration:   req.Duration,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	c.JSON(http.StatusOK, res)
}

// modifyOrder serves PATCH /api/orders/:id: changes a working order.
func (s *Server) modifyOrder(c *gin.Context) {
	var req struct {
		Symbol string  `json:"symbol"`
		Side   string  `json:"side"`
		Qty    float64 `json:"qty"`
		Price  float64 `json:"price"`
		Type   string  `json:"type"` // "limit" | "market"
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	sym := strings.ToUpper(strings.TrimSpace(req.Symbol))
	side := broker.Side(strings.ToLower(strings.TrimSpace(req.Side)))

	if sym == "" || (side != broker.Buy && side != broker.Sell) || req.Qty <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "require symbol, side (buy|sell), qty>0"})

		return
	}

	otype := broker.Limit
	if strings.ToLower(req.Type) == "market" {
		otype = broker.Market
	}

	res, err := s.eng.ModifyOrder(c.Request.Context(), broker.OrderModification{
		OrderID: c.Param("id"),
		Symbol:  sym,
		Side:    side,
		Type:    otype,
		Qty:     req.Qty,
		Price:   req.Price,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	c.JSON(http.StatusOK, res)
}

// cancelOrders serves DELETE /api/orders/:id (comma-separated ids allowed).
func (s *Server) cancelOrders(c *gin.Context) {
	ids := strings.Split(c.Param("id"), ",")
	if err := s.eng.CancelOrders(c.Request.Context(), ids...); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	c.JSON(http.StatusOK, gin.H{"cancelled": ids})
}

// Run starts the HTTP server and shuts it down when ctx is cancelled.
func (s *Server) Run(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:    addr,
		Handler: s.Router(),
		// Bound header reads to defend against slow-loris. No ReadTimeout/
		// WriteTimeout: the SSE stream is long-lived and would be killed by them.
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5e9)
		defer cancel()

		return srv.Shutdown(shutCtx)

	case err := <-errCh:
		return err
	}
}
