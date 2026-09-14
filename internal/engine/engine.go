// Package engine is the heart of the bot: it consumes the live price feed,
// maintains per-symbol candle history, runs the strategy on a fixed cadence,
// sizes orders, passes every order through the risk manager, and submits the
// approved ones to the broker. All state is exposed via thread-safe snapshots
// for the API layer.
package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/ai"
	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/errlog"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/ml"
	"github.com/Kellerman81/go_finance_bot/internal/notify"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/protect"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
	"github.com/Kellerman81/go_finance_bot/internal/screener"
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

// resolveTimeout bounds the start-up pass that maps watchlist symbols to broker
// tickers. A variable so tests can shorten it; nothing else writes it.
var resolveTimeout = 30 * time.Second

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
	// StrategyConfig is a JSON snapshot of config.StrategyConfig as resolved
	// for this symbol (StrategyConfigFor — its own strategy_overrides entry if
	// it has one, otherwise the shared config) at the moment this record was
	// created. Filled in centrally by record(), not by individual callers, so
	// every trade — not just some — carries which parameters actually
	// produced it. The point is answering "was this loss a bad config, or a
	// bad symbol under a config that works fine elsewhere?" without having to
	// cross-reference a settings-change timeline by hand.
	StrategyConfig string `json:"strategy_config,omitempty"`
	// Simulated marks a fill made while test mode was active — the order
	// went to the internal Sim, not the real broker. Provenance only: it
	// must NOT be filtered out of Stats()/StatsSince(), since the whole
	// point of test mode is that these count as real track record for
	// internal/autotune's own gating.
	Simulated bool `json:"simulated,omitempty"`
}

// Status is a high-level engine status snapshot.
type Status struct {
	Running          bool      `json:"running"`
	TradingEnabled   bool      `json:"trading_enabled"`
	Broker           string    `json:"broker"`
	Strategy         string    `json:"strategy"`
	Symbols          int       `json:"symbols"`
	SpentToday       float64   `json:"spent_today"`
	LastEval         time.Time `json:"last_eval"`
	TestModeActive   bool      `json:"test_mode_active"`
	TestModeDeadline time.Time `json:"test_mode_deadline,omitzero"`
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
	// prot gates entries on trading outcomes (cooldowns, stop guard, drawdown
	// halt). Nil means no protections, which every call site tolerates.
	prot *protect.Manager
	// notifier delivers trade/halt/error alerts. Nil means none.
	notifier *notify.Notifier
	// screen maintains the traded universe automatically. Nil means the
	// watchlist is whatever config and the API say it is.
	screen     *screener.Screener
	lastScreen screener.Result
	// guard caps concentration across the book (sector, correlation). Nil means
	// no concentration limits.
	guard *risk.PortfolioGuard
	// symStrategies / symStratCfg hold per-symbol parameter overrides. A symbol
	// absent from these uses the shared strategy.
	symStrategies map[string]strategy.Strategy
	symStratCfg   map[string]config.StrategyConfig
	// Live-editable configuration blocks, retained so the API can read back what
	// is in force rather than what the file said at startup.
	protCfg config.ProtectionsConfig
	portCfg config.PortfolioConfig
	sectors map[string]string
	// assetClasses maps symbol -> canonical asset class ("stock", "etf", ...)
	// for portCfg.MaxPctPerAssetClass. Merged from config.yaml's
	// asset_classes: block (always wins) over what resolveNames learned from
	// the provider/broker and persisted — the same two-layer arrangement as
	// sectors.
	assetClasses map[string]string
	screenCfg    config.ScreenerConfig
	notifyCfg    config.NotifyConfig
	// autotuneCfg is read live by internal/autotune's Schedule goroutine on
	// every tick (Enabled/Bars/MinTrades/RunOnlyForSymbols/ExcludeSymbols) —
	// see AutotuneConfig/SetAutotuneConfig. Interval is the one exception,
	// restart-only like ScreenerConfig's own Interval: Schedule's ticker is
	// built once at startup from whatever this held then.
	autotuneCfg config.AutotuneConfig

	// barsStore holds candle history + bar builders behind its own lock, so the
	// per-tick ingest path doesn't contend with evaluation or API reads.
	barsStore *barStore
	met       metrics

	// Scratch space for the per-symbol ATR read in checkExits, behind its own
	// lock so it never contends with evaluation. Reusing it keeps the exit sweep
	// allocation-free once warm; without it every held symbol costs a copy of its
	// whole candle history on every cycle, forever.
	atrMu   sync.Mutex
	atrBars []market.Candle
	atrCols columns

	mu        sync.RWMutex
	signals   map[string]strategy.Signal
	watchlist map[string]struct{}
	// names maps symbol -> company name for the UI. See names.go.
	names map[string]string
	// autotuneApplies maps symbol -> the time internal/autotune last
	// applied a strategy_overrides change to it (scheduled or manual). See
	// autotune_applies.go.
	autotuneApplies map[string]time.Time
	// lossStreaks maps symbol -> how many realized closes in a row it has
	// lost, feeding autotune.on_loss. See losstune.go.
	lossStreaks map[string]int
	// untrackedSince maps symbol -> when the broker first reported holding it
	// with no durable openPos record, for adoptUntrackedHoldings' grace.
	untrackedSince map[string]time.Time
	// prober is the screener's optional backtest/autotune scoring stage,
	// installed by cmd/bot. See screenclass.go.
	prober screener.Prober
	// configPath is the file protections write back to — today only
	// drop_after_losses, which comments a dropped symbol out of watchlist:.
	// Empty means never touch a config file. See dropsymbol.go.
	configPath string
	// lossTune carries symbols whose losing streak has reached the
	// configured counter, for internal/autotune to pick up. Created once in
	// New and never replaced, so it needs no lock. See losstune.go.
	lossTune chan string
	// untradeable are symbols the broker has permanently refused. See
	// untradeable.go — entries only, never exits.
	untradeable map[string]string
	// unsizeable holds the reason a symbol's buy could not be sized, so the
	// refusal is recorded once rather than every eval_interval — see
	// noteUnsizeable.
	unsizeable map[string]string
	// lastOrderErr remembers the most recent order-failure signature per
	// symbol, so an unchanged failure repeating every evaluation is announced
	// once instead of once per cycle. See untradeable.go's announcedOrderError.
	lastOrderErr map[string]string
	// pinned is the subset of watchlist chosen by hand rather than found by the
	// screener. Persisted separately, because after a restart the watchlist
	// alone cannot say who put a symbol there.
	pinned      map[string]struct{}
	peaks       map[string]float64       // high-water mark per symbol for trailing stops
	spreads     map[string]float64       // last measured bid-ask spread per symbol (from screening)
	openPos     map[string]*OpenPosition // durable record of positions opened, not yet closed
	pendingSell map[string]float64       // symbol -> held qty when its last sell was submitted (dedupe guard)
	// driftSince marks when pendingExposure first saw a symbol's broker
	// quantity fall short of what openPos records, so it can tell a fill
	// still settling (seconds) from a position closed outside the bot
	// (indefinite) — see pendingExposure and pendingFillGrace.
	driftSince map[string]time.Time
	// unconfirmed holds orders whose fill outcome the broker did not confirm
	// (see broker.OrderResult.Terminal) — keyed by order ID so more than one
	// can be outstanding for the same symbol. Never booked into openPos or
	// P&L; an operator must reconcile each against the broker and clear it.
	// See reconcile.go.
	unconfirmed    map[string]UnconfirmedOrder
	streaks        map[string]signalStreak // consecutive same-action runs (confirmation)
	orderTimes     []time.Time             // recent order submissions (for the rate breaker)
	trades         []TradeRecord
	running        bool
	tradingEnabled bool
	lastEval       time.Time
	lastSync       time.Time // last broker portfolio sync (for throttling)
	lastReconcile  time.Time // last broker open-order reconcile (for throttling)

	// Test mode: while active, e.brk points at an internal *broker.Sim
	// instead of the real broker, and tradingOn() reports true regardless
	// of tradingEnabled — see testmode.go. testModeRealBrk is nil exactly
	// when testModeActive is false.
	testModeActive   bool
	testModeDeadline time.Time // zero means no auto-expiry
	testModeRealBrk  broker.Broker
	testModeCfg      config.TestModeConfig

	// Event subscribers (signal/trade push). Guarded by its own lock so emitting
	// never blocks on the main mutex.
	subsMu    sync.Mutex
	subs      map[int]chan Event
	nextSubID int

	// fxRates caches, per watchlist symbol, the multiplier from the broker's
	// native quoting currency for that instrument into the broker's preferred
	// currency (1 when unknown or no conversion applies). The live feed and
	// warmup history both report prices in the instrument's native currency,
	// while the broker (post-sync) reports positions/equity/fills already
	// converted; every price the engine touches is converted through this
	// cache at the point it enters the engine (ingest, warmup, venue-price
	// checks) so sizing, risk limits, exits and PnL never mix currencies.
	// Populated by resolveFXRates (start-up) and refreshed from syncPortfolio;
	// deliberately never fetched from the tick path, since a broker/FX call
	// can block for seconds and ingest must not stall on it.
	fxMu     sync.RWMutex
	fxRates  map[string]float64
	fxSyncAt time.Time
}

// New constructs an Engine. The strategy is built from stratCfg and can be
// rebuilt at runtime via SetStrategyConfig.
func New(
	cfg config.EngineConfig,
	exitRules config.ExitsConfig,
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

	// min_positions binds inside the risk manager now (it is the coarse tier
	// of the position-slot rule — see risk.PortfolioGuard.MaxPositionValue),
	// so it has to reach it at construction and not only when the sizing form
	// is next saved. Without this the global split would silently enforce
	// nothing until someone touched the UI.
	rm.SetMinPositions(cfg.MinPositions)

	return &Engine{
		cfg:             cfg,
		exits:           exitRules,
		safety:          safety,
		stratCfg:        stratCfg,
		provider:        p,
		start:           strategy.New(stratCfg),
		brk:             b,
		rm:              rm,
		pf:              pf,
		prices:          prices,
		store:           store,
		log:             log.With("component", "engine"),
		barsStore:       newBarStore(maxBars, barIntervalFor(cfg)),
		signals:         make(map[string]strategy.Signal),
		watchlist:       make(map[string]struct{}),
		pinned:          make(map[string]struct{}),
		untradeable:     make(map[string]string),
		names:           make(map[string]string),
		autotuneApplies: make(map[string]time.Time),
		lossStreaks:     make(map[string]int),
		untrackedSince:  make(map[string]time.Time),
		lossTune:        make(chan string, lossTuneQueue),
		peaks:           make(map[string]float64),
		spreads:         make(map[string]float64),
		openPos:         make(map[string]*OpenPosition),
		pendingSell:     make(map[string]float64),
		unsizeable:      make(map[string]string),
		driftSince:      make(map[string]time.Time),
		unconfirmed:     make(map[string]UnconfirmedOrder),
		streaks:         make(map[string]signalStreak),
		tradingEnabled:  cfg.TradingEnabled,
		fxRates:         make(map[string]float64),
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

// SetProtections installs the outcome-driven protections (cooldowns, stop
// guard, drawdown halt). Passing nil disables them. Call before Run.
func (e *Engine) SetProtections(p *protect.Manager) {
	e.mu.Lock()

	e.prot = p
	e.mu.Unlock()
}

// Protections returns the installed protections manager (nil when disabled), so
// the API can report status and resume a halt.
func (e *Engine) Protections() *protect.Manager {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.prot
}

// SetNotifier installs the outbound alert sink. Passing nil disables alerts.
// Call before Run.
func (e *Engine) SetNotifier(n *notify.Notifier) {
	e.mu.Lock()

	e.notifier = n
	e.mu.Unlock()
}

// notifyOf returns the installed notifier (possibly nil, which is safe to use).
func (e *Engine) notifyOf() *notify.Notifier {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.notifier
}

// Notify sends e through the installed alert sinks, for callers outside this
// package (internal/autotune's scheduled auto-optimizer) that need to raise
// an alert without reaching into engine-internal state. Safe to call
// whether or not a notifier is installed.
func (e *Engine) Notify(evt notify.Event) {
	e.notifyOf().Send(evt)
}

// NotifyStats reports alert delivery counters for the API, so a webhook that
// has been silently failing is visible.
func (e *Engine) NotifyStats() notify.Stats { return e.notifyOf().Stats() }

// SetHistorySource installs a dedicated warmup history source, taking priority
// over the live provider. Call before AddSymbols so initial warmup uses it.
func (e *Engine) SetHistorySource(h HistorySource) {
	e.mu.Lock()

	e.history = h
	e.mu.Unlock()
}

// dedicatedHistorySource returns the history source installed by
// SetHistorySource, or nil when none is set.
func (e *Engine) dedicatedHistorySource() HistorySource {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.history
}

// historySource returns the dedicated history source if set, else the provider.
func (e *Engine) historySource() HistorySource {
	h := e.dedicatedHistorySource()
	if h != nil {
		return h
	}

	return e.provider
}

// Restore rehydrates persisted state (recent trades, daily spend) from the
// store. The persisted watchlist is returned so the caller can re-subscribe.
//
// A load failure here (most concretely: a schema column the running binary
// expects but a legacy database file lacks — see storage.legacyColumns) used
// to be silently swallowed for every call except the last, LoadWatchlist:
// RecentTrades, LoadDailySpend and, worst of all, LoadOpenPositions all only
// acted on err == nil and otherwise said nothing. A LoadOpenPositions
// failure specifically meant the bot could start believing it held no
// positions while the broker showed real holdings, with nothing pointing an
// operator at why. Every failure is now recorded to errlog (surfaced at
// GET /api/errors and the diagnostics panel) so it is visible without
// tailing the process log, without making a storage hiccup fail startup —
// syncPortfolio still reconciles real holdings from the broker shortly
// after, which is the actual safety net; this only makes the gap visible in
// the meantime.
func (e *Engine) Restore() (watchlist []string, err error) {
	if trades, terr := e.store.RecentTrades(maxTrades); terr == nil {
		if len(trades) > 0 {
			e.mu.Lock()

			e.trades = trades
			e.mu.Unlock()
		}
	} else {
		errlog.Recordf("storage", "restore recent trades failed: %v", terr)
	}

	if day, amount, derr := e.store.LoadDailySpend(); derr == nil {
		if day != "" {
			e.rm.Restore(day, amount)
		}
	} else {
		errlog.Recordf("storage", "restore daily spend failed: %v", derr)
	}

	if ops, oerr := e.store.LoadOpenPositions(); oerr == nil {
		if len(ops) > 0 {
			e.mu.Lock()

			for i := range ops {
				p := ops[i]

				e.openPos[p.Symbol] = &p
				e.peaks[p.Symbol] = p.Peak // restore trailing-stop high-water mark
			}

			e.mu.Unlock()
			e.log.Info("restored open positions", "count", len(ops))
		}
	} else {
		e.log.Error("restore open positions failed", "err", oerr)
		errlog.Recordf("storage", "restore open positions failed: %v", oerr)
	}

	e.restorePinned()
	e.restoreNames()
	e.restoreSectors()
	e.restoreAssetClasses()
	e.restoreLossStreaks()
	e.restoreAutotuneApplies()

	return e.store.LoadWatchlist()
}

// restorePinned loads the hand-picked set, which must happen before the caller
// re-adds the watchlist so a symbol the screener found stays unpinned and can
// still be pruned. The config watchlist is pinned separately, by AddSymbols.
func (e *Engine) restorePinned() {
	pins, err := e.store.LoadPinned()
	if err != nil || len(pins) == 0 {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	for _, s := range pins {
		e.pinned[s] = struct{}{}
	}
}

// AddSymbols subscribes to symbols, warms up candle history, and registers them
// for evaluation.
//
// These are symbols chosen by hand — the config watchlist or the API — so they
// are pinned. With screener.keep_manual the screener may then extend the
// watchlist but never prune what the operator picked. Pinning is applied even
// to a symbol that is already tracked, which is how a screener discovery gets
// adopted permanently: add it again by hand.
func (e *Engine) AddSymbols(symbols ...string) error {
	return e.trackSymbols(true, symbols...)
}

// AddScreened tracks symbols the screener discovered. They are not pinned, so a
// later pass may drop them again — that is what makes them temporary.
func (e *Engine) AddScreened(symbols ...string) error {
	return e.trackSymbols(false, symbols...)
}

// trackSymbols is the shared body of AddSymbols and AddScreened. pin marks the
// symbols as manually chosen; it never *un*pins one, so re-discovering a pinned
// symbol by screening cannot demote it.
func (e *Engine) trackSymbols(pin bool, symbols ...string) error {
	var fresh []string

	e.mu.Lock()

	for _, s := range symbols {
		if s == "" {
			continue
		}

		if pin {
			e.pinned[s] = struct{}{}
		}

		if _, ok := e.watchlist[s]; ok {
			continue
		}

		e.watchlist[s] = struct{}{}
		fresh = append(fresh, s)
	}

	e.mu.Unlock()

	if pin {
		e.persistPinned()
	}

	if len(fresh) == 0 {
		return nil
	}

	for _, s := range fresh {
		e.barsStore.add(s)
	}

	if err := e.provider.Subscribe(fresh...); err != nil {
		return err
	}

	e.resolveTickers(fresh)
	e.resolveFXRates(fresh)
	warmupSymbols(context.Background(), fresh, e.warmup)

	// Names are cosmetic, so this must never hold up trading: it runs detached
	// and whatever it has resolved by the time a view renders is what shows.
	go e.resolveNames(context.Background(), fresh)

	if err := e.store.SaveWatchlist(e.Watchlist()); err != nil {
		e.log.Warn("persist watchlist failed", "err", err)
	}

	return nil
}

// Pinned lists the symbols the operator chose by hand, sorted. The screener
// leaves these alone when screener.keep_manual is set.
func (e *Engine) Pinned() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	out := make([]string, 0, len(e.pinned))
	for s := range e.pinned {
		out = append(out, s)
	}

	slices.Sort(out)

	return out
}

// persistPinned saves the pinned set so a restart can still tell an operator's
// pick from a screener discovery. Without it every persisted symbol would look
// manual after a restart and the screener could never prune again.
func (e *Engine) persistPinned() {
	if err := e.store.SavePinned(e.Pinned()); err != nil {
		e.log.Warn("persist pinned watchlist failed", "err", err)
	}
}

// resolveTickers asks the broker for each symbol's tradeable ticker, so that the
// positions it reports come back under the names the bot uses.
//
// This is not a warm cache for its own sake. A broker names instruments its own
// way — Trading 212 lists PRAJL.XC as "PR1Jd" — and it maps its ticker back to
// the bot's symbol from a table filled in only when a symbol is *resolved*,
// which until now happened when an order was placed. That table is in memory, so
// after a restart a position the bot had been managing for weeks came back under
// the broker's name and stopped matching its own record.
//
// The consequences were quiet and specific: no open-position record matched, so
// the ROI ladder and max_hold silently switched off for that position, the
// trailing peak restarted from scratch, and a protective exit would have tried
// to sell a symbol the broker cannot resolve. Resolving up front means the
// mapping exists before the first portfolio sync.
//
// Best-effort by design: a symbol this broker does not offer is still worth
// tracking (signals, charts, a paper position), so a failure is logged, not
// fatal.
func (e *Engine) resolveTickers(symbols []string) {
	sr, ok := e.brk.(broker.SymbolResolver)
	if !ok {
		return
	}

	// Bounded: this runs during start-up, and a broker that is slow or down must
	// delay the bot, not prevent it from starting. The mapping is rebuilt on the
	// next order anyway.
	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()

	for _, s := range symbols {
		if _, err := sr.ResolveSymbol(ctx, s); err != nil {
			e.log.Warn("broker cannot resolve symbol; its positions will not match "+
				"the bot's records", "symbol", s, "err", err)
		}
	}
}

// fxRateFor returns the cached multiplier to convert symbol's native-currency
// price into the engine's preferred currency. It defaults to 1 (no
// conversion) for a symbol not yet resolved — a brief, self-healing gap at
// start-up before resolveFXRates/refreshFXRates first run, not a steady
// state. Never fetches over the network: see the fxRates field doc.
func (e *Engine) fxRateFor(symbol string) float64 {
	e.fxMu.RLock()
	rate, ok := e.fxRates[symbol]
	e.fxMu.RUnlock()

	if !ok || rate == 0 {
		return 1
	}

	return rate
}

// resolveFXRates caches each symbol's native-to-preferred-currency multiplier
// up front, before warmup seeds any candles — so warmup history and every
// live tick after it are priced in the same currency from the first bar.
// Best-effort and bounded like resolveTickers: a broker that doesn't support
// CurrencyResolver, or fails to resolve a given symbol, just leaves that
// symbol at the safe default of 1 (no conversion) rather than blocking
// start-up.
func (e *Engine) resolveFXRates(symbols []string) {
	csr, ok := e.brk.(broker.CurrencyResolver)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()

	for _, s := range symbols {
		_, rate, err := csr.InstrumentCurrency(ctx, s)
		if err != nil {
			e.log.Warn("fx rate resolution failed; prices for this symbol will not be currency-converted",
				"symbol", s, "err", err)

			continue
		}

		e.fxMu.Lock()
		e.fxRates[s] = rate
		e.fxMu.Unlock()
	}
}

// fxRefreshInterval bounds how often refreshFXRates re-resolves the whole
// watchlist. Reference rates move slowly enough that this only needs to be
// accurate to within a display/risk-limit tolerance, not tick-precise —
// matching fxTTL, the broker-side rate cache this ultimately reads through.
const fxRefreshInterval = time.Hour

// refreshFXRates re-resolves the whole watchlist's FX rates, but no more
// often than fxRefreshInterval, so a broker that needs a network round-trip
// per symbol isn't hit every eval cycle. Called from the sync path (never
// from ingest), so a slow broker/FX call here delays the next evaluation,
// not live tick processing.
func (e *Engine) refreshFXRates() {
	if _, ok := e.brk.(broker.CurrencyResolver); !ok {
		return
	}

	e.fxMu.RLock()
	stale := time.Since(e.fxSyncAt) >= fxRefreshInterval
	e.fxMu.RUnlock()

	if !stale {
		return
	}

	e.fxMu.Lock()
	e.fxSyncAt = time.Now()
	e.fxMu.Unlock()

	e.resolveFXRates(e.Watchlist())
}

// RemoveSymbol stops tracking a symbol by hand (it is not auto-liquidated).
//
// Because this is a deliberate operator choice, it also un-pins the symbol.
// An automated screener drop must NOT do the same — see dropFromScreen —
// or enabling screener.keep_manual afterward could never recover a symbol
// the screener already dropped before the protection was turned on. See
// CODE_REVIEW_FINDINGS.md's "an automated (screener-driven) drop is
// recorded identically to a manual un-pin".
func (e *Engine) RemoveSymbol(symbol string) {
	e.removeSymbol(symbol, true)
}

// dropFromScreen stops tracking a symbol because the screener no longer
// selects it. Unlike RemoveSymbol, it leaves the symbol's pinned status
// untouched: if the operator later enables screener.keep_manual, a pinned
// symbol dropped this way is picked back up on the very next screening pass
// — screener.Screen's KeepManual handling re-adds anything still in
// Inputs.Pinned via appendMissing regardless of whether it currently
// passes the filters or is even in the candidate pool. Sharing RemoveSymbol
// (which unconditionally cleared the pin) used to mean the protection had
// nothing left to recover once enabled after the fact.
func (e *Engine) dropFromScreen(symbol string) {
	e.removeSymbol(symbol, false)
}

// removeSymbol is RemoveSymbol/dropFromScreen's shared implementation.
// unpin controls whether the symbol's pinned status is cleared along with
// everything else.
func (e *Engine) removeSymbol(symbol string, unpin bool) {
	e.mu.Lock()
	delete(e.watchlist, symbol)

	if unpin {
		delete(e.pinned, symbol)
	}

	delete(e.signals, symbol)
	delete(e.peaks, symbol)
	e.mu.Unlock()
	e.persistPinned()
	e.barsStore.remove(symbol)
	ai.Default().Forget(symbol) // drop any cached LLM verdict for it
	ml.Default().Forget(symbol) // and its trained model

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

	dedicated := e.dedicatedHistorySource()

	src := e.historySource()
	candles, err := src.Candles(cctx, symbol, market.Res1Min, from, to)

	if (err != nil || len(candles) == 0) && dedicated != nil {
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

	// History candles arrive in the instrument's native currency, same as a
	// live tick; convert them the same way (resolveFXRates runs before
	// warmup, so the rate is already cached) so the seeded series and the
	// live bars appended after it are on one consistent scale — otherwise
	// the first live tick would look like a ~1/rate price gap to indicators.
	if rate := e.fxRateFor(symbol); rate != 1 {
		for i := range candles {
			candles[i].Open *= rate
			candles[i].High *= rate
			candles[i].Low *= rate
			candles[i].Close *= rate
		}
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

	// The screener re-derives the traded universe on its own, much slower clock.
	// A stopped ticker is the "disabled" case, so the select arm below is always
	// valid but simply never fires.
	screenTick := &time.Ticker{}

	if iv := e.screener().Interval(); iv > 0 {
		st := time.NewTicker(iv)
		defer st.Stop()

		screenTick = st

		go e.rescreen(ctx) // an initial pass, so startup doesn't wait a full interval
	}

	quotes := e.provider.Quotes()

	e.log.Info("engine started", "eval_interval", e.cfg.EvalInterval, "broker", e.brk.Name())

	for {
		select {
		case <-ctx.Done():
			e.log.Info("engine stopping")

			return

		case q, ok := <-quotes:
			if !ok {
				// The provider's quote channel is closed for good — every price
				// the engine holds is now frozen and will never update again
				// (nothing else feeds e.prices). Trading on it is worse than
				// not trading at all, so this halts automated trading exactly
				// like a protection would, loudly, rather than silently
				// continuing to evaluate/order against stale data forever. See
				// CODE_REVIEW_FINDINGS.md's closed-quote-stream finding.
				e.log.Error("quote stream closed — automated trading halted; prices will not update again without a restart")

				e.tripBreaker("market data feed's quote stream closed")

				e.record(TradeRecord{
					Time: time.Now(), Status: "halted",
					Reason: "market data feed's quote stream closed — trading disabled until restarted",
				})

				quotes = nil
				continue
			}

			e.ingest(q)

		case <-ticker.C:
			e.evaluate(ctx)

		case <-screenTick.C:
			e.rescreen(ctx)
		}
	}
}

// ingest applies a live tick to price cache, portfolio mark and bar builder.
//
// q.Price arrives in the instrument's native trading currency; it is
// converted to the engine's preferred currency here, once, so every
// consumer of e.prices/the bar store/portfolio marks downstream (sizing,
// risk limits, exits, PnL) can treat prices as already consistent with the
// broker-reported (also-converted) equity and cash — see the fxRates field
// doc for why this can't be a network call.
func (e *Engine) ingest(q market.Quote) {
	price := q.Price * e.fxRateFor(q.Symbol)

	e.prices.Set(q.Symbol, price)
	e.pf.MarkPrice(q.Symbol, price)

	completed := e.barsStore.ingest(q.Symbol, price, q.Volume, q.Time)
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

	e.checkTestModeExpiry(ctx)
	e.syncPortfolioIfStale(ctx)

	e.mu.RLock()

	symbols := make([]string, 0, len(e.watchlist))
	for s := range e.watchlist {
		symbols = append(symbols, s)
	}

	// testModeActive folds in here (not just in tradingOn(), re-read per
	// action below) so both halves of the "tradingEnabled && e.tradingOn()"
	// check a few lines down agree — this is the once-per-cycle capture,
	// tradingOn() is the per-action re-check; a bare e.tradingEnabled read
	// here would disagree with tradingOn() and silently defeat test mode.
	tradingEnabled := e.tradingEnabled || e.testModeActive
	e.mu.RUnlock()

	// Protective exits run first so a triggered stop always beats a strategy
	// signal; reconcile working orders for lifecycle/observability.
	e.checkExits(ctx)
	e.reconcile(ctx)

	// Evaluate every symbol read-only in parallel, then act on the results
	// sequentially so order submission remains serialized and deterministic.
	results := e.evaluateSignals(e.evaluable(symbols))
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

	// After the signals, so require_signal sees this cycle's opinion rather than
	// the previous one's.
	e.checkScaleIn(ctx)

	e.mu.Lock()

	e.lastEval = time.Now()
	e.mu.Unlock()
	e.met.onEval(time.Since(start))
}

// evaluateSignals evaluates each symbol concurrently (bounded by
// evalConcurrency) and returns the results in the input order. It is read-only:
// seriesFor copies its data and the strategies hold no state.
//
// The strategy is resolved per symbol, so a symbol with its own parameters is
// evaluated with them.
// evaluable narrows the watchlist to the symbols worth forming an opinion on.
//
// With engine.skip_full_slots set and max_open_positions already reached, a BUY
// on an unheld symbol cannot be authorized, so the work of deciding on one is
// spent to be refused — and that work includes LLM advisor calls and ML
// inference, which cost money and quota rather than microseconds. Held symbols
// are always kept: they still need sell signals, and their protective exits run
// in checkExits either way.
func (e *Engine) evaluable(symbols []string) []string {
	e.mu.RLock()

	skip := e.cfg.SkipFullSlots
	e.mu.RUnlock()

	limit := e.rm.Limits().MaxOpenPositions
	if !skip || limit <= 0 {
		return symbols
	}

	snap := e.pf.Snapshot()
	if snap.OpenPositions < limit {
		return symbols
	}

	out := make([]string, 0, len(symbols))

	var excluded []string

	for _, s := range symbols {
		if h, held := snap.Holding(s); held && h.Qty > 0 {
			out = append(out, s)
		} else {
			excluded = append(excluded, s)
		}
	}

	// A symbol skipped here is not evaluated this cycle, so bumpStreak is
	// never called for it — its confirmation streak was otherwise left
	// exactly where it stood, frozen rather than reset, for however long
	// the slot stays full. The first evaluation after a slot frees up could
	// then immediately extend a streak that started hours (or longer)
	// earlier, firing a trade off a "confirmation" that was never actually
	// consecutive in any meaningful sense — confirm_buy/confirm_sell exist
	// to require N *recent, unbroken* agreements, not "some agreements,
	// arbitrarily long ago, plus one more now". Resetting on exclusion
	// makes a symbol that re-enters evaluation start counting fresh, the
	// same as a symbol whose signal flipped or went to HOLD. See
	// CODE_REVIEW_FINDINGS.md's "skip_full_slots freezes (not resets)
	// confirmation streaks".
	if len(excluded) > 0 {
		e.mu.Lock()

		for _, s := range excluded {
			delete(e.streaks, s)
		}

		e.mu.Unlock()
	}

	return out
}

func (e *Engine) evaluateSignals(symbols []string) []evalResult {
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
			sig := e.strategyFor(sym).Evaluate(sym, series)
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

// Series returns symbol's candle history exactly as the engine aggregates and
// evaluates it — at cfg.Engine.BarInterval, with no resampling — for a caller
// that wants to backtest against precisely what the live strategy sees (e.g.
// internal/autotune). Resampling to a fixed "1" resolution instead (as this
// package used to) silently diverges from a configured sub-minute
// BarInterval; this is exact by construction, whatever BarInterval is set to.
func (e *Engine) Series(symbol string) []market.Candle {
	return e.seriesFor(symbol)
}

// SeriesResampled returns the symbol's candle history aggregated up to the given
// resolution (e.g. 5-minute or hourly bars), for multi-timeframe views. The
// engine aggregates ticks at BarInterval; this coarsens that series and never
// upsamples below it.
func (e *Engine) SeriesResampled(symbol string, res market.Resolution) []market.Candle {
	return market.Resample(e.seriesFor(symbol), res)
}

// marketOpen reports whether an order for symbol may be placed now, preferring
// the broker's real exchange schedule (holidays/breaks included) when it can
// answer, and falling back to the suffix-inferred calendar otherwise.
func (e *Engine) marketOpen(symbol string) (bool, string) {
	if sched, ok := e.brk.(broker.MarketScheduler); ok {
		if open, known := sched.TradingOpen(symbol, now(), e.cfg.PreOpenWindow); known {
			if !open {
				return false, "broker schedule: venue closed"
			}

			return true, ""
		}
	}

	return market.OrderingAllowed(symbol, now(), e.cfg.PreOpenWindow)
}

// entryAllowed answers the market-hours question for a BUY, which is not the
// same question an exit asks.
//
// marketOpen trusts the broker's schedule over the built-in calendar, because
// the broker knows about holidays and mid-session breaks that a suffix-inferred
// calendar cannot. That is the right trade for an exit: when the two disagree,
// acting is safer than being stuck holding something.
//
// For an entry it is the wrong trade. Nothing is lost by declining to open a
// position, and an order sent to a venue that is actually shut is rejected,
// logged, and retried on the next evaluation. So an entry requires BOTH to say
// open, and a disagreement is reported rather than silently resolved — an
// after-hours buy that the calendar would have blocked is the symptom that
// sent us looking here.
func (e *Engine) entryAllowed(symbol string) (bool, string) {
	brokerOK, why := e.marketOpen(symbol)
	if !brokerOK {
		return false, why
	}

	// A venue nothing can name is not gated by either calendar — both fail
	// open, on purpose, so an exit is never stuck. An entry can afford the
	// stricter reading of the one thing that needs no calendar at all.
	if _, known := market.ExchangeFor(symbol); !known && market.IsWeekendUTC(now()) {
		return false, "unknown venue: weekend (UTC)"
	}

	if calOK, calWhy := market.OrderingAllowed(symbol, now(), e.cfg.PreOpenWindow); !calOK {
		e.log.Warn("broker says the venue is open but the exchange calendar "+
			"disagrees; declining the entry",
			"symbol", symbol, "calendar", calWhy)

		return false, calWhy
	}

	return true, ""
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
	// The broker has already refused this instrument outright. Retrying cannot
	// change that, so don't spend an API call and a log line on it every
	// evaluation. Entries only — a sell is never blocked here.
	if sig.Action == strategy.Buy {
		if blocked, why := e.entryBlocked(sig.Symbol); blocked {
			e.log.Debug("entry skipped: broker refuses this instrument",
				"symbol", sig.Symbol, "reason", why)

			return
		}
	}

	// Market-hours gate: only place automated strategy orders when (or shortly
	// before) the symbol's exchange is open, since a reference price from a
	// closed venue is likely stale by the time it reopens. Protective exits
	// apply the same gate themselves, in exitPosition.
	if e.cfg.MarketHoursCheck {
		if ok, reason := e.entryAllowed(sig.Symbol); !ok {
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

	indSnap := e.indicatorSnapshotFrom(sig.Symbol, series)
	snap := e.pf.Snapshot()

	var (
		order            broker.Order
		sellHeldAtSubmit float64
	)

	switch sig.Action {
	case strategy.Hold:
		return // nothing to act on

	case strategy.Buy:
		// A prior buy for this symbol is still unconfirmed by the broker (see
		// flagUnconfirmed) — pendingExposure can't see it (it isn't booked into
		// openPos precisely because it's unconfirmed), so without this check the
		// pyramid guard below would not catch a genuine double-buy risk.
		if e.hasUnconfirmedBuy(sig.Symbol) {
			return
		}

		// Overlay locally tracked open positions onto the broker snapshot. The
		// broker snapshot only reflects settled fills, so a market order still
		// sitting unfilled (status NEW) would otherwise be invisible both to the
		// pyramid guard (causing repeat buys) and to the risk caps below
		// (max_total_invested / max_per_position / max_open_positions would
		// undercount exposure while a fill is pending).
		snap = e.pendingExposure(snap)
		if cur, _ := snap.Holding(sig.Symbol); !e.EngineConfig().AllowPyramiding && cur.Qty > 0 {
			return // already in a position (confirmed or pending fill); don't pyramid
		}

		// Protections gate entries only — a cooldown or a halt must never stop
		// the bot exiting a position it already holds.
		if ok, reason := e.Protections().AllowEntry(sig.Symbol); !ok {
			e.record(TradeRecord{
				Time: time.Now(), Symbol: sig.Symbol, Side: broker.Buy, Price: price,
				Status: "blocked", Reason: reason, Indicators: indSnap,
			})

			return
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

		size := e.sizeBuy(sig.Symbol, price)
		if !size.OK {
			e.noteUnsizeable(sig.Symbol, size.Reason, price, indSnap)

			return
		}

		e.clearUnsizeable(sig.Symbol)

		if size.Note != "" {
			sig.Reason += " | " + size.Note
		}

		// Concentration caps: is this order actually a bet we already have on?
		//
		// Checked with the REAL order value, which is why it runs here rather
		// than before sizing. It used to be handed buyNotional(), which is 0
		// whenever order_size_usd and min_positions are both unset — and
		// checkAssetClass compares value+addValue against the budget, so a
		// zero addValue degraded the class cap to "is this class already
		// over?" and let the order that crossed the line through.
		if ok, reason := e.portfolioGuard().AllowEntry(
			sig.Symbol, snap, size.Notional,
		); !ok {
			e.record(TradeRecord{
				Time: time.Now(), Symbol: sig.Symbol, Side: broker.Buy, Price: price,
				Status: "blocked", Reason: reason, Indicators: indSnap,
			})

			return
		}

		order = broker.Order{
			Symbol: sig.Symbol, Side: broker.Buy,
			Qty: size.Notional / price, Type: broker.Market,
		}

	case strategy.Sell:
		curHold, _ := snap.Holding(sig.Symbol)
		held := curHold.Qty
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
			if vp, _, ok := e.venuePrice(sig.Symbol); ok {
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

	// How the order reaches the venue: market or limit, in one clip or several.
	plan := planExecution(e.executionConfig(), authOrder, price, false)

	res, filledQty, unconfirmedQty, resting, err := e.submitPlan(ctx, plan, sig.Symbol, price)
	if err != nil {
		e.noteUntradeable(sig.Symbol, authOrder.Side, err)
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

	if plan.note != "" {
		decision.Reason += " | " + plan.note
	}

	if authOrder.Side == broker.Sell {
		// Guard against a duplicate sell next cycle regardless of whether this
		// one's outcome is confirmed yet — the dedupe guard exists precisely
		// for the in-flight/unconfirmed window.
		e.markPendingSell(sig.Symbol, sellHeldAtSubmit)
	}

	if unconfirmedQty > 0 {
		e.flagUnconfirmed(sig.Symbol, authOrder.Side, unconfirmedQty, res,
			sig.Reason+" | "+decision.Reason+" | broker did not confirm this order's outcome")
	} else if authOrder.Side == broker.Sell && !resting {
		// The whole order is now definitively confirmed one way or the other —
		// filled, or a confirmed zero (rejected/cancelled). Either way the
		// dedupe guard has done its job (blocking a resubmit while the outcome
		// was genuinely unknown) and must be cleared: on a confirmed rejection
		// `held` never changes, so leaving it set would block every future
		// exit attempt for this symbol forever on a broker that doesn't
		// implement OrderLister (Sim, Alpaca) — see reconcile, which is the
		// only other thing that ever clears it, and only for brokers that do.
		e.clearPendingSell(sig.Symbol)
	}

	if filledQty <= 0 {
		// Nothing confirmed to book: either every slice was a resting limit
		// order (watchLimitOrder owns its eventual resolution) or the whole
		// order was unconfirmed (already flagged above). Booking a guess here
		// is exactly the phantom-position/phantom-loss risk settledQty exists
		// to avoid.
		e.syncPortfolio(ctx)

		return
	}

	fillPx := res.FilledPx
	if fillPx <= 0 {
		fillPx = price
	}

	value := fillPx * filledQty
	if authOrder.Side == broker.Buy {
		e.rm.RecordBuy(value)
		e.persistSpend()
	}

	var outcome *realised

	if authOrder.Side == broker.Sell {
		if r, ok := e.closePnL(sig.Symbol, fillPx, filledQty); ok {
			outcome = &r
		}
	}

	rec := TradeRecord{
		Time:       time.Now(),
		Symbol:     sig.Symbol,
		Side:       authOrder.Side,
		Qty:        filledQty,
		Price:      fillPx,
		Value:      value,
		Status:     filledOr(res.Status, filledQty),
		Reason:     sig.Reason + " | " + decision.Reason,
		OrderID:    res.ID,
		Indicators: indSnap,
	}

	outcome.apply(&rec)
	e.record(rec)

	// Persist the open/closed position at the confirmed filled quantity, not
	// the authorised one — a partial fill must not corrupt the weighted
	// average entry price or later oversell the position.
	if authOrder.Side == broker.Buy {
		e.recordOpen(sig.Symbol, filledQty, fillPx, indSnap)
	} else {
		e.recordClose(sig.Symbol, filledQty)
	}

	e.syncPortfolio(ctx)
}

// maxVenueBarInterval is the coarsest bar spacing whose last close may still be
// treated as "the venue's current price".
//
// venuePrice asks for 1-minute bars, but a history source is free to serve
// something coarser: market.YahooHistory falls back to hourly and then daily
// candles when the fine interval returns too few bars, and returns them with no
// indication of the substitution. The last close of a DAILY candle is not a
// current price — it can be a day old — and using it to size an order or to
// judge a deviation gate is a real mispricing, not a display quirk.
//
// Five minutes is deliberately loose: genuine 1-minute data on a thinly traded
// instrument has gaps, and the inference below measures the series' finest
// spacing precisely so those gaps do not make real 1-minute data look hourly.
const maxVenueBarInterval = 5 * time.Minute

// venuePrice fetches the broker venue's latest price for symbol from the warmup
// history source (which keys on the watchlist symbol, e.g. LXS.DE — the actual
// listing the broker trades), so order sizing reflects the execution venue
// rather than a possibly-divergent live reference feed. The source reports in
// the instrument's native currency, same as a live tick, so it's converted
// the same way before returning — callers compare this against signal/cached
// prices that are already in the engine's preferred currency (see ingest).
//
// It returns the last candle's close, a reason when no usable price could be
// established, and whether one was. A price is refused when the series turns
// out to be coarser than intraday (see maxVenueBarInterval) — "I could not
// confirm a current venue price" is the honest answer there, and far safer than
// handing back a stale close that every caller would treat as live.
func (e *Engine) venuePrice(symbol string) (float64, string, bool) {
	src := e.historySource()
	if src == nil {
		return 0, "no history source to confirm a venue price", false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

	defer cancel()

	to := time.Now()
	from := to.Add(-2 * 24 * time.Hour)
	candles, err := src.Candles(ctx, symbol, market.Res1Min, from, to)

	if err != nil || len(candles) == 0 {
		return 0, "venue price unavailable — cannot confirm before ordering", false
	}

	if iv, ok := finestBarGap(candles); ok && iv > maxVenueBarInterval {
		return 0, fmt.Sprintf(
			"venue price is %s-bar data, not a current price (requested 1m) — "+
				"cannot confirm before ordering", iv.Round(time.Minute)), false
	}

	last := candles[len(candles)-1].Close
	if last <= 0 {
		return 0, "venue price unavailable — cannot confirm before ordering", false
	}

	return last * e.fxRateFor(symbol), "", true
}

// finestBarGap infers a candle series' native spacing as the SMALLEST positive
// gap between consecutive bars.
//
// The smallest gap, not the average or the last one: real 1-minute data on an
// illiquid instrument is full of multi-minute holes where nothing traded, so an
// average would read as "hourly" and reject perfectly good data. One genuine
// one-minute step anywhere in the series proves the feed is minute-resolution.
// Conversely a series that never steps finer than an hour IS hourly data,
// whatever was asked for. ok is false for a series too short to measure.
func finestBarGap(candles []market.Candle) (time.Duration, bool) {
	if len(candles) < 2 {
		return 0, false
	}

	smallest := time.Duration(0)

	for i := 1; i < len(candles); i++ {
		gap := candles[i].Time.Sub(candles[i-1].Time)
		if gap <= 0 {
			continue
		}

		if smallest == 0 || gap < smallest {
			smallest = gap
		}
	}

	return smallest, smallest > 0
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

	vp, reason, found := e.venuePrice(symbol)
	if !found {
		return 0, reason, false
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
//nolint:cyclop // mirrors act(): authorize → submit → record in one flow
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

	// Hold this buy's estimated cash cost against risk.Manager's headroom
	// for the round trip to the broker — otherwise a second manual/engine
	// buy authorized concurrently would see the same unspent cash this one
	// is about to commit. See risk.Manager.Reserve.
	var resID uint64
	if side == broker.Buy {
		resID = e.rm.ReserveBuy(authOrder.Qty, price)
	}

	res, err := e.brk.SubmitOrder(ctx, authOrder)
	if err != nil {
		e.rm.Release(resID)

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

	e.met.onOrder() // count only orders the broker accepted

	if side == broker.Sell {
		e.markPendingSell(symbol, qty) // dedupe guard for the in-flight/unconfirmed window
	}

	settled, confirmed := settledQty(res)
	if !confirmed {
		// Ambiguous — leave resID reserved rather than guess; see
		// risk.Manager.Reserve's fails-closed guidance.
		rec := e.flagUnconfirmed(symbol, side, authOrder.Qty, res,
			"manual | "+decision.Reason+" | broker did not confirm this order's outcome")
		e.syncPortfolio(ctx)

		return rec, nil
	}

	e.rm.Release(resID) // settled one way or the other; RecordBuy books the real spend

	if side == broker.Sell && settled <= 0 {
		// A confirmed rejection/cancellation, not an unknown outcome — clear
		// the dedupe guard (see the identical case in exitPosition) so a
		// still-held position isn't permanently unsellable on a broker that
		// doesn't implement OrderLister.
		e.clearPendingSell(symbol)

		rec := TradeRecord{
			Time: time.Now(), Symbol: symbol, Side: side, Price: price,
			Status: filledOr(res.Status, settled), Reason: "manual | " + decision.Reason,
			OrderID: res.ID, Indicators: indSnap,
		}
		e.record(rec)
		e.syncPortfolio(ctx)

		return rec, nil
	}

	fillPx := res.FilledPx
	if fillPx <= 0 {
		fillPx = price
	}

	value := fillPx * settled
	if side == broker.Buy {
		e.rm.RecordBuy(value)
		e.persistSpend()
	}

	var outcome *realised

	if side == broker.Sell {
		if r, ok := e.closePnL(symbol, fillPx, settled); ok {
			outcome = &r
		}
	}

	rec := TradeRecord{
		Time:       time.Now(),
		Symbol:     symbol,
		Side:       side,
		Qty:        settled,
		Price:      fillPx,
		Value:      value,
		Status:     filledOr(res.Status, settled),
		Reason:     "manual | " + decision.Reason,
		OrderID:    res.ID,
		Indicators: indSnap,
	}

	outcome.apply(&rec)
	e.record(rec)

	// Book at the confirmed filled quantity, not the authorised one — a
	// partial fill must not corrupt the weighted average entry price or later
	// oversell the position.
	if side == broker.Buy {
		e.recordOpen(symbol, settled, fillPx, indSnap)
	} else {
		e.recordClose(symbol, settled)
	}

	e.syncPortfolio(ctx)

	return rec, nil
}

// record appends a trade decision to the bounded in-memory log, persists it,
// and publishes it to event subscribers.
func (e *Engine) record(t TradeRecord) {
	if raw, err := json.Marshal(e.StrategyConfigFor(t.Symbol)); err == nil {
		t.StrategyConfig = string(raw)
	}

	e.mu.Lock()

	t.Simulated = e.testModeActive
	e.trades = append(e.trades, t)
	if len(e.trades) > maxTrades {
		e.trades = e.trades[len(e.trades)-maxTrades:]
	}

	e.mu.Unlock()

	if err := e.store.SaveTrade(t); err != nil {
		e.log.Warn("persist trade failed", "err", err)
		errlog.Recordf("storage", "persist trade failed: %v", err)
	}

	rec := t
	e.emit(Event{Type: EventTrade, Time: t.Time, Symbol: t.Symbol, Trade: &rec})
	e.log.Info("trade", "symbol", t.Symbol, "side", t.Side, "qty", t.Qty,
		"price", t.Price, "status", t.Status, "reason", t.Reason)

	// Every decision funnels through here, so this is the one place protections
	// need to learn outcomes and alerts need to be raised.
	e.feedProtections(t)
	e.noteRealizedClose(t)
	e.alert(t)
}

// feedProtections reports a closed position's outcome to the protections, so
// the cooldown and stop guard see it. Only realized closing sells carry P&L.
func (e *Engine) feedProtections(t TradeRecord) {
	p := e.Protections()
	if p == nil || t.Side != broker.Sell || t.PnL == nil || t.Status == "blocked" {
		return
	}

	// Same reasoning as checkDrawdown: protections protect the REAL account.
	// A simulated close must not lock a symbol out (RecordClose) or halt
	// trading (ObserveRealizedClose) — either would apply to real trading
	// once test mode ends, based on activity that never touched the real
	// account.
	if t.Simulated {
		return
	}

	// A protective stop firing repeatedly on one symbol is the pattern the stop
	// guard exists to catch; a strategy-signalled exit is not.
	stopped := strings.Contains(t.Reason, "stop-loss") ||
		strings.Contains(t.Reason, "trailing-stop")

	p.RecordClose(t.Symbol, *t.PnL, stopped)

	if reason := p.ObserveRealizedClose(*t.PnL); reason != "" {
		e.haltFromProtections(reason)
	}
}

// alert forwards a trade record to the notifier as the appropriate event kind.
func (e *Engine) alert(t TradeRecord) {
	n := e.notifyOf()
	if n == nil {
		return
	}

	kind := notify.KindTrade

	switch t.Status {
	case "blocked":
		kind = notify.KindBlocked
	case "error":
		kind = notify.KindError

		// A failing order is retried every evaluation for as long as the
		// signal holds. Announce a given failure once, not once per cycle —
		// the trade-log row is still written every time. See
		// announcedOrderError.
		if t.Symbol != "" &&
			e.announcedOrderError(t.Symbol, string(t.Side)+"|"+t.Err) {
			return
		}
	case "halted":
		kind = notify.KindHalt
	case "unknown":
		// An order the broker never confirmed — as urgent as a halt: an
		// operator needs to reconcile it by hand, and the bot cannot.
		kind = notify.KindHalt
	case "dry-run":
		return // a dry run placed nothing; not worth a notification
	}

	if t.Status != "error" && t.Symbol != "" {
		e.clearOrderError(t.Symbol) // the condition cleared; a recurrence is news again
	}

	title := fmt.Sprintf("%s %s", strings.ToUpper(string(t.Side)), t.Symbol)
	if t.Symbol == "" {
		title = t.Status
	}

	text := fmt.Sprintf("qty %.4f @ %.2f (%.2f)", t.Qty, t.Price, t.Value)
	if t.PnL != nil {
		text += fmt.Sprintf("\nP&L %.2f (%.2f%%)", *t.PnL, *t.PnLPct)
	}

	if t.Reason != "" {
		text += "\n" + t.Reason
	}

	n.Send(notify.Event{
		Kind: kind, Time: t.Time, Title: title, Text: text,
		Symbol: t.Symbol, Notional: t.Value,
	})
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
		wasTestModeActive := e.tripBreakerLocked() // trip the breaker; e.mu is already held here
		e.mu.Unlock()

		if wasTestModeActive {
			e.EndTestMode("trade-rate circuit breaker tripped")
		}

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

		p.Adds++ // a pyramid or scale-in step, capped by scale_in.max_adds
	}

	p.Qty += qty
	// Accumulate what this buy cost to place, so the eventual close can be
	// booked net of the round trip (see OpenPosition.EntryCommission). Uses
	// the same modelled cost function risk.Manager charges against
	// max_daily_spend, so the two never disagree about what a fill cost.
	p.EntryCommission += e.commissionFor(qty * price)

	// The scale-out ladder measures against the largest the position has been,
	// so adding to a position re-bases the rungs rather than leaving them sized
	// against the first tranche.
	if p.Qty > p.OrigQty {
		p.OrigQty = p.Qty
	}

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

	// Draw down the entry commission by the same fraction closePnL just
	// charged, so a later rung is not charged for the share this sale already
	// carried (see OpenPosition.EntryCommission).
	if p.Qty > 0 && qty < p.Qty {
		p.EntryCommission -= p.EntryCommission * (qty / p.Qty)
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

// pendingFillGrace bounds how long pendingExposure will keep overlaying an
// openPos quantity the broker snapshot doesn't (yet) reflect. A fill still
// settling resolves within one sync cycle — seconds under any reasonable
// sync_interval, and order paths force an immediate post-fill sync besides —
// so this is deliberately far larger than that: long enough to absorb a
// broker outage or a paused sync, but short enough that a position actually
// closed outside the bot (the case reconcile's drift panel and
// ForgetOpenPosition exist for) stops being counted as real exposure well
// before an operator is likely to notice and clear it by hand. Without this
// bound, that drift inflates equity indefinitely, which both silently masks
// a real drawdown while it persists and makes clearing it via "forget" look
// like a sudden, false one when protections finally observe the correction.
// See CODE_REVIEW_FINDINGS.md's "forgetting a drifted position trips a false
// max-drawdown halt" entry.
const pendingFillGrace = 10 * time.Minute

// pendingExposure overlays locally tracked open positions onto snap. The
// broker snapshot only reflects settled fills, so a buy still working at the
// broker (status NEW) is otherwise invisible to the pyramid guard, the risk
// caps (max_total_invested / max_per_position / max_open_positions) and the
// drawdown/daily-loss protections, which would all undercount real exposure
// while a fill is pending. See pendingFillGrace for why this overlay expires.
func (e *Engine) pendingExposure(snap portfolio.Snapshot) portfolio.Snapshot {
	e.mu.Lock()

	open := make(map[string]OpenPosition, len(e.openPos))
	for sym, p := range e.openPos {
		open[sym] = *p
	}

	if e.driftSince == nil {
		e.driftSince = make(map[string]time.Time)
	}

	drifting := make(map[string]bool, len(open))

	for sym, p := range open {
		if p.Qty <= 0 {
			continue
		}

		existing, held := snap.Holding(sym)
		if held && existing.Qty >= p.Qty {
			continue // broker already reflects at least this much
		}

		drifting[sym] = true

		if _, tracked := e.driftSince[sym]; !tracked {
			e.driftSince[sym] = now()
		}
	}

	for sym := range e.driftSince {
		if !drifting[sym] {
			delete(e.driftSince, sym) // broker caught up, or the record is gone
		}
	}

	since := make(map[string]time.Time, len(drifting))
	for sym := range drifting {
		since[sym] = e.driftSince[sym]
	}

	e.mu.Unlock()

	for sym, p := range open {
		if p.Qty <= 0 || !drifting[sym] {
			continue
		}

		if now().Sub(since[sym]) > pendingFillGrace {
			continue // stale drift, not a fill still settling — see pendingFillGrace
		}

		existing, held := snap.Holding(sym)

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

		// Normalized key, matching how portfolio keys the real holdings this
		// overlay merges into — otherwise a differently-cased openPos key
		// would add a SECOND synthetic row for a symbol already present.
		snap.Holdings[portfolio.Normalize(sym)] = portfolio.Holding{
			Symbol: portfolio.Normalize(sym), Qty: p.Qty, AvgPrice: p.EntryPrice,
			LastPrice: price, MarketValue: mv,
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

// ForgetOpenPosition drops the bot's durable record for symbol without selling
// anything. It exists for one situation: the position was closed outside the
// bot (sold by hand in the broker's own app), so the record now describes stock
// that is not there. Left in place it keeps the symbol's protective exits armed
// against a phantom holding and keeps counting toward max_open_positions.
//
// Nothing is booked as a trade. The bot does not know what the manual sale
// filled at, and inventing an exit price would corrupt realized P&L and the
// win/loss statistics far more than the missing round-trip does. The entry is
// simply forgotten.
//
// It returns false when no record exists, so the API can 404 rather than report
// a success that did nothing.
//
// Forgetting is bookkeeping, never a trading loss — but pendingExposure (see
// checkDrawdown) may have been overlaying this record's quantity onto equity
// as a phantom holding (a drifted record the broker never confirmed, exactly
// what this method clears). Removing it then drops equity by that same
// phantom amount, which without correction would misread as a real drawdown
// against a peak that phantom equity had itself inflated. To prevent that,
// the equity this record was contributing is measured before and after the
// removal, and any drop is un-done from the protections' peak — a forget
// must never be able to trip a drawdown halt by itself. See
// CODE_REVIEW_FINDINGS.md's "forgetting a drifted position trips a false
// max-drawdown halt" entry.
func (e *Engine) ForgetOpenPosition(symbol string) bool {
	e.mu.Lock()

	p, ok := e.openPos[symbol]
	if !ok {
		e.mu.Unlock()

		return false
	}

	qty := p.Qty
	e.mu.Unlock()

	before := e.pendingExposure(e.pf.Snapshot())

	e.mu.Lock()
	delete(e.openPos, symbol)
	delete(e.peaks, symbol)
	// A sell submitted for the old holding can never fill against stock that is
	// gone; leaving the guard set would suppress a later, legitimate exit.
	delete(e.pendingSell, symbol)
	delete(e.driftSince, symbol)
	e.mu.Unlock()

	after := e.pendingExposure(e.pf.Snapshot())

	beforeEquity := before.Cash + before.Invested
	afterEquity := after.Cash + after.Invested

	if delta := beforeEquity - afterEquity; delta > 0 {
		e.Protections().LowerPeak(delta)
	}

	if err := e.store.DeleteOpenPosition(symbol); err != nil {
		e.log.Warn("delete open position failed", "symbol", symbol, "err", err)
	}

	e.log.Info("open position forgotten (closed outside the bot)",
		"symbol", symbol, "qty", qty)

	return true
}

// AdoptHeldPosition creates a durable record for a position the broker
// reports but the bot has none for — the mirror case to ForgetOpenPosition,
// and the Reconciliation panel's "held, not tracked" row. This is not
// cosmetic: checkExits applies percentage stop-loss/take-profit to every
// broker-held symbol regardless of whether it is tracked (entry/qty come
// straight from the broker snapshot), but the scale-out ladder
// (checkPartialExit), the max_hold time barrier (heldFor) and realized P&L
// (closePnL) all require an openPos record and silently do nothing without
// one — so an untracked holding gets a real stop-loss but no ladder, no
// time barrier, and its eventual exit reports no P&L and never reaches the
// stop-loss/low-profit protections (feedProtections skips a trade with a
// nil PnL). See CODE_REVIEW_FINDINGS.md's "held, not tracked positions get
// degraded protective management" entry.
//
// The most common cause is a fill the broker confirmed but the bot's own
// confirmation read did not (see the Trading212 404-while-polling fix,
// CRIT-3) — the trade genuinely happened, but nothing ever called
// RecordBuy.
//
// EntryPrice/Qty come from the broker's own reported average price and
// quantity — unlike ForgetOpenPosition's sell side, the true fill price IS
// knowable here. EntryTime is set to now, since the bot has no record of
// when the position actually opened, so max_hold's clock starts from
// adoption rather than the real entry. Peak seeds at max(avg price, last
// price) rather than scanning candle history for a true historical high,
// matching what checkExits already computes on the fly for an untracked
// symbol — a looked-up historical peak could sit above the current price
// for reasons unrelated to this specific position and trip an immediate,
// unwarranted trailing stop.
//
// It returns false when the symbol is not held at the broker, or a record
// already exists — this is only for the "held, not tracked" row; a
// quantity mismatch where the bot already has a (wrong) record is a
// different kind of drift, not handled here.
func (e *Engine) AdoptHeldPosition(symbol string) bool {
	h, held := e.pf.Holding(symbol)
	if !held || h.Qty <= 0 {
		return false
	}

	price := h.LastPrice
	if cached, ok := e.prices.Get(symbol); ok && cached > 0 {
		price = cached
	}

	pos := &OpenPosition{
		Symbol:     symbol,
		Qty:        h.Qty,
		EntryPrice: h.AvgPrice,
		EntryTime:  now(),
		Peak:       max(h.AvgPrice, price),
	}

	// The "is it already tracked" check and the write happen under the same
	// lock, so two near-simultaneous adopt calls for the same symbol (a
	// double-click before the button disables, two browser tabs) can't both
	// pass the check and race to write — the second always sees the first's
	// record and backs off, rather than silently re-writing over it.
	e.mu.Lock()

	if _, tracked := e.openPos[symbol]; tracked {
		e.mu.Unlock()

		return false
	}

	e.openPos[symbol] = pos
	e.peaks[symbol] = pos.Peak
	e.mu.Unlock()

	if err := e.store.SaveOpenPosition(*pos); err != nil {
		e.log.Warn("persist adopted open position failed", "symbol", symbol, "err", err)
	}

	e.log.Info("open position adopted (held at the broker, not previously tracked)",
		"symbol", symbol, "qty", pos.Qty, "entry_price", pos.EntryPrice)

	return true
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

// now returns the current wall-clock time. A var, not a func, so tests can
// place the engine at a specific hour — the market-hours gates are decided
// entirely by what time it is, and there is no other way to exercise them.
var now = time.Now

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

	e.refreshFXRates()

	acct, err := e.brk.GetAccount(ctx)
	if err != nil {
		e.log.Debug("account sync failed", "err", err)

		return // stale cash paired with fresh positions would misprice equity
	}

	e.pf.SetCash(acct.Cash)

	positions, err := e.brk.GetPositions(ctx)
	if err != nil {
		e.log.Debug("positions sync failed", "err", err)

		return
	}

	hs := make([]portfolio.Holding, len(positions))
	for i, p := range positions {
		// Positions carry the most current, broker-authoritative FX rate for
		// their symbol; refresh the engine's cache from it opportunistically
		// so ingest() converts live ticks correctly even between
		// refreshFXRates' hourly full-watchlist passes.
		if p.FXRate != 0 {
			e.fxMu.Lock()
			e.fxRates[p.Symbol] = p.FXRate
			e.fxMu.Unlock()
		}

		px := p.Current
		if cached, ok := e.prices.Get(p.Symbol); ok {
			// e.prices holds prices already converted to the preferred
			// currency (ingest applies fxRateFor before caching), consistent
			// with p.Current/AvgPrice from the broker — no further conversion
			// needed here.
			px = cached
		}

		hs[i] = portfolio.Holding{
			Symbol: p.Symbol, Qty: p.Qty, AvgPrice: p.AvgPrice, LastPrice: px,
			MarketValue: px * p.Qty, UnrealizedPL: (px - p.AvgPrice) * p.Qty,
		}
	}

	e.pf.SyncPositions(hs)
	e.adoptUntrackedHoldings(hs)

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

	e.checkDrawdown()
}

// haltFromProtections trips the breaker and records/persists the halt for a
// reason protect.Manager just reported — shared by checkDrawdown
// (mark-to-market equity) and feedProtections (realized-only P&L), so both
// react identically once protect.Manager decides to halt.
func (e *Engine) haltFromProtections(reason string) {
	e.tripBreaker("protection tripped: " + reason)

	e.log.Error("protection tripped — automated trading halted", "reason", reason)
	e.record(TradeRecord{
		Time: time.Now(), Status: "halted",
		Reason: "protections: " + reason + " — trading disabled until resumed",
	})
	e.PersistProtectionState()
}

// checkDrawdown feeds current equity to the protections and, if a drawdown or
// daily-loss limit has been breached, halts automated trading. Equity is only
// meaningful after a successful position sync, which is why this runs here.
//
// The halt is deliberately sticky: it stays until someone calls POST
// /api/protections/resume. An automated bot that un-halts itself would walk
// straight back into whatever was draining the account.
//
// Skipped entirely while test mode is active: protections exist to protect
// the REAL account, and the simulated book's equity has no bearing on that.
// Feeding it in would both halt (and so end) the very test-mode window being
// observed on a simulated drawdown, and corrupt the real peakEquity baseline
// that trading resumes against once test mode ends. See feedProtections for
// the realized-P&L counterpart.
func (e *Engine) checkDrawdown() {
	if e.inTestMode() {
		return
	}

	p := e.Protections()
	if p == nil {
		return
	}

	// Overlay pending (submitted-but-not-yet-broker-confirmed) buys, same as the
	// risk caps do. Without this, a buy debits cash immediately while the broker
	// snapshot may not show the new position for a moment, making equity look
	// like it dropped by the full purchase price instead of just moving from
	// cash into stock of equal value.
	snap := e.pendingExposure(e.pf.Snapshot())
	equity := snap.Cash + snap.Invested

	if reason := p.ObserveEquity(equity); reason != "" {
		e.haltFromProtections(reason)
	}
}

// PersistProtectionState saves the protections' halt/peak/day-equity state so
// it survives a restart — without this, an active drawdown/daily-loss halt
// silently clears on restart, because a fresh peak makes the drawdown
// compute as 0% (see protect.State's doc comment). Errors are logged, not
// fatal: the in-memory halt still holds for the life of this process either
// way.
func (e *Engine) PersistProtectionState() {
	p := e.Protections()
	if p == nil {
		return
	}

	if err := e.store.SaveProtectionState(p.ExportState()); err != nil {
		e.log.Warn("persist protection state failed", "err", err)
	}
}

// RestoreProtectionState loads previously-persisted halt/peak/day-equity
// state into the active protections manager, if any. Call after
// SetProtectionsConfig (so the manager exists) and before the engine starts
// evaluating.
func (e *Engine) RestoreProtectionState() {
	p := e.Protections()
	if p == nil {
		return
	}

	st, ok, err := e.store.LoadProtectionState()
	if err != nil {
		e.log.Warn("load protection state failed", "err", err)

		return
	}

	if !ok {
		return
	}

	p.ImportState(st)

	if st.Halted {
		e.log.Warn("restored an active protection halt from a previous run",
			"reason", st.HaltReason, "halted_at", st.HaltedAt)

		e.tripBreaker("restored halt: " + st.HaltReason)
	}
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
// current signal (whether or not it is in the active set), evaluated with that
// symbol's own parameters when it has overrides.
func (e *Engine) DetectorSignals() map[string][]strategy.DetectorResult {
	syms := e.Watchlist()
	out := make(map[string][]strategy.DetectorResult, len(syms))

	for _, sym := range syms {
		out[sym] = strategy.EvaluateAll(e.StrategyConfigFor(sym), sym, e.seriesFor(sym))
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

// sizeBuy decides how much of symbol to buy at price, delegating to
// risk.Manager.SizeBuy so the live engine and the backtester size orders
// through one implementation rather than two mirrored ones.
//
// There is no fixed order size any more: sizing starts at the real money-limit
// headroom — which now carries the position-slot rule that min_positions and
// min_positions_per_asset_class feed — and risk reduces it from there. See
// risk.Manager.SizeBuy for the full rationale.
func (e *Engine) sizeBuy(symbol string, price float64) risk.BuySize {
	e.mu.RLock()

	cfg := risk.SizingConfig{
		Exits:           e.exits,
		MaxRiskSizeUSD:  e.cfg.MaxRiskSizeUSD,
		MinOrderSizeUSD: e.cfg.MinOrderSizeUSD,
	}
	e.mu.RUnlock()

	return e.rm.SizeBuy(symbol, price, e.pf.Snapshot(), cfg, e.atrFor(symbol))
}

// Sizing returns the current order-sizing settings.
func (e *Engine) Sizing() (minPositions int, maxRiskSizeUSD, minOrderSizeUSD float64) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.cfg.MinPositions, e.cfg.MaxRiskSizeUSD, e.cfg.MinOrderSizeUSD
}

// SetSizing updates order sizing at runtime.
//
// minPositions only overwrites when >= 0 (it has no meaning below that).
// maxRiskSizeUSD/minOrderSizeUSD are always overwritten: 0 is a real, valid
// value for each (no cap; no refusal threshold), matching how every other
// settings section in this codebase treats a PUT as a whole-value replace.
//
// minPositions also goes to the risk manager, which is where it now binds —
// as the coarse tier of the position-slot rule (see
// risk.PortfolioGuard.MaxPositionValue) rather than a separate ceiling the
// engine applied before Authorize ever saw the order.
func (e *Engine) SetSizing(minPositions int, maxRiskSizeUSD, minOrderSizeUSD float64) {
	e.mu.Lock()

	if minPositions >= 0 {
		e.cfg.MinPositions = minPositions
	}

	e.cfg.MaxRiskSizeUSD = maxRiskSizeUSD
	e.cfg.MinOrderSizeUSD = minOrderSizeUSD
	applied := e.cfg.MinPositions

	e.mu.Unlock()
	e.rm.SetMinPositions(applied)

	e.log.Info("sizing updated",
		"min_positions", applied,
		"max_risk_size_usd", maxRiskSizeUSD,
		"min_order_size_usd", minOrderSizeUSD)
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

	return e.tradingEnabled || e.testModeActive
}

// tripBreakerLocked halts trading, assuming the caller already holds
// e.mu.Lock(). It returns whether test mode was active, so the caller can
// unlock and then call EndTestMode itself — EndTestMode takes its own lock,
// so it cannot be called while this one is held.
func (e *Engine) tripBreakerLocked() (wasTestModeActive bool) {
	e.tradingEnabled = false
	return e.testModeActive
}

// tripBreaker halts trading (real or simulated) and, if a test-mode window
// was active, ends it too. Of its call sites, a dead quote feed and a
// tripped order-rate breaker mean an assumption the simulation depends on
// just as much as real trading (fresh prices, a sane order cadence) is
// already broken, so continuing to "train" through it would validate
// nothing. The protections-driven halt (haltFromProtections) can no longer
// actually reach here WHILE test mode is active — checkDrawdown and
// feedProtections both skip a simulated book entirely, precisely so that
// protections (which exist for the real account) never cut a training
// window short on simulated activity — but the guard stays regardless: it
// is still correct for a real halt that happens to land while test mode
// is (for whatever future reason) active.
func (e *Engine) tripBreaker(reason string) {
	e.mu.Lock()
	wasActive := e.tripBreakerLocked()
	e.mu.Unlock()

	if wasActive {
		e.EndTestMode(reason)
	}
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
		Running:          e.running,
		TradingEnabled:   e.tradingEnabled,
		Broker:           e.brk.Name(),
		Strategy:         e.start.Name(),
		Symbols:          len(e.watchlist),
		SpentToday:       e.rm.SpentToday(),
		LastEval:         e.lastEval,
		TestModeActive:   e.testModeActive,
		TestModeDeadline: e.testModeDeadline,
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

// filledOr returns a broker status, defaulting to "filled" when the broker
// reported none — the sim and some REST adapters leave it empty on an
// immediate fill.
// filledOr returns status, or a fallback describing qty when the broker left
// status blank. An empty status alongside a real (qty > 0) fill safely means
// "filled" — some brokers confirm a fill (Terminal, or a real reported
// FilledQty) without ever populating a status string. An empty status
// alongside qty == 0 must NOT also default to "filled": that combination is
// a confirmed rejection/cancellation (settledQty's "confirmed zero"), and
// several call sites record it with Qty/Value left at their zero value —
// reporting Status "filled" next to Qty 0 in the trade log is exactly the
// misleading-audit-trail risk CRIT-1/2 already flagged, just one step
// further downstream. See CODE_REVIEW_FINDINGS.md's filledOr finding.
func filledOr(status string, qty float64) string {
	if status != "" {
		return status
	}

	if qty > 0 {
		return "filled"
	}

	return "unfilled"
}
