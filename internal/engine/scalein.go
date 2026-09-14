package engine

import (
	"context"
	"fmt"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// Scaling into a losing position.
//
// This is averaging down, and it deserves the warning its config carries: it
// improves the entry price of every trade that recovers and increases the size
// of every trade that does not. What makes it survivable rather than a
// martingale is the combination enforced here — a hard cap on adds, an add size
// that cannot exceed one ordinary order, and a stop-loss that still applies to
// the enlarged position, so the worst case stays bounded.
//
// It is a separate pass from the signal path on purpose. AllowPyramiding adds on
// repeated BUY signals (the strategy still likes it); this adds on drawdown (the
// strategy has not been asked). Conflating them would make one switch mean two
// very different things.

// checkScaleIn adds to open positions that have moved far enough against their
// average entry, up to the configured cap.
//
// A fresh snapshot is fetched for every candidate, not once before the loop:
// addToPosition's own Authorize call books each add synchronously
// (recordOpen updates e.openPos before the loop moves to the next symbol),
// so a snapshot taken once up front would not reflect earlier adds made
// during this same pass — every candidate after the first would authorise
// against a max_total_invested headroom that doesn't yet know about them,
// and multiple positions crossing their rung on the same cycle could
// collectively blow through the cap. See CODE_REVIEW_FINDINGS.md's "Scale-in
// authorizes every position add against one stale pre-loop snapshot".
//
//nolint:cyclop // one "is this add allowed?" guard per branch
func (e *Engine) checkScaleIn(ctx context.Context) {
	cfg := e.ScaleInConfig()
	if !cfg.Enabled || cfg.MaxAdds <= 0 || cfg.StepPct <= 0 || cfg.SizePct <= 0 {
		return
	}

	for _, p := range e.OpenPositions() {
		price, ok := e.prices.Get(p.Symbol)
		if !ok || price <= 0 {
			continue
		}

		if !e.scaleInDue(p, price) {
			continue
		}

		if cfg.RequireSignal && e.signalFor(p.Symbol) != strategy.Buy {
			continue // the price fell, but the strategy no longer likes it
		}

		snap := e.pendingExposure(e.pf.Snapshot())

		e.addToPosition(ctx, p.Symbol, price, snap)
	}
}

// scaleInDue reports whether the next rung of the ladder is reached: the
// position is below its cap on adds and the price is at least step_pct under the
// AVERAGE entry. Measuring against the average (which each add pulls down) means
// every further add needs a further fall, so the ladder tightens on itself
// instead of firing repeatedly on one slide.
func (e *Engine) scaleInDue(p OpenPosition, price float64) bool {
	cfg := e.ScaleInConfig()
	if p.Adds >= cfg.MaxAdds || p.EntryPrice <= 0 || p.Qty <= 0 {
		return false
	}

	return price <= p.EntryPrice*(1-cfg.StepPct)
}

// addToPosition places one scale-in buy, through every gate a fresh entry passes:
// the protections, the concentration caps, the venue-price check and the money
// limits. An add is a new order putting new money at risk and is not exempt from
// any of them.
func (e *Engine) addToPosition(
	ctx context.Context,
	symbol string,
	price float64,
	snap portfolio.Snapshot,
) {
	cfg := e.ScaleInConfig()
	indSnap := e.indicatorSnapshot(symbol)

	blocked := func(reason string) {
		e.record(TradeRecord{
			Time: now(), Symbol: symbol, Side: broker.Buy, Price: price,
			Status: "blocked", Reason: "SCALE-IN " + reason, Indicators: indSnap,
		})
	}

	// First, and silently: a closed venue or an instrument the broker refuses
	// is not news. act() skips both with a debug line for exactly that reason,
	// and this path used to run them LAST — after the venue-price check, which
	// on a closed venue cannot confirm a price and recorded a blocked trade
	// saying so. Every eval_interval. All weekend.
	if why, ok := e.entryGates(symbol); !ok {
		e.log.Debug("scale-in skipped", "symbol", symbol, "reason", why)

		return
	}

	if ok, reason := e.Protections().AllowEntry(symbol); !ok {
		blocked(reason)

		return
	}

	confirmed, reason, ok := e.confirmVenuePrice(symbol, price)
	if !ok {
		blocked(reason)

		return
	}

	price = confirmed

	// Sized from the same headroom-then-risk path as an opening buy, then cut
	// to the ladder's step fraction. Refused sizing is only logged here: a
	// scale-in is an optional add, and the entry path already records the
	// same condition for this symbol once — recording it again per add would
	// be the second copy of a message the operator has already seen.
	size := e.sizeBuy(symbol, price)
	if !size.OK {
		e.log.Debug("scale-in skipped", "symbol", symbol, "reason", size.Reason)

		return
	}

	notional := size.Notional * cfg.SizePct
	if ok, reason := e.portfolioGuard().AllowEntry(symbol, snap, notional); !ok {
		blocked(reason)

		return
	}

	tag := fmt.Sprintf("SCALE-IN add %d/%d at %.2f (avg entry down %.1f%%)",
		e.addsFor(symbol)+1, cfg.MaxAdds, price, cfg.StepPct*100)

	order := broker.Order{
		Symbol: symbol, Side: broker.Buy, Qty: notional / price, Type: broker.Market,
	}

	authOrder, decision := e.rm.Authorize(order, price, snap)
	if !decision.Approved {
		blocked(decision.Reason)

		return
	}

	if !e.tradingOn() {
		e.record(TradeRecord{
			Time: now(), Symbol: symbol, Side: broker.Buy, Qty: authOrder.Qty,
			Price: price, Value: decision.EstValue, Status: "dry-run", Reason: tag,
			Indicators: indSnap,
		})

		return
	}

	if !e.guardOrderRate(true) {
		return
	}

	// Hold this add's estimated cash cost against risk.Manager's headroom
	// for the round trip to the broker — see risk.Manager.Reserve.
	resID := e.rm.ReserveBuy(authOrder.Qty, price)

	res, err := e.brk.SubmitOrder(ctx, authOrder)
	if err != nil {
		e.rm.Release(resID)
		e.noteUntradeable(symbol, broker.Buy, err)
		e.record(TradeRecord{
			Time: now(), Symbol: symbol, Side: broker.Buy, Qty: authOrder.Qty,
			Price: price, Status: "error", Reason: tag, Err: err.Error(),
			Indicators: indSnap,
		})

		return
	}

	e.met.onOrder()

	bought, isSettled := settledQty(res)
	if !isSettled {
		// Ambiguous — leave resID reserved rather than guess; see
		// risk.Manager.Reserve's fails-closed guidance.
		e.flagUnconfirmed(symbol, broker.Buy, authOrder.Qty, res, tag+" | broker did not confirm this order's outcome")
		e.syncPortfolio(ctx)

		return
	}

	e.rm.Release(resID)

	fillPx := res.FilledPx
	if fillPx <= 0 {
		fillPx = price
	}

	// RecordBuy so this add counts toward max_daily_spend like every other
	// buy path does — previously missing entirely, found while auditing
	// this function for the cash-reservation finding: a scale-in add spent
	// real cash but never registered against the daily budget, so repeated
	// adds could blow through max_daily_spend while Authorize kept seeing
	// spentToday as if none of them had happened.
	e.rm.RecordBuy(fillPx * bought)
	e.persistSpend()

	e.record(TradeRecord{
		Time: now(), Symbol: symbol, Side: broker.Buy, Qty: bought,
		Price: fillPx, Value: fillPx * bought, Status: filledOr(res.Status, bought),
		Reason: tag, OrderID: res.ID, Indicators: indSnap,
	})
	e.recordOpen(symbol, bought, fillPx, indSnap) // also bumps Adds
	e.syncPortfolio(ctx)
}

// addsFor returns how many scale-in adds a position has taken.
func (e *Engine) addsFor(symbol string) int {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if p, ok := e.openPos[symbol]; ok {
		return p.Adds
	}

	return 0
}

// signalFor returns the latest recorded signal action for symbol, or Hold when
// none has been evaluated yet.
func (e *Engine) signalFor(symbol string) strategy.Action {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if s, ok := e.signals[symbol]; ok {
		return s.Action
	}

	return strategy.Hold
}
