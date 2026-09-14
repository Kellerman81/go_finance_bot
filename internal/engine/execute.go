package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
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

			resID = e.rm.ReserveBuy(child.Qty, refPrice)
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
			// risk.Manager.Reserve's fails-closed guidance.
			unconfirmed += child.Qty
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

		remID = e.rm.ReserveBuy(rem, remPrice)
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
	cfg := e.executionConfig()

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
	if !e.orderStillOpen(ctx, res.ID) {
		e.flagUnconfirmed(order.Symbol, order.Side, order.Qty, res,
			fmt.Sprintf("limit %.4f left the working set before the %s timeout with no confirmed outcome",
				order.LimitPx, limitTimeout(cfg)))

		return
	}

	canceller, ok := e.brk.(broker.OrderCanceller)
	if !ok {
		e.log.Warn("broker cannot cancel orders; leaving the limit order working",
			"symbol", order.Symbol, "order_id", res.ID, "broker", e.brk.Name())

		return
	}

	if err := canceller.CancelOrders(ctx, res.ID); err != nil {
		e.log.Warn("cancel expired limit order failed",
			"symbol", order.Symbol, "order_id", res.ID, "err", err)

		return
	}

	// A cancel that succeeds is the broker's confirmation that (at least) some
	// quantity was still unfilled and working — booked as a confirmed zero
	// fill for the limit leg, consistent with never having booked it at
	// submission. If a partial fill happened in the narrow window before the
	// cancel took effect, the next syncPortfolio reconciles it against the
	// broker's own position report.
	e.rm.Release(resID) // confirmed: this clip will never fill

	e.record(TradeRecord{
		Time: now(), Symbol: order.Symbol, Side: order.Side, Qty: 0,
		Price: order.LimitPx, Status: "cancelled", OrderID: res.ID,
		Reason: fmt.Sprintf("limit %.4f unfilled after %s", order.LimitPx, limitTimeout(cfg)),
	})

	if !cfg.MarketFallback {
		e.syncPortfolio(ctx)

		return
	}

	// The limit leg is assumed to have filled nothing (see above), so the
	// fallback re-sends the full original quantity, not a remainder.
	market := order

	market.Type = broker.Market
	market.LimitPx = 0

	var fbResID uint64
	if market.Side == broker.Buy {
		fbResID = e.rm.ReserveBuy(market.Qty, order.LimitPx)
	}

	fallback, err := e.brk.SubmitOrder(ctx, market)
	if err != nil {
		e.rm.Release(fbResID) // never reached the venue

		e.record(TradeRecord{
			Time: now(), Symbol: order.Symbol, Side: order.Side, Qty: order.Qty,
			Status: "error", Reason: "market fallback after unfilled limit", Err: err.Error(),
		})

		return
	}

	e.met.onOrder()

	if order.Side == broker.Sell {
		e.markPendingSell(order.Symbol, order.Qty)
	}

	filledQty, confirmed := settledQty(fallback)
	if !confirmed {
		// Ambiguous — leave fbResID reserved rather than guess.
		e.flagUnconfirmed(order.Symbol, order.Side, order.Qty, fallback,
			"market fallback after unfilled limit | broker did not confirm this order's outcome")
		e.syncPortfolio(ctx)

		return
	}

	fillPx := fallback.FilledPx
	if fillPx <= 0 {
		fillPx = order.LimitPx
	}

	indSnap := e.indicatorSnapshot(order.Symbol)

	value := fillPx * filledQty
	if order.Side == broker.Buy {
		e.rm.RecordBuy(value)
		e.rm.Release(fbResID) // real spend now booked; the estimate can go
		e.persistSpend()
	}

	var outcome *realised

	if order.Side == broker.Sell {
		if r, ok := e.closePnL(order.Symbol, fillPx, filledQty); ok {
			outcome = &r
		}
	}

	rec := TradeRecord{
		Time: now(), Symbol: order.Symbol, Side: order.Side, Qty: filledQty,
		Price: fillPx, Value: value,
		Status: filledOr(fallback.Status, filledQty), OrderID: fallback.ID,
		Reason: "market fallback after unfilled limit", Indicators: indSnap,
	}

	outcome.apply(&rec)
	e.record(rec)

	if order.Side == broker.Buy {
		e.recordOpen(order.Symbol, filledQty, fillPx, indSnap)
	} else {
		e.recordClose(order.Symbol, filledQty)
	}

	e.syncPortfolio(ctx)
}

// orderStillOpen reports whether an order is still working at the broker. A
// broker that cannot list working orders is assumed to still have it, so the
// cancel is attempted rather than skipped.
func (e *Engine) orderStillOpen(ctx context.Context, id string) bool {
	lister, ok := e.brk.(broker.OrderLister)
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
