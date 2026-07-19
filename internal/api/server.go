// Package api exposes the bot over a gin HTTP server: read endpoints for live
// tracking (quotes, signals, positions, trades) and control endpoints for the
// watchlist, money limits and trading toggle.
package api

import (
	"context"
	"embed"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/engine"
	"github.com/Kellerman81/go_finance_bot/internal/errlog"
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

// Router builds the gin engine with all routes registered.
func (s *Server) Router() *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })

	// Web UI (single embedded page, precompressed + ETag-cached).
	r.GET("/", indexAsset.serve)

	// /api: gzip JSON responses, and require the auth token when one is set.
	api := r.Group("/api")
	api.Use(gzipJSON())

	if s.authToken != "" {
		api.Use(requireToken(s.authToken))
	}

	{
		api.GET("/dashboard", s.getDashboard)
		api.GET("/status", s.getStatus)
		api.GET("/quotes", s.getQuotes)
		api.GET("/positions", s.getPositions)
		api.GET("/open", s.getOpenPositions)
		api.GET("/signals", s.getSignals)
		api.GET("/trades", s.getTrades)
		api.GET("/stats", s.getStats)
		api.GET("/metrics", s.getMetrics)
		api.GET("/events", s.streamEvents)

		api.GET("/watchlist", s.getWatchlist)
		api.POST("/watchlist", s.addWatchlist)
		api.DELETE("/watchlist/:symbol", s.removeWatchlist)

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
		api.GET("/errors", s.getErrors)

		api.POST("/trading", s.setTrading)
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

// getLimits serves GET /api/limits: the money limits plus spend today.
func (s *Server) getLimits(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"limits":      s.rm.Limits(),
		"spent_today": s.rm.SpentToday(),
	})
}

// putLimits serves PUT /api/limits: replaces the money limits live.
func (s *Server) putLimits(c *gin.Context) {
	var l config.Limits

	if err := c.ShouldBindJSON(&l); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	if l.MaxOrderValue < 0 || l.MaxTotalInvested < 0 || l.MaxPerPosition < 0 ||
		l.MaxDailySpend < 0 || l.CashReserve < 0 || l.MaxOpenPositions < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "limits must be non-negative"})

		return
	}

	s.rm.SetLimits(l)
	c.JSON(http.StatusOK, gin.H{"limits": s.rm.Limits()})
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

	s.eng.SetTradingEnabled(req.Enabled)
	c.JSON(http.StatusOK, gin.H{"trading_enabled": req.Enabled})
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
	c.JSON(http.StatusOK, s.eng.StrategyConfig())
}

// getExits serves GET /api/exits: the protective-exit levels.
func (s *Server) getExits(c *gin.Context) {
	c.JSON(http.StatusOK, s.eng.ExitsConfig())
}

// getSizing serves GET /api/sizing: order sizing (order_size_usd, min_positions).
func (s *Server) getSizing(c *gin.Context) {
	size, minPos := s.eng.Sizing()
	c.JSON(http.StatusOK, gin.H{"order_size_usd": size, "min_positions": minPos})
}

// putSizing serves PUT /api/sizing: applies new position sizing live.
func (s *Server) putSizing(c *gin.Context) {
	var req struct {
		OrderSizeUSD float64 `json:"order_size_usd"`
		MinPositions int     `json:"min_positions"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	if req.MinPositions < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "min_positions must be >= 0"})

		return
	}

	s.eng.SetSizing(req.OrderSizeUSD, req.MinPositions)

	size, minPos := s.eng.Sizing()
	c.JSON(http.StatusOK, gin.H{"order_size_usd": size, "min_positions": minPos})
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

	k, cb, cs := s.eng.Behavior()
	c.JSON(http.StatusOK, gin.H{"keep_interval": k.String(), "confirm_buy": cb, "confirm_sell": cs})
}

// putExits serves PUT /api/exits: applies new protective-exit levels live.
func (s *Server) putExits(c *gin.Context) {
	var x config.ExitsConfig

	if err := c.ShouldBindJSON(&x); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})

		return
	}

	s.eng.SetExitsConfig(x)
	c.JSON(http.StatusOK, s.eng.ExitsConfig())
}

// getDetectors serves GET /api/detectors: every detector's signal per symbol.
func (s *Server) getDetectors(c *gin.Context) {
	c.JSON(http.StatusOK, s.eng.DetectorSignals())
}

// getErrors serves GET /api/errors: recent upstream API failures.
func (*Server) getErrors(c *gin.Context) {
	c.JSON(http.StatusOK, errlog.Recent())
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

	if strings.TrimSpace(req.Symbol) == "" || req.Qty <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "require symbol and qty>0"})

		return
	}

	otype := broker.Limit
	if strings.ToLower(req.Type) == "market" {
		otype = broker.Market
	}

	res, err := s.eng.ModifyOrder(c.Request.Context(), broker.OrderModification{
		OrderID: c.Param("id"),
		Symbol:  strings.ToUpper(strings.TrimSpace(req.Symbol)),
		Side:    broker.Side(strings.ToLower(strings.TrimSpace(req.Side))),
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
