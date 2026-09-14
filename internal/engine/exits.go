package engine

import (
	"context"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/exits"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/strategy/indicators"
)

// atrFor returns the current Average True Range for symbol in price units, or 0
// when the volatility-adaptive stops are not configured or there is too little
// history. Computed from the engine's own bars, so it is the same volatility the
// detectors see.
// It reads a bounded window rather than the whole history, for two reasons.
//
// The cheap one: copying up to a week of one-minute candles and three more
// full-length float columns, per held symbol, per evaluation, to produce a
// single number is waste that scales with how long the bot has been running.
//
// The one that matters: the backtester's atrAt uses exactly this window
// (period*3). Wilder's ATR is a recursive average whose old terms decay but
// never vanish, so reading the full series here and a bounded one there made the
// live stop and the backtested stop subtly different numbers — the class of
// divergence internal/exits exists to prevent.
func (e *Engine) atrFor(symbol string) float64 {
	cfg := e.ExitsConfig()
	if !exits.UsesATR(cfg) {
		return 0
	}

	period := exits.ATRPeriod(cfg)

	e.atrMu.Lock()
	defer e.atrMu.Unlock()

	e.atrBars = e.barsStore.tail(symbol, period*3, e.atrBars[:0])
	if len(e.atrBars) < period+2 {
		return 0
	}

	e.atrCols = growColumns(e.atrCols, len(e.atrBars))

	for i, c := range e.atrBars {
		e.atrCols.high[i], e.atrCols.low[i], e.atrCols.closes[i] = c.High, c.Low, c.Close
	}

	atr, ok := indicators.ATRLast(e.atrCols.high, e.atrCols.low, e.atrCols.closes, period)
	if !ok {
		return 0
	}

	return atr
}

// columns is a reusable set of aligned OHLC columns.
type columns struct {
	high, low, closes []float64
}

// growColumns resizes c to hold exactly n values, reusing its arrays when they
// are already large enough.
func growColumns(c columns, n int) columns {
	if cap(c.high) >= n {
		return columns{c.high[:n], c.low[:n], c.closes[:n]}
	}

	// One allocation carved into three, rounded up so a window that grows bar by
	// bar does not reallocate on every one.
	size := n + n/4 + 16
	buf := make([]float64, 3*size)

	return columns{
		high:   buf[0:n:size],
		low:    buf[size : size+n : 2*size],
		closes: buf[2*size : 2*size+n : 3*size],
	}
}

// heldFor returns how long the position in symbol has been open, from the bot's
// durable open-position record. Zero when the entry time is unknown, which
// disables the ROI ladder for that position rather than guessing its age.
func (e *Engine) heldFor(symbol string) time.Duration {
	e.mu.RLock()
	defer e.mu.RUnlock()

	p, ok := e.openPos[symbol]
	if !ok || p.EntryTime.IsZero() {
		return 0
	}

	return time.Since(p.EntryTime)
}

// maxExitPriceAge bounds how old an engine-side tick price may be before
// checkExits treats it as unusable and falls back to the broker's own
// last-synced price instead. Generous relative to any reasonable
// eval_interval, so it only catches a feed that has genuinely stalled
// (dropping ticks under load), not ordinary quote cadence.
const maxExitPriceAge = 5 * time.Minute

// checkExits enforces protective exit rules (stop-loss, take-profit, trailing
// stop) on every open position. It runs before strategy signals so a triggered
// exit always wins. Exits respect the trading toggle: when trading is disabled
// they are recorded as dry-run — unless cfg.BypassTradingHalt is set, in which
// case they keep submitting real orders regardless. The toggle is re-read per
// position (not passed in) so that a circuit-breaker trip mid-loop downgrades
// the remaining exits to dry-run instead of re-tripping the breaker per symbol.
//
//nolint:cyclop // one guarded exit rule per branch; the order is significant
func (e *Engine) checkExits(ctx context.Context) {
	// A single locked snapshot for the whole sweep: e.exits is a struct SetExitsConfig
	// replaces wholesale (never mutated field-by-field), so one RLock-protected copy
	// here is internally consistent for the rest of this function and checkPartialExit,
	// where the old code read e.exits.Enabled, e.exits (passed to exits.Compute) and
	// e.exits.PartialTakeProfit/e.exits (in checkPartialExit) with no lock at all — a
	// settings PATCH mid-sweep could torn-read a slice header, wrong-length pairing.
	cfg := e.ExitsConfig()
	if !cfg.Enabled {
		return
	}

	snap := e.pf.Snapshot()
	for sym, h := range snap.Holdings {
		if h.Qty <= sellDustQty {
			continue // empty or dust-only; nothing sellable
		}

		// GetFresh, not Get: a price the feed stopped updating a while ago
		// (dropping ticks under load without the stream fully closing) must
		// not be silently trusted for a stop-loss/take-profit decision — a
		// fast move is exactly what a stop exists to catch, and evaluating
		// it against a stale price can miss the move or fire on a level
		// that's no longer where the market actually is. Falling back to
		// the broker's own last-synced price (already the existing
		// behaviour for "no price at all") is safer than trusting either an
		// absent or a stale engine-side tick. See
		// CODE_REVIEW_FINDINGS.md's dropped-ticks-unrecoverable finding.
		price, ok := e.prices.GetFresh(sym, maxExitPriceAge)
		if !ok || price <= 0 {
			price = h.LastPrice
		}

		if price <= 0 {
			continue
		}

		entry := h.AvgPrice

		// Update and read the high-water mark for the trailing stop.
		e.mu.Lock()

		peak := e.peaks[sym]
		rose := false

		if price > peak {
			peak = price
			rose = true
		}

		if entry > peak {
			peak = entry
		}

		e.peaks[sym] = peak

		var (
			posSnap OpenPosition
			persist bool
		)

		if op, ok := e.openPos[sym]; ok && peak > op.Peak {
			op.Peak = peak
			posSnap, persist = *op, true
		}

		e.mu.Unlock()

		// Persist a new trailing-stop high-water mark so it survives restarts.
		if persist && rose {
			if err := e.store.SaveOpenPosition(posSnap); err != nil {
				e.log.Warn("persist open position peak failed", "symbol", sym, "err", err)
			}
		}

		// The same rule engine the backtester uses, so a stop that fires here
		// fires there — including the ATR-adaptive stops and the ROI ladder.
		levels := exits.Compute(cfg, entry, peak, e.atrFor(sym), e.heldFor(sym))

		if reason := levels.Triggered(price); reason != "" {
			e.exitPosition(ctx, sym, h.Qty, price, reason, e.tradingOn(), cfg.BypassTradingHalt, snap)

			continue
		}

		// Only once no full exit fired. A trailing stop that trips on the way
		// back down from a rung must close the position, not shave it: the
		// downside rules always outrank the scale-out ladder.
		e.checkPartialExit(ctx, cfg, sym, entry, price, h.Qty, snap)
	}
}

// checkPartialExit sells one tranche of a position that has cleared the next
// rung of the scale-out ladder. It is a no-op when no ladder is configured, when
// no rung is due, or when the position has already been fully scaled out.
//
// cfg is the caller's already-locked-and-copied ExitsConfig (see checkExits) —
// read once for the whole sweep rather than re-read per symbol.
func (e *Engine) checkPartialExit(
	ctx context.Context,
	cfg config.ExitsConfig,
	symbol string,
	entry, price, held float64,
	snap portfolio.Snapshot,
) {
	if len(cfg.PartialTakeProfit) == 0 || entry <= 0 {
		return
	}

	e.mu.RLock()

	p, ok := e.openPos[symbol]

	var orig float64

	taken := 0

	if ok {
		orig, taken = p.OrigQty, p.PartialsTaken
	}

	e.mu.RUnlock()

	if !ok {
		return // no durable record: nothing to measure rungs against
	}

	if orig <= 0 {
		orig = held
	}

	part, due := exits.NextPartial(e.exits, price/entry-1, taken)
	if !due {
		return
	}

	qty := min(orig*part.Fraction, held)
	// Never leave an unsellable crumb behind: if the remainder would be dust,
	// this rung closes the position instead.
	if held-qty <= sellDustQty {
		qty = held
	}

	if qty <= sellDustQty {
		return
	}

	if e.exitPosition(ctx, symbol, qty, price, part.Reason, e.tradingOn(), cfg.BypassTradingHalt, snap) {
		e.markPartialTaken(symbol, part.Rung)
	}
}

// markPartialTaken records that the ladder has fired through rung, so each rung
// fires once per position and the progress survives a restart.
func (e *Engine) markPartialTaken(symbol string, rung int) {
	e.mu.Lock()

	p, ok := e.openPos[symbol]
	if !ok { // fully closed by this sale — nothing left to remember
		e.mu.Unlock()

		return
	}

	p.PartialsTaken = rung + 1

	snap := *p
	e.mu.Unlock()

	if err := e.store.SaveOpenPosition(snap); err != nil {
		e.log.Warn("persist partial-exit progress failed", "symbol", symbol, "err", err)
	}
}

// exitPosition sells qty of a held position via a market order, passing through
// the risk manager and respecting the trading toggle. qty may be less than the
// whole holding — that is how the scale-out ladder takes a tranche. snap is the
// portfolio snapshot the caller already holds (checkExits takes one for the
// whole sweep).
//
// It reports whether a real fill was recorded, which the ladder needs: a rung
// must not be marked as taken when the sell was a dry run, was refused, or
// errored at the broker.
//
//nolint:cyclop // a linear sequence of guards, each with one way to decline
func (e *Engine) exitPosition(
	ctx context.Context,
	symbol string,
	qty, price float64,
	reason string,
	tradingEnabled, bypassHalt bool,
	snap portfolio.Snapshot,
) bool {
	// Same resubmit guard as strategy sells: while a prior sell for this
	// position is unconfirmed at the broker (async fill), the holding still
	// shows up in the snapshot and the exit would trigger again every
	// evaluation — don't stack duplicate sell orders.
	if pending, ok := e.pendingSellQty(symbol); ok && qty >= pending {
		return false
	}

	// A closed venue cannot fill anything, so an exit sent to one is waste at
	// best. It is worse than waste here: checkExits runs every eval_interval, so
	// a barrier that trips out of hours resubmits every 30s until the venue
	// reopens — thousands of orders across a weekend against a broker that is
	// heavily rate-limited. Nothing is lost by waiting: max_hold only grows, and
	// a price barrier is re-tested at the open against a fresh quote instead of
	// Friday's stale one.
	//
	// It also restores backtest parity. Historical candles only exist during
	// sessions, so the backtester can never fire an exit at 03:00 on a Saturday.
	// The live engine could, and did.
	//
	// Symbols the calendar does not recognise are treated as open, so an exit is
	// never permanently blocked by a missing exchange mapping.
	if e.cfg.MarketHoursCheck {
		if open, why := e.marketOpen(symbol); !open {
			e.log.Debug("exit deferred: venue closed",
				"symbol", symbol, "exit", reason, "venue", why)

			return false
		}
	}

	order := broker.Order{Symbol: symbol, Side: broker.Sell, Qty: qty, Type: broker.Market}
	authOrder, decision := e.rm.Authorize(order, price, snap)

	if !decision.Approved {
		return false // nothing sellable
	}

	indSnap := e.indicatorSnapshot(symbol)
	tag := "EXIT " + reason

	if !tradingEnabled && !bypassHalt {
		e.record(TradeRecord{
			Time: now(), Symbol: symbol, Side: broker.Sell, Qty: authOrder.Qty,
			Price: price, Value: decision.EstValue, Status: "dry-run", Reason: tag,
			Indicators: indSnap,
		})

		return false
	}

	if !e.guardOrderRate(true) {
		return false // circuit breaker tripped
	}

	res, err := e.brk.SubmitOrder(ctx, authOrder)
	if err != nil {
		e.record(TradeRecord{
			Time: now(), Symbol: symbol, Side: broker.Sell, Qty: authOrder.Qty,
			Price: price, Status: "error", Reason: tag, Err: err.Error(),
			Indicators: indSnap,
		})

		return false
	}

	e.met.onOrder() // count only orders the broker accepted

	// The dedupe guard keys on the holding as it stood when the order went out,
	// not on the slice being sold — otherwise a scale-out of 1 of 3 would leave
	// a marker of 1 and block the later full exit of the remaining 2. Set it
	// regardless of whether the fill is confirmed yet: it exists precisely for
	// the in-flight/unconfirmed window, to stop this same exit resubmitting
	// next cycle.
	heldAtSubmit := qty
	if h, ok := snap.Holding(symbol); ok && h.Qty > heldAtSubmit {
		heldAtSubmit = h.Qty
	}

	e.markPendingSell(symbol, heldAtSubmit)

	sold, confirmed := settledQty(res)
	if !confirmed {
		e.flagUnconfirmed(symbol, broker.Sell, authOrder.Qty, res, tag+" | broker did not confirm this order's outcome")
		e.syncPortfolio(ctx)

		return false
	}

	if sold <= 0 {
		// A confirmed rejection/cancellation, not an unknown outcome — nothing
		// sold and the position is unchanged, so the dedupe guard has done its
		// job and must be cleared: exitPosition always submits a plain market
		// order (never a resting limit), so a confirmed zero here is
		// definitive, and leaving the guard set would block every future exit
		// attempt for this symbol forever on a broker that doesn't implement
		// OrderLister (Sim, Alpaca) — see reconcile, the only other thing that
		// ever clears it, and only for brokers that do.
		e.clearPendingSell(symbol)
		e.record(TradeRecord{
			Time: now(), Symbol: symbol, Side: broker.Sell, Price: price,
			Status: filledOr(res.Status, sold), Reason: tag, OrderID: res.ID, Indicators: indSnap,
		})
		e.syncPortfolio(ctx)

		return false
	}

	fillPx := res.FilledPx
	if fillPx <= 0 {
		fillPx = price
	}

	var outcome *realised

	if r, ok := e.closePnL(symbol, fillPx, sold); ok {
		outcome = &r
	}

	rec := TradeRecord{
		Time:       now(),
		Symbol:     symbol,
		Side:       broker.Sell,
		Qty:        sold,
		Price:      fillPx,
		Value:      fillPx * sold,
		Status:     filledOr(res.Status, sold),
		Reason:     tag,
		OrderID:    res.ID,
		Indicators: indSnap,
	}

	outcome.apply(&rec)
	e.record(rec)
	e.recordClose(symbol, sold) // reduces, or removes the record once fully sold — the confirmed qty, not the requested one
	e.syncPortfolio(ctx)

	return true
}
