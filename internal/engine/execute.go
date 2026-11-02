package engine

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/errlog"
)

// defaultLimitTimeout is how long an unfilled limit order is left working before
// the engine gives up on it.
const defaultLimitTimeout = 2 * time.Minute

// defaultMaxSlices bounds how many child orders one parent may become.
const defaultMaxSlices = 5

// execPlan is how one approved order will actually be sent: a list of child
// orders, in submission order.
type execPlan struct {
	orders []broker.Order
	// note describes the plan for the trade record, e.g. "limit 99.95 x3
	// slices". Empty for a plain single market order.
	note string
}

// planExecution turns an authorised order into the orders that go to the venue.
//
// urgent orders — protective exits — bypass the limit logic entirely and go to
// market as a single clip. A stop-loss that rests unfilled at a limit price
// while the market runs away from it is not a saving, it is the exact scenario
// the stop existed to prevent.
func planExecution(
	cfg config.ExecutionConfig,
	order broker.Order,
	price float64,
	urgent bool,
) execPlan {
	if urgent || price <= 0 {
		return execPlan{orders: []broker.Order{order}}
	}

	children := sliceOrder(cfg, order, price)

	if !wantsLimit(cfg) {
		note := ""
		if len(children) > 1 {
			note = fmt.Sprintf("market in %d slices", len(children))
		}

		return execPlan{orders: children, note: note}
	}

	limitPx := limitPrice(cfg, order.Side, price)
	for i := range children {
		children[i].Type = broker.Limit
		children[i].LimitPx = limitPx
	}

	note := fmt.Sprintf("limit %.4f", limitPx)
	if len(children) > 1 {
		note += fmt.Sprintf(" in %d slices", len(children))
	}

	return execPlan{orders: children, note: note}
}

// wantsLimit reports whether limit execution is configured.
func wantsLimit(cfg config.ExecutionConfig) bool { return cfg.Type == "limit" }

// limitPrice returns the passive limit price for a side: below the reference for
// a buy, above it for a sell. Being passive is the whole point — a limit order
// at or through the touch just pays the spread with extra steps.
func limitPrice(cfg config.ExecutionConfig, side broker.Side, price float64) float64 {
	off := cfg.LimitOffsetPct
	if off < 0 {
		off = -off
	}

	if side == broker.Buy {
		return price * (1 - off)
	}

	return price * (1 + off)
}

// sliceOrder splits an order into child clips of at most SliceNotional each.
// Returns the original as a single child when slicing is disabled or the order
// is already small enough.
func sliceOrder(cfg config.ExecutionConfig, order broker.Order, price float64) []broker.Order {
	notional := order.Qty * price
	if cfg.SliceNotional <= 0 || notional <= cfg.SliceNotional || price <= 0 {
		return []broker.Order{order}
	}

	maxSlices := cfg.MaxSlices
	if maxSlices <= 0 {
		maxSlices = defaultMaxSlices
	}

	n := int(notional/cfg.SliceNotional) + 1
	if n > maxSlices {
		n = maxSlices
	}

	if n < 2 {
		return []broker.Order{order}
	}

	per := order.Qty / float64(n)
	out := make([]broker.Order, 0, n)
	placed := 0.0

	for i := range n {
		qty := per
		if i == n-1 {
			qty = order.Qty - placed // the last clip absorbs any rounding remainder
		}

		if qty <= 0 {
			break
		}

		child := order

		child.Qty = qty
		out = append(out, child)

		placed += qty
	}

	return out
}

// submitPlan sends a plan's child orders to the broker, one at a time, each
// passing the order-rate guard. It returns the first child's result (whose ID
// and status represent the parent), the total confirmed-filled quantity, the
// total quantity whose outcome the broker did not confirm (see
// broker.OrderResult.Terminal) — market-type clips only — and whether at
// least one clip is a resting limit order still awaiting resolution:
// watchLimitOrder owns that resolution, so it is neither filled nor
// unconfirmed-in-the-anomalous-sense, and a caller deciding whether it has a
// *complete* picture of the order's outcome (e.g. whether to clear the
// pendingSell dedupe guard) must not treat "not unconfirmed" as "done" while
// this is true.
//
// A failure on the *first* child is a failed order. A failure partway through
// is not: some quantity is already working at the venue, and pretending the
// whole thing failed would leave the engine's books disagreeing with the
// broker's. Those are logged and the accepted quantity is reported honestly.
//
// price is the reference price used to size the plan (before any per-clip
// limit price); a Buy clip reserves its estimated cash cost against
// risk.Manager's headroom for as long as its outcome stays unknown — see
// risk.Manager.Reserve. Sell clips never reserve: Authorize always allows
// sells regardless of cash headroom, by design.
func (e *Engine) submitPlan(
	ctx context.Context,
	plan execPlan,
	symbol string,
	price float64,
) (first broker.OrderResult, filled, unconfirmed float64, resting bool, err error) {
	for i, child := range plan.orders {
		if !e.guardOrderRate(true) {
			break // circuit breaker tripped; stop sending the remaining clips
		}

		var resID uint64
		if child.Side == broker.Buy {
			refPrice := child.LimitPx
			if refPrice <= 0 {
				refPrice = price
			}

			resID = e.rm.ReserveBuy(child.Symbol, child.Qty, refPrice)
		}

		res, err := e.brk.SubmitOrder(ctx, child)
		if err != nil {
			e.rm.Release(resID) // never reached the venue; nothing to hold headroom for

			if i == 0 {
				return broker.OrderResult{}, 0, 0, false, err
			}

			e.log.Warn("slice submit failed; earlier slices are already working",
				"symbol", symbol, "slice", i+1, "of", len(plan.orders), "err", err)

			break
		}

		e.met.onOrder() // count only orders the broker accepted

		switch qty, confirmed := settledQty(res); {
		case confirmed:
			filled += qty
			// Settled one way or the other (filled, or a confirmed
			// zero) — the reservation's only job, holding headroom
			// while pending, is done; the caller books the real spend
			// via RecordBuy.
			e.rm.Release(resID)

			// ...except that "confirmed" is not always "finished".
			// settledQty also confirms a PARTIAL fill the broker
			// reported while the order is still working (FilledQty > 0
			// with Terminal false — Alpaca and Trading 212 both return
			// that at their fill-wait deadline, since partially_filled
			// is a working status for both). The filled part is booked
			// above; the remainder is still resting at the venue and
			// needs the same timeout/cancel/fallback treatment as any
			// other resting clip, or limit_timeout and market_fallback
			// simply would not apply to it.
			//
			// The watcher is handed the REMAINDER, not the original
			// clip: everything it does downstream is sized from
			// order.Qty — the market fallback re-sends it, and
			// flagUnconfirmed reports it — and it is only the unfilled
			// part that is still in play. Re-sending the whole clip
			// would buy the partial a second time (or, on an exit,
			// try to sell stock that is already gone). The reservation
			// is re-made for the remainder alone for the same reason.
			if e.watchRemainder(ctx, res, child, qty, price) {
				resting = true
			} else if rem := workingRemainder(child, res, qty); rem > 0 {
				// A market clip's working remainder: flagged by the caller
				// as unconfirmed, its reservation re-made and parked with
				// it — fail closed while unknown (EN3-3).
				unconfirmed += rem

				if child.Side == broker.Buy {
					e.parkUnconfirmedReservation(symbol, e.rm.ReserveBuy(symbol, rem, price))
				}
			}

		case child.Type == broker.Limit:
			resting = true // genuinely pending, not anomalous — watchLimitOrder owns it
			// resID deliberately stays reserved: watchLimitOrder releases
			// it once this clip's fate is known.
			//
			// The watcher is spawned HERE and in the partial-fill case
			// above — the two branches where the clip still has quantity
			// working — rather than for every Limit-typed clip. A limit
			// order the broker confirmed at submission (Sim
			// fills a marketable one instantly; Trading 212 posts every
			// order to its market endpoint and confirms the fill inline)
			// used to get a watcher too — which woke after limit_timeout,
			// found the order long gone from the working set and called
			// flagUnconfirmed on a fill that had already been booked.
			// That invented an "unknown" TradeRecord for a completed order
			// and, through hasUnconfirmedBuy, blocked every further buy of
			// that symbol until someone cleared it by hand.
			go e.watchLimitOrder(ctx, res, child, resID)

		default:
			// A market order the broker could not confirm one way or the other —
			// genuinely anomalous; the caller must flag it rather than guess.
			// Leave resID reserved rather than guess at the outcome — see
			// risk.Manager.Reserve's fails-closed guidance — parked so the
			// flagged order can release it once reconciled.
			unconfirmed += child.Qty

			e.parkUnconfirmedReservation(symbol, resID)
		}

		if i == 0 {
			first = res
		}
	}

	return first, filled, unconfirmed, resting, nil
}

// settledQty reports the quantity a fill should be booked at, and whether the
// broker's result is confirmed enough to book at all.
//
// Every real broker here fills asynchronously and gives up waiting after a
// bounded window. broker.OrderResult.Terminal distinguishes a confirmed
// outcome — genuinely filled, or genuinely rejected/cancelled with a real
// zero fill — from one the broker could not confirm either way (still
// working past the deadline, or an ambiguous status like Trading 212's
// 404-while-polling). Only a confirmed outcome is booked: a confirmed zero
// must NOT fall back to the authorised quantity (that turns a rejected order
// into a phantom position), and an unconfirmed result must not be booked at
// all (that would silently invent or silently drop a real position). The
// caller reports an unconfirmed result to the operator instead of guessing —
// see Engine.flagUnconfirmed.
func settledQty(res broker.OrderResult) (qty float64, confirmed bool) {
	if res.Terminal {
		return res.FilledQty, true
	}

	if res.FilledQty > 0 {
		return res.FilledQty, true // a broker-reported partial fill mid-poll is a real observation
	}

	return 0, false
}

// workingRemainder is the part of a MARKET order still working after the
// broker confirmed a partial fill at its wait deadline (FilledQty > 0,
// Terminal false — what Alpaca and Trading 212 return). settledQty books the
// filled part; the rest is neither filled nor refused, and was dropped from
// every bookkeeping path: no watcher, no unconfirmed order, its reservation
// released. A buy remainder that later filled never counted against
// max_daily_spend; a sell remainder was sold AGAIN next evaluation while it
// still worked (EN3-3). Limit clips have their own path (watchRemainder, LS-6).
func workingRemainder(order broker.Order, res broker.OrderResult, filled float64) float64 {
	if res.Terminal || order.Type == broker.Limit {
		return 0
	}

	if rem := order.Qty - filled; rem > sellDustQty {
		return rem
	}

	return 0
}

// limitTimeout returns the configured working time for a limit order.
func limitTimeout(cfg config.ExecutionConfig) time.Duration {
	if cfg.LimitTimeout > 0 {
		return cfg.LimitTimeout
	}

	return defaultLimitTimeout
}

// watchRemainder starts a watcher for the still-working part of a limit clip
// the broker reported PARTIALLY filled, and reports whether it started one.
//
// It exists because "the broker confirmed a quantity" and "the order is
// finished" are different questions. settledQty confirms a partial fill on an
// order that is still working (FilledQty > 0 with Terminal false — what both
// Alpaca and Trading 212 return at their fill-wait deadline, since
// partially_filled is a working status for each). The filled part is booked by
// the caller; what is left is an ordinary resting clip and needs the same
// timeout, cancel and market-fallback treatment as any other, or limit_timeout
// and market_fallback simply would not apply to it.
//
// The watcher is handed the REMAINDER rather than the original clip.
// Everything downstream of it is sized from order.Qty — the market fallback
// re-sends that quantity, flagUnconfirmed reports it — and only the unfilled
// part is still in play. Handing it the whole clip bought the already-filled
// part a second time, or on an exit tried to sell stock that was already gone.
// The reservation is re-made for the remainder alone for the same reason. See
// CODE_REVIEW_FINDINGS.md's LS-6.
func (e *Engine) watchRemainder(
	ctx context.Context,
	res broker.OrderResult,
	child broker.Order,
	filledQty, price float64,
) bool {
	rem := child.Qty - filledQty
	if res.Terminal || child.Type != broker.Limit || rem <= 0 {
		return false
	}

	remainder := child

	remainder.Qty = rem

	var remID uint64

	if child.Side == broker.Buy {
		remPrice := child.LimitPx
		if remPrice <= 0 {
			remPrice = price
		}

		remID = e.rm.ReserveBuy(child.Symbol, rem, remPrice)
	}

	go e.watchLimitOrder(ctx, res, remainder, remID)

	return true
}

// watchLimitOrder cancels a limit order that has not filled within the timeout
// and, when configured, re-sends the unfilled remainder as a market order.
//
// It runs in its own goroutine: the evaluation loop must not sit waiting for a
// resting order. Everything it does afterwards goes through the normal record
// and sync paths, so a fallback fill is indistinguishable from any other fill.
//
// A resting limit order was never booked at submission (see settledQty), so
// this function is the only place its eventual outcome gets recorded.
//
// resID is the reservation submitPlan made against risk.Manager's headroom
// for this clip (0 for a sell, which never reserves). It is released the
// moment this clip's fate becomes definitively known (a confirmed cancel
// with no fallback, or a fallback that fails to submit) or once RecordBuy
// books its real spend (a fallback that fills). Every other return path
// here is genuinely ambiguous about whether the order filled — those
// deliberately leave resID reserved rather than guess; see
// risk.Manager.Reserve's fails-closed guidance.
func (e *Engine) watchLimitOrder(ctx context.Context, res broker.OrderResult, order broker.Order, resID uint64) {
	defer e.contain("limit-order watcher " + order.Symbol)

	cfg := e.executionConfig()

	untrack := e.trackWatcher(order.Symbol)
	defer untrack()

	// The broker the order was SENT to, held for the watcher's whole life.
	// Re-reading e.brk at wake-up let a test-mode switch during the wait
	// point a real order's watcher at the simulated broker (which cannot
	// cancel, so the real order was abandoned) or a simulated order's at the
	// real one (which does not know its id, so a phantom unconfirmed buy was
	// flagged and blocked real buys of the symbol) — EN2-9.
	brk := e.currentBroker()

	timer := time.NewTimer(limitTimeout(cfg))
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}

	// Gone from the working set without us cancelling it — could mean it
	// filled, or was cancelled by something else (the venue, the broker's own
	// duration expiry, a human). This adapter gives no way to tell, so it must
	// be flagged rather than guessed at either way.
	if !orderStillOpen(ctx, brk, res.ID) {
		e.flagUnconfirmed(order.Symbol, order.Side, order.Qty, res,
			fmt.Sprintf("limit %.4f left the working set before the %s timeout with no confirmed outcome",
				order.LimitPx, limitTimeout(cfg)), resID)

		return
	}

	canceller, ok := brk.(broker.OrderCanceller)
	if !ok {
		e.log.Warn("broker cannot cancel orders; leaving the limit order working",
			"symbol", order.Symbol, "order_id", res.ID, "broker", brk.Name())

		return
	}

	if err := canceller.CancelOrders(ctx, res.ID); err != nil {
		e.log.Warn("cancel expired limit order failed",
			"symbol", order.Symbol, "order_id", res.ID, "err", err)

		return
	}

	// A cancel that succeeds is the broker's confirmation that (at least) some
	// quantity was still unfilled and working. It is NOT a confirmation that
	// nothing filled: a limit order can fill partially at any point of its
	// working life, and OpenOrder carries no filled quantity. Re-sending the
	// whole clip after a 60/100 partial bought 160 and left the 60 unbooked
	// (EN2-5, the half the LS-6 fix did not reach). See filledWhileResting for
	// how the filled part is measured, and when it cannot be.
	e.rm.Release(resID) // confirmed: the working part of this clip will never fill

	e.record(TradeRecord{
		Time: now(), Symbol: order.Symbol, Side: order.Side, Qty: 0,
		Price: order.LimitPx, Status: "cancelled", OrderID: res.ID,
		Reason: fmt.Sprintf("limit %.4f unfilled after %s", order.LimitPx, limitTimeout(cfg)),
	})

	// Held across sync → measure → book, so sibling slices of one plan (they
	// share limit_timeout and wake together) each see the others' bookings
	// rather than all claiming the same excess.
	e.restMu.Lock()
	defer e.restMu.Unlock()

	e.syncPortfolio(ctx)

	remaining := order.Qty

	if order.Side == broker.Buy {
		filled, known := e.filledWhileResting(order)
		if !known {
			// Ambiguous: another slice of this symbol is resting or
			// unconfirmed, so the broker's excess cannot be pinned on this
			// clip. Re-sending the whole clip is the over-buy EN2-5 is about;
			// sending nothing under-buys, which is safe — any shares that did
			// fill are untracked holdings that adopt picks up.
			e.record(TradeRecord{
				Time: now(), Symbol: order.Symbol, Side: order.Side, Qty: order.Qty,
				Price: order.LimitPx, Status: "blocked",
				Reason: "market fallback skipped: another order for this symbol is in flight, " +
					"so a partial fill of this limit could not be measured",
			})

			return
		}

		if filled > 0 {
			e.rm.RecordBuy(order.LimitPx * filled)
			e.persistSpend()
			e.bookFill(order, filled, order.LimitPx, "partial", res.ID,
				fmt.Sprintf("limit %.4f partially filled before the %s timeout", order.LimitPx, limitTimeout(cfg)))
		}

		remaining -= filled
	}

	// The sell side of the same measurement. A resting sell that partly
	// filled left the record at its old size: no TradeRecord or realized
	// P&L for the shares that sold, the protections never saw the round
	// trip end, and pendingExposure overlaid the phantom onto equity for ten
	// minutes — EN3-1's false-drawdown mechanism (EN3-5). When nothing else
	// for the symbol is working, the record's excess over the holding is
	// what this clip sold.
	if order.Side == broker.Sell {
		if sold, known := e.soldWhileResting(order); known && sold > 0 {
			e.bookFill(order, sold, order.LimitPx, "partial", res.ID,
				fmt.Sprintf("limit %.4f partially filled before the %s timeout", order.LimitPx, limitTimeout(cfg)))
		}
	}

	if !cfg.MarketFallback {
		e.syncPortfolio(ctx)

		return
	}

	// A sell's fallback is capped at the holding inside marketFallback, which
	// is all a partial sale needs — the shares that sold are already gone.
	e.marketFallback(ctx, brk, order, remaining)
}

// trackWatcher counts a live limit watcher for symbol (see filledWhileResting)
// and returns the func that uncounts it.
func (e *Engine) trackWatcher(symbol string) func() {
	e.mu.Lock()

	if e.restingWatchers == nil {
		e.restingWatchers = make(map[string]int)
	}

	e.restingWatchers[symbol]++

	e.mu.Unlock()

	return func() {
		e.mu.Lock()
		defer e.mu.Unlock()

		e.restingWatchers[symbol]--

		if e.restingWatchers[symbol] <= 0 {
			delete(e.restingWatchers, symbol)
		}
	}
}

// filledWhileResting measures how much of a cancelled resting limit BUY filled
// before the cancel, from the excess of the broker's holding over the engine's
// openPos record (which never contains a resting clip — it is only booked once
// confirmed). The result is clamped to [0, order.Qty].
//
// known is false when the excess cannot be attributed to this order: another
// limit watcher for the symbol is still live, or a buy of it is unconfirmed —
// either could own those shares. It is only measured for buys. On the sell
// side the same arithmetic reads broker-report lag after a recent buy (the
// record ahead of the snapshot, what pendingFillGrace absorbs) as a sale and
// would book a phantom close and realized P&L, so a partial sale is left to
// reconcile and the fallback is only capped at the holding.
// soldWhileResting is filledWhileResting for a resting SELL: the record's
// excess over the broker's holding, capped at the clip, known only when no
// sibling slice or unconfirmed sell of the symbol could own the difference.
func (e *Engine) soldWhileResting(order broker.Order) (sold float64, known bool) {
	if e.countUnconfirmed(order.Symbol, broker.Sell) > 0 {
		return 0, false
	}

	held := 0.0
	if h, ok := e.pf.Snapshot().Holding(order.Symbol); ok {
		held = h.Qty
	}

	e.mu.RLock()

	siblings := e.restingWatchers[order.Symbol] > 1

	recorded := 0.0
	if p, ok := e.openPos[order.Symbol]; ok {
		recorded = p.Qty
	}

	e.mu.RUnlock()

	if siblings {
		return 0, false
	}

	if deficit := recorded - held; deficit > sellDustQty {
		return math.Min(deficit, order.Qty), true
	}

	return 0, true
}

func (e *Engine) filledWhileResting(order broker.Order) (filled float64, known bool) {
	if e.hasUnconfirmedBuy(order.Symbol) {
		return 0, false
	}

	held := 0.0
	if h, ok := e.pf.Snapshot().Holding(order.Symbol); ok {
		held = h.Qty
	}

	e.mu.RLock()

	siblings := e.restingWatchers[order.Symbol] > 1

	recorded := 0.0
	if p, ok := e.openPos[order.Symbol]; ok {
		recorded = p.Qty
	}

	e.mu.RUnlock()

	if siblings {
		return 0, false
	}

	if excess := held - recorded; excess > sellDustQty {
		return math.Min(excess, order.Qty), true
	}

	return 0, true
}

// marketFallback re-sends the unfilled remainder of a cancelled limit order as
// a market order.
//
// A fallback BUY is a new commitment of money made up to limit_timeout after
// the decision that sized it, so it passes the same gates any buy does at the
// moment it is sent: the trading toggle (a drawdown halt, the kill switch or a
// breaker trip in the meantime must stop it), the order-rate breaker, and a
// fresh risk.Authorize against current headroom (EN2-4). A fallback SELL is
// an exit and is never gated — it is only capped at what is still held, since
// whatever sold while the limit rested is already gone.
//
//nolint:cyclop // gate → submit → confirm → book, mirroring act()
func (e *Engine) marketFallback(ctx context.Context, brk broker.Broker, order broker.Order, qty float64) {
	market := order

	market.Type = broker.Market
	market.LimitPx = 0
	market.Qty = qty

	held := 0.0
	if h, ok := e.pf.Snapshot().Holding(order.Symbol); ok {
		held = h.Qty
	}

	if market.Side == broker.Sell {
		market.Qty = math.Min(market.Qty, held)
	}

	if market.Qty <= sellDustQty {
		e.syncPortfolio(ctx)

		return
	}

	refPx := order.LimitPx
	if px, ok := e.prices.Get(order.Symbol); ok && px > 0 {
		refPx = px
	}

	var fbResID uint64

	if market.Side == broker.Buy {
		authOrder, ok := e.gateFallbackBuy(market, refPx)
		if !ok {
			return
		}

		market = authOrder
		fbResID = e.rm.ReserveBuy(market.Symbol, market.Qty, refPx)
	} else {
		e.guardOrderRate(false) // counted, never refused: exits are not gated
	}

	fallback, err := brk.SubmitOrder(ctx, market)
	if err != nil {
		e.rm.Release(fbResID) // never reached the venue

		e.record(TradeRecord{
			Time: now(), Symbol: order.Symbol, Side: order.Side, Qty: market.Qty,
			Status: "error", Reason: "market fallback after unfilled limit", Err: err.Error(),
		})

		return
	}

	e.met.onOrder()

	if order.Side == broker.Sell {
		// The HELD quantity, as every other site records it — the clip size
		// read as "not landed yet" for as long as the remaining holding was
		// at least one clip, blocking later exits (EN2-3).
		e.markPendingSell(order.Symbol, held)
	}

	filledQty, confirmed := settledQty(fallback)
	if !confirmed {
		// Ambiguous — leave fbResID reserved rather than guess.
		e.flagUnconfirmed(order.Symbol, order.Side, market.Qty, fallback,
			"market fallback after unfilled limit | broker did not confirm this order's outcome", fbResID)
		e.syncPortfolio(ctx)

		return
	}

	if order.Side == broker.Sell {
		e.clearPendingSell(order.Symbol) // outcome known; same rule as act
	}

	fillPx := fallback.FilledPx
	if fillPx <= 0 {
		fillPx = refPx
	}

	if order.Side == broker.Buy {
		e.rm.RecordBuy(fillPx * filledQty)
		e.rm.Release(fbResID) // real spend now booked; the estimate can go
		e.persistSpend()
	}

	if filledQty > 0 {
		e.bookFill(market, filledQty, fillPx, filledOr(fallback.Status, filledQty), fallback.ID,
			"market fallback after unfilled limit")
	}

	e.syncPortfolio(ctx)
}

// gateFallbackBuy runs a fallback BUY through the gates any buy passes at the
// moment it is sent — see marketFallback — and returns the authorised order.
// A refusal is recorded; the breaker records its own halt.
func (e *Engine) gateFallbackBuy(market broker.Order, refPx float64) (broker.Order, bool) {
	blocked := func(reason string) {
		e.record(TradeRecord{
			Time: now(), Symbol: market.Symbol, Side: market.Side, Qty: market.Qty,
			Price: refPx, Status: "blocked", Reason: "market fallback after unfilled limit | " + reason,
		})
	}

	if !e.tradingOn() {
		blocked("trading is disabled")

		return broker.Order{}, false
	}

	authOrder, decision := e.rm.Authorize(market, refPx, e.pendingExposure(e.pf.Snapshot()))
	if !decision.Approved {
		blocked(decision.Reason)

		return broker.Order{}, false
	}

	if !e.guardOrderRate(true) {
		return broker.Order{}, false
	}

	return authOrder, true
}

// bookFill records a confirmed fill of qty at fillPx and books it into the
// open-position record — the tail every fill path shares once the outcome is
// known. Spend accounting (RecordBuy) stays with the caller, which knows
// which reservation the fill settles.
func (e *Engine) bookFill(order broker.Order, qty, fillPx float64, status, orderID, reason string) {
	indSnap := e.indicatorSnapshot(order.Symbol)

	var outcome *realised

	if order.Side == broker.Sell {
		if r, ok := e.closePnL(order.Symbol, fillPx, qty); ok {
			outcome = &r
		}
	}

	rec := TradeRecord{
		Time: now(), Symbol: order.Symbol, Side: order.Side, Qty: qty,
		Price: fillPx, Value: fillPx * qty,
		Status: status, OrderID: orderID, Reason: reason, Indicators: indSnap,
	}

	outcome.apply(&rec)
	e.record(rec)

	if order.Side == broker.Buy {
		e.recordOpen(order.Symbol, qty, fillPx, indSnap)
	} else {
		e.recordClose(order.Symbol, qty)
	}
}

// orderStillOpen reports whether an order is still working at the broker. A
// broker that cannot list working orders is assumed to still have it, so the
// cancel is attempted rather than skipped.
func orderStillOpen(ctx context.Context, brk broker.Broker, id string) bool {
	lister, ok := brk.(broker.OrderLister)
	if !ok {
		return true
	}

	open, err := lister.OpenOrders(ctx)
	if err != nil {
		return true
	}

	for _, o := range open {
		if o.OrderID == id {
			return true
		}
	}

	return false
}

// executionConfig returns the current execution settings.
func (e *Engine) executionConfig() config.ExecutionConfig {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.cfg.Execution
}

// currentBroker returns the broker orders go to right now — the simulated one
// while test mode is active.
func (e *Engine) currentBroker() broker.Broker {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.brk
}

// planningExecutionConfig is the execution config a NEW order is planned with:
// limit execution degrades to market on a broker that cannot both list and
// cancel orders.
//
// A resting limit needs a watcher that can see it and cancel it. Without one
// (Alpaca here, and the simulated broker in test mode) the watcher logged
// "cannot cancel" and returned: an expired limit SELL left its pendingSell
// marker equal to the holding forever, blocking strategy sells and stop-losses
// for the symbol (EN2-6), and a buy's reservation was never released, filled
// or not, so cash was counted twice until buys starved. In test mode the leak
// went into the REAL risk.Manager, and a watcher waking after the window
// checked the real broker and flagged a phantom unconfirmed buy (EN2-9).
// Recorded once per broker so the setting is not silently ineffective.
func (e *Engine) planningExecutionConfig() config.ExecutionConfig {
	cfg := e.executionConfig()
	if !wantsLimit(cfg) {
		return cfg
	}

	brk := e.currentBroker()

	_, lists := brk.(broker.OrderLister)
	_, cancels := brk.(broker.OrderCanceller)

	if lists && cancels {
		return cfg
	}

	e.mu.Lock()

	if e.limitDegraded == nil {
		e.limitDegraded = make(map[string]bool)
	}

	first := !e.limitDegraded[brk.Name()]

	e.limitDegraded[brk.Name()] = true

	e.mu.Unlock()

	if first {
		errlog.Recordf("engine", "execution.type is \"limit\" but broker %q cannot list and cancel orders, "+
			"so a resting limit could never be managed: orders are sent to market instead", brk.Name())
	}

	cfg.Type = "market"

	return cfg
}
