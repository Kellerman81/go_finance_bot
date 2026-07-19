// Package engine is the heart of the bot: it consumes the live price feed,
// maintains per-symbol candle history, runs the strategy on a fixed cadence,
// sizes orders, passes every order through the risk manager, and submits the
// approved ones to the broker. All state is exposed via thread-safe snapshots
// for the API layer.
package engine

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// barIntervalFor returns the configured bar interval, defaulting to 1 minute.
func barIntervalFor(cfg config.EngineConfig) time.Duration {
	if cfg.BarInterval > 0 {
		return cfg.BarInterval
	}

	return barInterval
}

const (
	barInterval = time.Minute
	// maxBars retains up to a week of 1-minute bars (a US trading week is ~1950
	// minutes, an EU one ~2550), so the trend detector can read the prevailing
	// direction over the whole week rather than the last bar or two.
	maxBars = 3000
	// sellDustQty is the smallest holding worth trying to sell. A residual below
	// this (e.g. left by broker quantity rounding after a full exit) is treated as
	// closed, so a sub-fractional "position" can't trigger an endless sell-retry
	// loop against the broker.
	sellDustQty = 1e-6
	// warmupLookback is how far back to request history on warmup. It spans
	// enough calendar days to pull a full trading week of 1-minute bars
	// (markets are closed nights/weekends, so this reaches ~5 trading days).
	warmupLookback = 9 * 24 * time.Hour
	maxTrades      = 250
)

// TradeRecord captures every trade decision (executed, dry-run or blocked).
type TradeRecord struct {
	Time            time.Time   `json:"time"`
	Symbol          string      `json:"symbol"`
	Side            broker.Side `json:"side"`
	Qty             float64     `json:"qty"`
	Price           float64     `json:"price"`
	Value           float64     `json:"value"`
	Status          string      `json:"status"` // filled | dry-run | blocked | error
	Reason          string      `json:"reason"`
	OrderID         string      `json:"order_id,omitempty"`
	Err             string      `json:"error,omitempty"`
	PnL             *float64    `json:"pnl,omitempty"`              // realized P&L; closing SELL fills only
	PnLPct          *float64    `json:"pnl_pct,omitempty"`          // realized P&L as % of entry cost; closing SELL fills only
	Win             *bool       `json:"win,omitempty"`              // pnl>=0; closing SELL fills only — also the "was this trade realized/closed" marker
	Indicators      string      `json:"indicators,omitempty"`       // JSON []strategy.DetectorResult snapshot at decision time
	EntryIndicators string      `json:"entry_indicators,omitempty"` // closing SELLs only: the original BUY's Indicators snapshot
}

// Status is a high-level engine status snapshot.
type Status struct {
	Running        bool      `json:"running"`
	TradingEnabled bool      `json:"trading_enabled"`
	Broker         string    `json:"broker"`
	Strategy       string    `json:"strategy"`
	Symbols        int       `json:"symbols"`
	SpentToday     float64   `json:"spent_today"`
	LastEval       time.Time `json:"last_eval"`
}

// Engine coordinates feed, strategy, risk and broker.
type Engine struct {
	cfg      config.EngineConfig
	exits    config.ExitsConfig
	safety   config.SafetyConfig
	stratCfg config.StrategyConfig
	provider market.DataProvider
	start    strategy.Strategy
	brk      broker.Broker
	rm       *risk.Manager
	pf       *portfolio.Portfolio
	prices   *PriceCache
	store    Store
	history  HistorySource
	log      *slog.Logger

	// barsStore holds candle history + bar builders behind its own lock, so the
	// per-tick ingest path doesn't contend with evaluation or API reads.
	barsStore *barStore
	met       metrics

	mu             sync.RWMutex
	signals        map[string]strategy.Signal
	watchlist      map[string]struct{}
	peaks          map[string]float64       // high-water mark per symbol for trailing stops
	openPos        map[string]*OpenPosition // durable record of positions opened, not yet closed
	pendingSell    map[string]float64       // symbol -> held qty when its last sell was submitted (dedupe guard)
	streaks        map[string]signalStreak  // consecutive same-action runs (confirmation)
	orderTimes     []time.Time              // recent order submissions (for the rate breaker)
	trades         []TradeRecord
	running        bool
	tradingEnabled bool
	lastEval       time.Time
	lastSync       time.Time // last broker portfolio sync (for throttling)
	lastReconcile  time.Time // last broker open-order reconcile (for throttling)

	// Event subscribers (signal/trade push). Guarded by its own lock so emitting
	// never blocks on the main mutex.
	subsMu    sync.Mutex
	subs      map[int]chan Event
	nextSubID int
}

// New constructs an Engine. The strategy is built from stratCfg and can be
// rebuilt at runtime via SetStrategyConfig.
func New(
	cfg config.EngineConfig,
	exits config.ExitsConfig,
	safety config.SafetyConfig,
	stratCfg config.StrategyConfig,
	p market.DataProvider,
	b broker.Broker,
	rm *risk.Manager,
	pf *portfolio.Portfolio,
	prices *PriceCache,
	store Store,
	log *slog.Logger,
) *Engine {
	if log == nil {
		log = slog.Default()
	}

	if store == nil {
		store = nopStore{}
	}

	return &Engine{
		cfg:            cfg,
		exits:          exits,
		safety:         safety,
		stratCfg:       stratCfg,
		provider:       p,
		start:          strategy.New(stratCfg),
		brk:            b,
		rm:             rm,
		pf:             pf,
		prices:         prices,
		store:          store,
		log:            log.With("component", "engine"),
		barsStore:      newBarStore(maxBars, barIntervalFor(cfg)),
		signals:        make(map[string]strategy.Signal),
		watchlist:      make(map[string]struct{}),
		peaks:          make(map[string]float64),
		openPos:        make(map[string]*OpenPosition),
		pendingSell:    make(map[string]float64),
		streaks:        make(map[string]signalStreak),
		tradingEnabled: cfg.TradingEnabled,
	}
}

// HistorySource supplies historical candles for indicator warmup. The live
// DataProvider satisfies it, but a dedicated source (e.g. the Saxo broker's
// chart endpoint) can be set when the feed lacks usable history.
type HistorySource interface {
	// Candles fetches historical bars for symbol in [from, to] at res.
	Candles(
		ctx context.Context,
		symbol string,
		res market.Resolution,
		from, to time.Time,
	) ([]market.Candle, error)
}

// SetHistorySource installs a dedicated warmup history source, taking priority
// over the live provider. Call before AddSymbols so initial warmup uses it.
func (e *Engine) SetHistorySource(h HistorySource) {
	e.mu.Lock()

	e.history = h
	e.mu.Unlock()
}

// historySource returns the dedicated history source if set, else the provider.
func (e *Engine) historySource() HistorySource {
	e.mu.RLock()

	h := e.history
	e.mu.RUnlock()

	if h != nil {
		return h
	}

	return e.provider
}

// Restore rehydrates persisted state (recent trades, daily spend) from the
// store. The persisted watchlist is returned so the caller can re-subscribe.
func (e *Engine) Restore() (watchlist []string, err error) {
	if trades, err := e.store.RecentTrades(maxTrades); err == nil && len(trades) > 0 {
		e.mu.Lock()

		e.trades = trades
		e.mu.Unlock()
	}

	if day, amount, err := e.store.LoadDailySpend(); err == nil && day != "" {
		e.rm.Restore(day, amount)
	}

	if ops, err := e.store.LoadOpenPositions(); err == nil && len(ops) > 0 {
		e.mu.Lock()

		for i := range ops {
			p := ops[i]

			e.openPos[p.Symbol] = &p
			e.peaks[p.Symbol] = p.Peak // restore trailing-stop high-water mark
		}

		e.mu.Unlock()
		e.log.Info("restored open positions", "count", len(ops))
	}

	return e.store.LoadWatchlist()
}

// AddSymbols subscribes to symbols, warms up candle history, and registers them
// for evaluation.
func (e *Engine) AddSymbols(symbols ...string) error {
	var fresh []string

	e.mu.Lock()

	for _, s := range symbols {
		if s == "" {
			continue
		}

		if _, ok := e.watchlist[s]; ok {
			continue
		}

		e.watchlist[s] = struct{}{}
		fresh = append(fresh, s)
	}

	e.mu.Unlock()

	if len(fresh) == 0 {
		return nil
	}

	for _, s := range fresh {
		e.barsStore.add(s)
	}

	if err := e.provider.Subscribe(fresh...); err != nil {
		return err
	}

	warmupSymbols(context.Background(), fresh, e.warmup)

	if err := e.store.SaveWatchlist(e.Watchlist()); err != nil {
		e.log.Warn("persist watchlist failed", "err", err)
	}

	return nil
}

// RemoveSymbol stops tracking a symbol (it is not auto-liquidated).
func (e *Engine) RemoveSymbol(symbol string) {
	e.mu.Lock()
	delete(e.watchlist, symbol)
	delete(e.signals, symbol)
	delete(e.peaks, symbol)
	e.mu.Unlock()
	e.barsStore.remove(symbol)

	_ = e.provider.Unsubscribe(symbol)
	if err := e.store.SaveWatchlist(e.Watchlist()); err != nil {
		e.log.Warn("persist watchlist failed", "err", err)
	}
}

// warmupConcurrency bounds how many symbols warm up in parallel, so startup is
// not serialized on network round-trips. The upstream history source paces its
// own requests (e.g. the Yahoo rate limiter), so this stays modest.
const warmupConcurrency = 6

// warmupSymbols runs warm for each symbol across a bounded worker pool and
// returns once all have completed.
func warmupSymbols(ctx context.Context, symbols []string, warm func(context.Context, string)) {
	sem := make(chan struct{}, warmupConcurrency)

	var wg sync.WaitGroup

	for _, s := range symbols {
		sem <- struct{}{}

		wg.Go(func() {
			defer func() { <-sem }()

			warm(ctx, s)
		})
	}

	wg.Wait()
}

// warmup seeds a symbol's bar history from the history source (falling back
// to the live provider) so indicators are usable before live bars accrue.
func (e *Engine) warmup(ctx context.Context, symbol string) {
	to := time.Now()
	from := to.Add(-warmupLookback)
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)

	defer cancel()

	src := e.historySource()
	candles, err := src.Candles(cctx, symbol, market.Res1Min, from, to)

	if (err != nil || len(candles) == 0) && e.history != nil {
		// Dedicated history source missed; fall back to the live provider.
		if c, err2 := e.provider.Candles(
			cctx,
			symbol,
			market.Res1Min,
			from,
			to,
		); err2 == nil &&
			len(c) > 0 {
			candles, err = c, nil
		}
	}

	if err != nil || len(candles) == 0 {
		e.log.Debug("no warmup candles; will build from live ticks", "symbol", symbol, "err", err)

		return
	}

	e.log.Info("warmed up from history", "symbol", symbol, "bars", len(candles))
	e.barsStore.seed(symbol, candles)
}

// Run starts the ingest and evaluation loops. It blocks until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	e.mu.Lock()

	e.running = true
	e.mu.Unlock()

	defer func() {
		e.mu.Lock()

		e.running = false
		e.mu.Unlock()
	}()

	e.syncPortfolio(ctx)

	ticker := time.NewTicker(e.cfg.EvalInterval)
	defer ticker.Stop()

	quotes := e.provider.Quotes()

	e.log.Info("engine started", "eval_interval", e.cfg.EvalInterval, "broker", e.brk.Name())

	for {
		select {
		case <-ctx.Done():
			e.log.Info("engine stopping")

			return

		case q, ok := <-quotes:
			if !ok {
				e.log.Warn("quote stream closed")

				quotes = nil
				continue
			}

			e.ingest(q)

		case <-ticker.C:
			e.evaluate(ctx)
		}
	}
}

// ingest applies a live tick to price cache, portfolio mark and bar builder.
func (e *Engine) ingest(q market.Quote) {
	e.prices.Set(q.Symbol, q.Price)
	e.pf.MarkPrice(q.Symbol, q.Price)

	completed := e.barsStore.ingest(q.Symbol, q.Price, q.Time)
	e.met.onTick(completed)
}

// evalConcurrency bounds how many symbols are evaluated in parallel. The
// strategy evaluation is read-only (it copies its series), so this only affects
// throughput, not correctness; acting on the results stays sequential.
const evalConcurrency = 8

// evalResult carries one symbol's evaluation for the sequential act phase.
type evalResult struct {
	sym    string
	sig    strategy.Signal
	series []market.Candle
}

// evaluate runs the strategy for every watched symbol and acts on signals.
func (e *Engine) evaluate(ctx context.Context) {
	start := time.Now()

	e.syncPortfolioIfStale(ctx)

	e.mu.RLock()

	symbols := make([]string, 0, len(e.watchlist))
	for s := range e.watchlist {
		symbols = append(symbols, s)
	}

	tradingEnabled := e.tradingEnabled
	activeStrategy := e.start
	e.mu.RUnlock()

	// Protective exits run first so a triggered stop always beats a strategy
	// signal; reconcile working orders for lifecycle/observability.
	e.checkExits(ctx)
	e.reconcile(ctx)

	// Evaluate every symbol read-only in parallel, then act on the results
	// sequentially so order submission remains serialized and deterministic.
	results := e.evaluateSignals(activeStrategy, symbols)
	for _, r := range results {
		e.setSignal(r.sym, r.sig)

		// Require N consecutive evaluations to agree before acting, so a one-off
		// reading doesn't fire a trade (confirm_buy / confirm_sell). A HOLD breaks
		// the run, so the count must be unbroken.
		streak := e.bumpStreak(r.sym, r.sig.Action)
		if r.sig.Action != strategy.Hold && streak >= e.confirmsNeeded(r.sig.Action) {
			// Re-read the toggle per action: if the circuit breaker trips on one
			// symbol, the remaining signals this cycle become dry-runs instead of
			// re-tripping the breaker and logging "halted" once per symbol.
			e.act(ctx, r.sig, r.series, tradingEnabled && e.tradingOn())
		}
	}

	e.mu.Lock()

	e.lastEval = time.Now()
	e.mu.Unlock()
	e.met.onEval(time.Since(start))
}

// evaluateSignals evaluates the strategy for each symbol concurrently (bounded
// by evalConcurrency) and returns the results in the input order. It is
// read-only: seriesFor copies its data and the strategy holds no state.
func (e *Engine) evaluateSignals(start strategy.Strategy, symbols []string) []evalResult {
	results := make([]evalResult, len(symbols))
	sem := make(chan struct{}, evalConcurrency)

	var wg sync.WaitGroup

	for i, sym := range symbols {
		sem <- struct{}{}

		wg.Go(func() {
			defer func() { <-sem }()

			series := e.seriesFor(sym)
			// Even with no bars yet, evaluate so the symbol shows as "warming up"
			// in /api/signals rather than silently vanishing from the list.
			sig := start.Evaluate(sym, series)
			if len(series) == 0 {
				sig.Reason = "no candle data yet (awaiting feed/warmup)"
			}

			results[i] = evalResult{sym: sym, sig: sig, series: series}
		})
	}

	wg.Wait()

	return results
}

// setSignal stores a symbol's latest signal and emits a signal event when the
// action changes (a new symbol counts as a change).
func (e *Engine) setSignal(sym string, sig strategy.Signal) {
	e.mu.Lock()

	prev, existed := e.signals[sym]

	e.signals[sym] = sig
	e.mu.Unlock()

	if !existed || prev.Action != sig.Action {
		s := sig
		e.emit(Event{Type: EventSignal, Time: time.Now(), Symbol: sym, Signal: &s})
	}
}

// defaultReconcileTTL throttles broker open-order polling when SyncInterval is
// unset.
const defaultReconcileTTL = 15 * time.Second

// reconcileTTL is the minimum gap between working-order polls: the configured
// SyncInterval when set, else the built-in default.
func (e *Engine) reconcileTTL() time.Duration {
	if e.cfg.SyncInterval > 0 {
		return e.cfg.SyncInterval
	}

	return defaultReconcileTTL
}

// reconcile polls the broker's working orders (when supported), records the
// count for metrics, and clears stale pending-sell guards for symbols that no
// longer have a working sell order — the prior sell has settled or been
// cancelled, so the resubmit guard is no longer needed.
func (e *Engine) reconcile(ctx context.Context) {
	lister, ok := e.brk.(broker.OrderLister)
	if !ok {
		return
	}

	e.mu.RLock()

	fresh := time.Since(e.lastReconcile) < e.reconcileTTL()
	e.mu.RUnlock()

	if fresh {
		return
	}

	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	orders, err := lister.OpenOrders(cctx)
	if err != nil {
		e.log.Debug("reconcile: open orders failed", "err", err)

		return
	}

	e.met.setOpenOrders(len(orders))

	workingSell := make(map[string]bool)
	for _, o := range orders {
		if o.Side == broker.Sell {
			workingSell[o.Symbol] = true
		}
	}

	e.mu.Lock()

	e.lastReconcile = time.Now()
	for sym := range e.pendingSell {
		if !workingSell[sym] {
			delete(e.pendingSell, sym)
		}
	}

	e.mu.Unlock()
}

// seriesFor returns completed bars plus the in-progress bar for a symbol.
func (e *Engine) seriesFor(symbol string) []market.Candle {
	return e.barsStore.series(symbol)
}

// SeriesResampled returns the symbol's candle history aggregated up to the given
// resolution (e.g. 5-minute or hourly bars), for multi-timeframe views. The
// engine aggregates ticks at BarInterval; this coarsens that series and never
// upsamples below it.
func (e *Engine) SeriesResampled(symbol string, res market.Resolution) []market.Candle {
	return market.Resample(e.seriesFor(symbol), res)
}

// act sizes an order from a signal, runs it through risk, and (if trading is
// enabled and approved) submits it to the broker. series is the candle window
// the signal was computed from, reused for the indicator snapshot so the
// strategy isn't re-evaluated over a fresh copy.
//
//nolint:cyclop,funlen // gate → size → authorize → submit → record is one order flow
func (e *Engine) act(
	ctx context.Context,
	sig strategy.Signal,
	series []market.Candle,
	tradingEnabled bool,
) {
	// Market-hours gate: only place automated strategy orders when (or shortly
	// before) the symbol's exchange is open, since a reference price from a
	// closed venue is likely stale by the time it reopens. Protective exits
	// bypass this (they are handled in checkExits).
	if e.cfg.MarketHoursCheck {
		if ok, reason := market.OrderingAllowed(sig.Symbol, now(), e.cfg.PreOpenWindow); !ok {
			e.log.Debug(
				"order skipped: market closed",
				"symbol",
				sig.Symbol,
				"side",
				sig.Action,
				"reason",
				reason,
			)

			return
		}
	}

	price, ok := e.prices.Get(sig.Symbol)
	if !ok || price <= 0 {
		price = sig.Price
	}

	if price <= 0 {
		return
	}

	indSnap := e.indicatorSnapshotFrom(series)
	snap := e.pf.Snapshot()

	var (
		order            broker.Order
		sellHeldAtSubmit float64
	)

	switch sig.Action {
	case strategy.Hold:
		return // nothing to act on

	case strategy.Buy:
		// Overlay locally tracked open positions onto the broker snapshot. The
		// broker snapshot only reflects settled fills, so a market order still
		// sitting unfilled (status NEW) would otherwise be invisible both to the
		// pyramid guard (causing repeat buys) and to the risk caps below
		// (max_total_invested / max_per_position / max_open_positions would
		// undercount exposure while a fill is pending).
		snap = e.pendingExposure(snap)
		if !e.cfg.AllowPyramiding && snap.Holdings[sig.Symbol].Qty > 0 {
			return // already in a position (confirmed or pending fill); don't pyramid
		}

		// Confirm the broker venue's price before sizing. The live feed may be a
		// different listing/currency than the broker trades; sizing and risk must
		// use the real execution price. A missing price or a too-large move since
		// the signal blocks the buy.
		confirmed, reason, ok := e.confirmVenuePrice(sig.Symbol, price)
		if !ok {
			e.record(TradeRecord{
				Time: time.Now(), Symbol: sig.Symbol, Side: broker.Buy, Price: price,
				Status: "blocked", Reason: reason, Indicators: indSnap,
			})

			return
		}

		price = confirmed

		qty := e.buyNotional() / price

		order = broker.Order{Symbol: sig.Symbol, Side: broker.Buy, Qty: qty, Type: broker.Market}

	case strategy.Sell:
		held := snap.Holdings[sig.Symbol].Qty
		if held <= sellDustQty {
			e.clearPendingSell(sig.Symbol)

			return // nothing (or only dust) to sell; skip silently
		}

		if pending, ok := e.pendingSellQty(sig.Symbol); ok && held >= pending {
			return // a previous sell for this position hasn't been confirmed by the broker yet
		}

		if !e.heldLongEnough(sig.Symbol) {
			return // within keep_interval; hold unless a protective exit fires
		}

		// Refresh the price from the venue for the risk check; a sell exits the
		// whole position, so don't veto it on deviation.
		if e.cfg.VenuePriceCheck {
			if vp, ok := e.venuePrice(sig.Symbol); ok {
				price = vp
			}
		}

		order = broker.Order{Symbol: sig.Symbol, Side: broker.Sell, Qty: held, Type: broker.Market}
		sellHeldAtSubmit = held

	default:
		e.log.Warn("unknown signal action", "symbol", sig.Symbol, "action", sig.Action)

		return
	}

	authOrder, decision := e.rm.Authorize(order, price, snap)
	if !decision.Approved {
		e.record(TradeRecord{
			Time: time.Now(), Symbol: sig.Symbol, Side: order.Side, Price: price,
			Status: "blocked", Reason: decision.Reason, Indicators: indSnap,
		})

		return
	}

	if !tradingEnabled {
		e.record(TradeRecord{
			Time: time.Now(), Symbol: sig.Symbol, Side: authOrder.Side, Qty: authOrder.Qty,
			Price: price, Value: decision.EstValue, Status: "dry-run",
			Reason: sig.Reason + " | " + decision.Reason, Indicators: indSnap,
		})

		return
	}

	if !e.guardOrderRate(true) {
		return // circuit breaker tripped
	}

	e.met.onOrder()

	res, err := e.brk.SubmitOrder(ctx, authOrder)
	if err != nil {
		e.log.Warn("order submit failed", "symbol", sig.Symbol, "err", err)
		e.record(TradeRecord{
			Time:       time.Now(),
			Symbol:     sig.Symbol,
			Side:       authOrder.Side,
			Qty:        authOrder.Qty,
			Price:      price,
			Status:     "error",
			Reason:     decision.Reason,
			Err:        err.Error(),
			Indicators: indSnap,
		})

		return
	}

	fillPx := res.FilledPx
	if fillPx <= 0 {
		fillPx = price
	}

	value := fillPx * res.FilledQty
	if authOrder.Side == broker.Buy && res.FilledQty > 0 {
		e.rm.RecordBuy(value)
		e.persistSpend()
	}

	var (
		pnlVal, pnlPctVal *float64
		win               *bool
		entryIndicators   string
	)

	if authOrder.Side == broker.Sell {
		if pv, pp, ei, ok := e.closePnL(sig.Symbol, fillPx, res.FilledQty); ok {
			pnlVal, pnlPctVal, win, entryIndicators = &pv, &pp, winFromPnL(pv), ei
		}
	}

	e.record(TradeRecord{
		Time:            time.Now(),
		Symbol:          sig.Symbol,
		Side:            authOrder.Side,
		Qty:             res.FilledQty,
		Price:           fillPx,
		Value:           value,
		Status:          orStr(res.Status, "filled"),
		Reason:          sig.Reason + " | " + decision.Reason,
		OrderID:         res.ID,
		Indicators:      indSnap,
		PnL:             pnlVal,
		PnLPct:          pnlPctVal,
		Win:             win,
		EntryIndicators: entryIndicators,
	})

	// Persist the open/closed position immediately. Use the authorised qty,
	// since async brokers report 0 filled at submit time.
	if authOrder.Side == broker.Buy {
		e.recordOpen(sig.Symbol, authOrder.Qty, fillPx, indSnap)
	} else {
		e.recordClose(sig.Symbol, authOrder.Qty)
		e.markPendingSell(sig.Symbol, sellHeldAtSubmit)
	}

	e.syncPortfolio(ctx)
}

// venuePrice fetches the broker venue's latest price for symbol from the warmup
// history source (which keys on the watchlist symbol, e.g. LXS.DE — the actual
// listing the broker trades), so order sizing reflects the execution venue and
// currency rather than a possibly-divergent live reference feed. It returns the
// last candle's close and whether a usable price was found.
func (e *Engine) venuePrice(symbol string) (float64, bool) {
	src := e.historySource()
	if src == nil {
		return 0, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

	defer cancel()

	to := time.Now()
	from := to.Add(-2 * 24 * time.Hour)
	candles, err := src.Candles(ctx, symbol, market.Res1Min, from, to)

	if err != nil || len(candles) == 0 {
		return 0, false
	}

	last := candles[len(candles)-1].Close
	if last <= 0 {
		return 0, false
	}

	return last, true
}

// confirmVenuePrice validates a BUY's price against the broker venue right
// before ordering. When the check is enabled it re-reads the venue price and
// (a) blocks the order if no price can be confirmed, and (b) blocks it when the
// venue price deviates more than MaxPriceDeviation from the signal price (it
// moved too much since the signal). On success it returns the venue price to
// size and risk-check the order with. When the check is disabled it passes the
// signal price straight through.
func (e *Engine) confirmVenuePrice(
	symbol string,
	signalPrice float64,
) (price float64, reason string, ok bool) {
	if !e.cfg.VenuePriceCheck {
		return signalPrice, "", true
	}

	vp, found := e.venuePrice(symbol)
	if !found {
		return 0, "venue price unavailable — cannot confirm before ordering", false
	}

	if signalPrice > 0 && e.cfg.MaxPriceDeviation > 0 {
		if dev := math.Abs(vp/signalPrice - 1); dev > e.cfg.MaxPriceDeviation {
			return 0, fmt.Sprintf("venue price %.4f deviates %.1f%% from signal %.4f (max %.1f%%)",
				vp, dev*100, signalPrice, e.cfg.MaxPriceDeviation*100), false
		}
	}

	return vp, "", true
}

// ManualOrder executes an explicit user order. It is always passed through the
// risk manager (money limits apply to manual orders too) but bypasses the
// auto-trading toggle, since it is an explicit user action. Returns the
// resulting trade record.
//
//nolint:cyclop,funlen // mirrors act(): authorize → submit → record in one flow
func (e *Engine) ManualOrder(
	ctx context.Context,
	symbol string,
	side broker.Side,
	qty float64,
) (TradeRecord, error) {
	price, ok := e.prices.Get(symbol)
	if !ok || price <= 0 {
		return TradeRecord{}, errNoPrice(symbol)
	}

	indSnap := e.indicatorSnapshot(symbol)
	snap := e.pf.Snapshot()

	if side == broker.Buy {
		snap = e.pendingExposure(snap)
	}

	order := broker.Order{Symbol: symbol, Side: side, Qty: qty, Type: broker.Market}

	authOrder, decision := e.rm.Authorize(order, price, snap)
	if !decision.Approved {
		rec := TradeRecord{
			Time: time.Now(), Symbol: symbol, Side: side, Price: price,
			Status: "blocked", Reason: decision.Reason, Indicators: indSnap,
		}
		e.record(rec)

		return rec, nil
	}

	// Manual orders never trip the breaker (explicit user action) but must
	// count toward the rate window, or a manual burst hides from the guard
	// that protects the broker's session order-rate limit.
	e.guardOrderRate(false)
	e.met.onOrder()

	res, err := e.brk.SubmitOrder(ctx, authOrder)
	if err != nil {
		rec := TradeRecord{
			Time:       time.Now(),
			Symbol:     symbol,
			Side:       side,
			Qty:        authOrder.Qty,
			Price:      price,
			Status:     "error",
			Reason:     decision.Reason,
			Err:        err.Error(),
			Indicators: indSnap,
		}
		e.record(rec)

		return rec, err
	}

	fillPx := res.FilledPx
	if fillPx <= 0 {
		fillPx = price
	}

	value := fillPx * res.FilledQty
	if side == broker.Buy && res.FilledQty > 0 {
		e.rm.RecordBuy(value)
		e.persistSpend()
	}

	var (
		pnlVal, pnlPctVal *float64
		win               *bool
		entryIndicators   string
	)

	if side == broker.Sell {
		if pv, pp, ei, ok := e.closePnL(symbol, fillPx, res.FilledQty); ok {
			pnlVal, pnlPctVal, win, entryIndicators = &pv, &pp, winFromPnL(pv), ei
		}
	}

	rec := TradeRecord{
		Time:            time.Now(),
		Symbol:          symbol,
		Side:            side,
		Qty:             res.FilledQty,
		Price:           fillPx,
		Value:           value,
		Status:          orStr(res.Status, "filled"),
		Reason:          "manual | " + decision.Reason,
		OrderID:         res.ID,
		Indicators:      indSnap,
		PnL:             pnlVal,
		PnLPct:          pnlPctVal,
		Win:             win,
		EntryIndicators: entryIndicators,
	}
	e.record(rec)

	if side == broker.Buy {
		e.recordOpen(symbol, authOrder.Qty, fillPx, indSnap)
	} else {
		e.recordClose(symbol, authOrder.Qty)
	}

	e.syncPortfolio(ctx)

	return rec, nil
}

// record appends a trade decision to the bounded in-memory log, persists it,
// and publishes it to event subscribers.
func (e *Engine) record(t TradeRecord) {
	e.mu.Lock()

	e.trades = append(e.trades, t)
	if len(e.trades) > maxTrades {
		e.trades = e.trades[len(e.trades)-maxTrades:]
	}

	e.mu.Unlock()

	if err := e.store.SaveTrade(t); err != nil {
		e.log.Warn("persist trade failed", "err", err)
	}

	rec := t
	e.emit(Event{Type: EventTrade, Time: t.Time, Symbol: t.Symbol, Trade: &rec})
	e.log.Info("trade", "symbol", t.Symbol, "side", t.Side, "qty", t.Qty,
		"price", t.Price, "status", t.Status, "reason", t.Reason)
}

// guardOrderRate enforces the trade-rate circuit breaker. It records an order
// submission and, for automated orders, trips the breaker (disabling trading)
// when more than safety.MaxOrdersPerMin orders occur in the trailing minute.
// It returns true if the order may proceed.
func (e *Engine) guardOrderRate(automated bool) bool {
	e.mu.Lock()

	cutoff := time.Now().Add(-time.Minute)
	kept := e.orderTimes[:0]

	for _, t := range e.orderTimes {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}

	e.orderTimes = kept

	limit := e.safety.MaxOrdersPerMin
	if automated && limit > 0 && len(e.orderTimes) >= limit {
		e.tradingEnabled = false // trip the breaker
		e.mu.Unlock()
		e.log.Error("trade-rate circuit breaker tripped — automated trading halted",
			"orders_last_min", len(e.orderTimes), "limit", limit)
		e.record(TradeRecord{
			Time: time.Now(), Status: "halted",
			Reason: fmt.Sprintf("circuit breaker: >= %d orders/min — trading disabled", limit),
		})

		return false
	}

	e.orderTimes = append(e.orderTimes, time.Now())
	e.mu.Unlock()

	return true
}

// recordOpen records (or adds to) an open position after a buy fills and
// persists it immediately, so a restart knows the position is still open.
// entryIndicators is the detector snapshot at the moment of THIS buy; it is
// only stored when the position is newly opened — pyramiding onto an
// existing position leaves the original entry's snapshot untouched, since
// that's what should correlate with the eventual outcome.
func (e *Engine) recordOpen(symbol string, qty, price float64, entryIndicators string) {
	if qty <= 0 || price <= 0 {
		return
	}

	e.mu.Lock()
	// A buy means any earlier sell for this symbol has settled; drop a stale
	// pending-sell marker so it can't block a future exit of the new position.
	delete(e.pendingSell, symbol)

	p, ok := e.openPos[symbol]
	if !ok {
		p = &OpenPosition{
			Symbol:          symbol,
			EntryPrice:      price,
			EntryTime:       now(),
			Peak:            price,
			EntryIndicators: entryIndicators,
		}
		e.openPos[symbol] = p
	} else {
		newQty := p.Qty + qty
		if newQty > 0 {
			p.EntryPrice = (p.EntryPrice*p.Qty + price*qty) / newQty // weighted entry
		}
	}

	p.Qty += qty
	if price > p.Peak {
		p.Peak = price
	}

	if price > e.peaks[symbol] {
		e.peaks[symbol] = price
	}

	snap := *p
	e.mu.Unlock()

	if err := e.store.SaveOpenPosition(snap); err != nil {
		e.log.Warn("persist open position failed", "symbol", symbol, "err", err)
	}
}

// recordClose reduces (or closes) an open position after a sell fills and
// persists the change immediately.
func (e *Engine) recordClose(symbol string, qty float64) {
	e.mu.Lock()

	p, ok := e.openPos[symbol]
	if !ok {
		e.mu.Unlock()

		return
	}

	p.Qty -= qty

	closed := p.Qty <= 1e-9

	if closed {
		delete(e.openPos, symbol)
		delete(e.peaks, symbol)
	}

	snap := *p
	e.mu.Unlock()

	var err error

	if closed {
		err = e.store.DeleteOpenPosition(symbol)
	} else {
		err = e.store.SaveOpenPosition(snap)
	}

	if err != nil {
		e.log.Warn("persist open position failed", "symbol", symbol, "err", err)
	}
}

// pendingExposure overlays locally tracked open positions onto snap. The
// broker snapshot only reflects settled fills, so a buy still working at the
// broker (status NEW) is otherwise invisible to the pyramid guard and to the
// risk caps (max_total_invested / max_per_position / max_open_positions),
// which would both undercount real exposure while a fill is pending.
func (e *Engine) pendingExposure(snap portfolio.Snapshot) portfolio.Snapshot {
	e.mu.RLock()

	open := make(map[string]OpenPosition, len(e.openPos))
	for sym, p := range e.openPos {
		open[sym] = *p
	}

	e.mu.RUnlock()

	for sym, p := range open {
		if p.Qty <= 0 {
			continue
		}

		existing, held := snap.Holdings[sym]
		if held && existing.Qty >= p.Qty {
			continue // broker already reflects at least this much
		}

		price := p.EntryPrice
		if cached, ok := e.prices.Get(sym); ok && cached > 0 {
			price = cached
		}

		if held {
			snap.Invested -= existing.MarketValue
		} else {
			snap.OpenPositions++
		}

		mv := price * p.Qty

		snap.Holdings[sym] = portfolio.Holding{
			Symbol: sym, Qty: p.Qty, AvgPrice: p.EntryPrice, LastPrice: price, MarketValue: mv,
		}

		snap.Invested += mv
	}

	return snap
}

// pendingSellQty returns the held quantity recorded when a sell for symbol
// was last submitted, and whether such a record exists.
func (e *Engine) pendingSellQty(symbol string) (float64, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	qty, ok := e.pendingSell[symbol]

	return qty, ok
}

// markPendingSell records the held quantity at the time a sell was submitted,
// so a held-but-unconfirmed sell isn't resubmitted on the next evaluation tick.
func (e *Engine) markPendingSell(symbol string, heldAtSubmit float64) {
	e.mu.Lock()

	e.pendingSell[symbol] = heldAtSubmit
	e.mu.Unlock()
}

// clearPendingSell drops the pending-sell marker for symbol (the broker now
// confirms no position remains, so there is nothing left to guard).
func (e *Engine) clearPendingSell(symbol string) {
	e.mu.Lock()
	delete(e.pendingSell, symbol)
	e.mu.Unlock()
}

// OpenPositions returns the bot's durable open-position records, sorted.
func (e *Engine) OpenPositions() []OpenPosition {
	e.mu.RLock()
	defer e.mu.RUnlock()

	out := make([]OpenPosition, 0, len(e.openPos))
	for _, p := range e.openPos {
		out = append(out, *p)
	}

	slices.SortFunc(out, func(a, b OpenPosition) int { return cmp.Compare(a.Symbol, b.Symbol) })

	return out
}

// persistSpend records the current rolling daily spend after a buy fill.
func (e *Engine) persistSpend() {
	if err := e.store.SaveDailySpend(today(), e.rm.SpentToday()); err != nil {
		e.log.Warn("persist daily spend failed", "err", err)
	}
}

// today returns the current UTC day (YYYY-MM-DD) for daily-spend bookkeeping.
func today() string { return time.Now().UTC().Format("2006-01-02") }

// now returns the current wall-clock time.
func now() time.Time { return time.Now() }

// syncPortfolioIfStale refreshes the portfolio from the broker only if the last
// sync is older than half the eval interval. Order paths call syncPortfolio
// directly (forced) so post-fill state is always fresh; this just avoids a
// redundant double round-trip at the top of every evaluate cycle.
func (e *Engine) syncPortfolioIfStale(ctx context.Context) {
	e.mu.RLock()

	ttl := e.cfg.SyncInterval
	if ttl <= 0 {
		ttl = e.cfg.EvalInterval / 2
	}

	fresh := ttl > 0 && time.Since(e.lastSync) < ttl
	e.mu.RUnlock()

	if fresh {
		return
	}

	e.syncPortfolio(ctx)
}

// syncPortfolio refreshes cash and positions from the broker and marks prices.
func (e *Engine) syncPortfolio(ctx context.Context) {
	defer func() {
		e.mu.Lock()

		e.lastSync = time.Now()
		e.mu.Unlock()
	}()

	acct, err := e.brk.GetAccount(ctx)
	if err != nil {
		e.log.Debug("account sync failed", "err", err)
	} else {
		e.pf.SetCash(acct.Cash)
	}

	positions, err := e.brk.GetPositions(ctx)
	if err != nil {
		e.log.Debug("positions sync failed", "err", err)

		return
	}

	hs := make([]portfolio.Holding, len(positions))
	for i, p := range positions {
		px := p.Current
		if cached, ok := e.prices.Get(p.Symbol); ok {
			px = cached
		}

		hs[i] = portfolio.Holding{
			Symbol: p.Symbol, Qty: p.Qty, AvgPrice: p.AvgPrice, LastPrice: px,
			MarketValue: px * p.Qty, UnrealizedPL: (px - p.AvgPrice) * p.Qty,
		}
	}

	e.pf.SyncPositions(hs)

	// Drop trailing-stop peaks for positions no longer held.
	held := make(map[string]struct{}, len(hs))
	for _, h := range hs {
		held[h.Symbol] = struct{}{}
	}

	e.mu.Lock()

	for sym := range e.peaks {
		if _, ok := held[sym]; !ok {
			delete(e.peaks, sym)
		}
	}

	e.mu.Unlock()
}

// Close releases engine resources (currently the persistence store).
func (e *Engine) Close() error { return e.store.Close() }

// ----- API-facing snapshot accessors -----

// StrategyConfig returns the current strategy configuration.
func (e *Engine) StrategyConfig() config.StrategyConfig {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.stratCfg
}

// SetStrategyConfig rebuilds the strategy from new settings at runtime.
func (e *Engine) SetStrategyConfig(cfg config.StrategyConfig) {
	s := strategy.New(cfg)

	e.mu.Lock()

	e.stratCfg = cfg
	e.start = s
	e.mu.Unlock()
	e.log.Info(
		"strategy updated",
		"detectors",
		cfg.Detectors,
		"combine",
		cfg.Combine,
		"min_strength",
		cfg.MinStrength,
	)
}

// ExitsConfig returns the current protective-exit settings.
func (e *Engine) ExitsConfig() config.ExitsConfig {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.exits
}

// SetExitsConfig updates the protective-exit settings at runtime.
func (e *Engine) SetExitsConfig(x config.ExitsConfig) {
	e.mu.Lock()

	e.exits = x
	e.mu.Unlock()
	e.log.Info(
		"exits updated",
		"stop_loss",
		x.StopLossPct,
		"trailing",
		x.TrailingStopPct,
		"take_profit",
		x.TakeProfitPct,
	)
}

// DetectorSignals returns, for every watched symbol, each available detector's
// current signal (whether or not it is in the active set).
func (e *Engine) DetectorSignals() map[string][]strategy.DetectorResult {
	syms := e.Watchlist()
	cfg := e.StrategyConfig()
	out := make(map[string][]strategy.DetectorResult, len(syms))

	for _, sym := range syms {
		out[sym] = strategy.EvaluateAll(cfg, e.seriesFor(sym))
	}

	return out
}

// signalStreak tracks how many consecutive evaluations produced the same action.
type signalStreak struct {
	action strategy.Action
	count  int
}

// bumpStreak records this evaluation's action and returns the current run
// length of that action (a change of action resets the run to 1).
func (e *Engine) bumpStreak(symbol string, action strategy.Action) int {
	e.mu.Lock()
	defer e.mu.Unlock()

	st := e.streaks[symbol]
	if st.action == action {
		st.count++
	} else {
		st = signalStreak{action: action, count: 1}
	}

	e.streaks[symbol] = st

	return st.count
}

// confirmsNeeded returns how many consecutive confirmations an action requires.
func (e *Engine) confirmsNeeded(action strategy.Action) int {
	e.mu.RLock()

	n := e.cfg.ConfirmBuy
	if action == strategy.Sell {
		n = e.cfg.ConfirmSell
	}

	e.mu.RUnlock()

	if n < 1 {
		return 1
	}

	return n
}

// heldLongEnough reports whether a position has been open at least KeepInterval,
// so a strategy SELL may proceed. Protective exits bypass this entirely.
func (e *Engine) heldLongEnough(symbol string) bool {
	e.mu.RLock()

	keep := e.cfg.KeepInterval
	pos, ok := e.openPos[symbol]
	e.mu.RUnlock()

	if keep <= 0 || !ok {
		return true // disabled, or no entry record => don't block
	}

	return time.Since(pos.EntryTime) >= keep
}

// buyNotional returns the target euro/dollar size for a new buy. With
// MinPositions > 0 each buy is capped at max_total_invested / MinPositions so
// the budget always supports at least that many concurrent positions; otherwise
// the fixed OrderSizeUSD is used.
func (e *Engine) buyNotional() float64 {
	e.mu.RLock()

	minPos := e.cfg.MinPositions
	size := e.cfg.OrderSizeUSD
	e.mu.RUnlock()

	if minPos > 0 {
		if total := e.rm.Limits().MaxTotalInvested; total > 0 {
			slice := total / float64(minPos)
			if size <= 0 || slice < size {
				return slice
			}
		}
	}

	return size
}

// Sizing returns the current order-sizing settings.
func (e *Engine) Sizing() (orderSizeUSD float64, minPositions int) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.cfg.OrderSizeUSD, e.cfg.MinPositions
}

// SetSizing updates order sizing at runtime. minPositions > 0 caps each buy at
// max_total_invested / minPositions; 0 uses orderSizeUSD.
func (e *Engine) SetSizing(orderSizeUSD float64, minPositions int) {
	e.mu.Lock()

	if orderSizeUSD > 0 {
		e.cfg.OrderSizeUSD = orderSizeUSD
	}

	if minPositions >= 0 {
		e.cfg.MinPositions = minPositions
	}

	e.mu.Unlock()
	e.log.Info("sizing updated", "order_size_usd", orderSizeUSD, "min_positions", minPositions)
}

// Behavior returns the holding/confirmation settings.
func (e *Engine) Behavior() (keepInterval time.Duration, confirmBuy, confirmSell int) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.cfg.KeepInterval, e.cfg.ConfirmBuy, e.cfg.ConfirmSell
}

// SetBehavior updates the minimum-hold and confirmation settings at runtime.
// confirmBuy/confirmSell below 1 are clamped to 1; a negative keepInterval is
// ignored.
func (e *Engine) SetBehavior(keepInterval time.Duration, confirmBuy, confirmSell int) {
	e.mu.Lock()

	if keepInterval >= 0 {
		e.cfg.KeepInterval = keepInterval
	}

	if confirmBuy >= 1 {
		e.cfg.ConfirmBuy = confirmBuy
	}

	if confirmSell >= 1 {
		e.cfg.ConfirmSell = confirmSell
	}

	e.mu.Unlock()
	e.log.Info("behavior updated", "keep_interval", keepInterval,
		"confirm_buy", confirmBuy, "confirm_sell", confirmSell)
}

// tradingOn returns the live value of the trading toggle. Order paths that
// captured the toggle at the start of a cycle re-read it before each submit so
// a mid-cycle circuit-breaker trip takes effect immediately.
func (e *Engine) tradingOn() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.tradingEnabled
}

// SetTradingEnabled toggles live order submission at runtime.
func (e *Engine) SetTradingEnabled(on bool) {
	e.mu.Lock()

	e.tradingEnabled = on
	e.mu.Unlock()
	e.log.Info("trading toggled", "enabled", on)
}

// Status returns a high-level status snapshot.
func (e *Engine) Status() Status {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return Status{
		Running:        e.running,
		TradingEnabled: e.tradingEnabled,
		Broker:         e.brk.Name(),
		Strategy:       e.start.Name(),
		Symbols:        len(e.watchlist),
		SpentToday:     e.rm.SpentToday(),
		LastEval:       e.lastEval,
	}
}

// Signals returns the latest signal per symbol, sorted by symbol.
func (e *Engine) Signals() []strategy.Signal {
	e.mu.RLock()
	defer e.mu.RUnlock()

	out := make([]strategy.Signal, 0, len(e.signals))
	for _, s := range e.signals {
		out = append(out, s)
	}

	slices.SortFunc(out, func(a, b strategy.Signal) int { return cmp.Compare(a.Symbol, b.Symbol) })

	return out
}

// Trades returns recent trade records, newest first.
func (e *Engine) Trades() []TradeRecord {
	e.mu.RLock()
	defer e.mu.RUnlock()

	out := make([]TradeRecord, len(e.trades))
	for i, t := range e.trades {
		out[len(e.trades)-1-i] = t
	}

	return out
}

// Watchlist returns the tracked symbols, sorted.
func (e *Engine) Watchlist() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	out := make([]string, 0, len(e.watchlist))
	for s := range e.watchlist {
		out = append(out, s)
	}

	slices.Sort(out)

	return out
}

// errNoPrice reports that no live price has been seen for symbol yet.
func errNoPrice(symbol string) error {
	return fmt.Errorf("no current price for %s yet", symbol)
}

// orStr returns s, or fallback when s is empty.
func orStr(s, fallback string) string {
	if s == "" {
		return fallback
	}

	return s
}
