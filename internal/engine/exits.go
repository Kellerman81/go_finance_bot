package engine

import (
	"context"
	"fmt"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
)

// checkExits enforces protective exit rules (stop-loss, take-profit, trailing
// stop) on every open position. It runs before strategy signals so a triggered
// exit always wins. Exits respect the trading toggle: when trading is disabled
// they are recorded as dry-run. The toggle is re-read per position (not passed
// in) so that a circuit-breaker trip mid-loop downgrades the remaining exits to
// dry-run instead of re-tripping the breaker per symbol.
func (e *Engine) checkExits(ctx context.Context) {
	if !e.exits.Enabled {
		return
	}
	snap := e.pf.Snapshot()
	for sym, h := range snap.Holdings {
		if h.Qty <= sellDustQty {
			continue // empty or dust-only; nothing sellable
		}
		price, ok := e.prices.Get(sym)
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
		var posSnap OpenPosition
		var persist bool
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

		reason := ""
		switch {
		case e.exits.StopLossPct > 0 && price <= entry*(1-e.exits.StopLossPct):
			reason = fmt.Sprintf("stop-loss %.2f<=%.2f (-%.1f%%)", price, entry, e.exits.StopLossPct*100)
		case e.exits.TakeProfitPct > 0 && price >= entry*(1+e.exits.TakeProfitPct):
			reason = fmt.Sprintf("take-profit %.2f>=%.2f (+%.1f%%)", price, entry, e.exits.TakeProfitPct*100)
		case e.exits.TrailingStopPct > 0 && peak > entry && price <= peak*(1-e.exits.TrailingStopPct):
			reason = fmt.Sprintf("trailing-stop %.2f<=peak %.2f (-%.1f%%)", price, peak, e.exits.TrailingStopPct*100)
		}
		if reason == "" {
			continue
		}
		e.exitPosition(ctx, sym, h.Qty, price, reason, e.tradingOn(), snap)
	}
}

// exitPosition liquidates a held position via a market sell, passing through
// the risk manager and respecting the trading toggle. snap is the portfolio
// snapshot the caller already holds (checkExits takes one for the whole sweep).
func (e *Engine) exitPosition(ctx context.Context, symbol string, qty, price float64, reason string, tradingEnabled bool, snap portfolio.Snapshot) {
	// Same resubmit guard as strategy sells: while a prior sell for this
	// position is unconfirmed at the broker (async fill), the holding still
	// shows up in the snapshot and the exit would trigger again every
	// evaluation — don't stack duplicate sell orders.
	if pending, ok := e.pendingSellQty(symbol); ok && qty >= pending {
		return
	}
	order := broker.Order{Symbol: symbol, Side: broker.Sell, Qty: qty, Type: broker.Market}
	authOrder, decision := e.rm.Authorize(order, price, snap)
	if !decision.Approved {
		return // nothing sellable
	}

	indSnap := e.indicatorSnapshot(symbol)
	tag := "EXIT " + reason
	if !tradingEnabled {
		e.record(TradeRecord{
			Time: now(), Symbol: symbol, Side: broker.Sell, Qty: authOrder.Qty,
			Price: price, Value: decision.EstValue, Status: "dry-run", Reason: tag,
			Indicators: indSnap,
		})
		return
	}

	if !e.guardOrderRate(true) {
		return // circuit breaker tripped
	}
	e.met.onOrder()
	res, err := e.brk.SubmitOrder(ctx, authOrder)
	if err != nil {
		e.record(TradeRecord{
			Time: now(), Symbol: symbol, Side: broker.Sell, Qty: authOrder.Qty,
			Price: price, Status: "error", Reason: tag, Err: err.Error(),
			Indicators: indSnap,
		})
		return
	}
	fillPx := res.FilledPx
	if fillPx <= 0 {
		fillPx = price
	}
	var pnlVal, pnlPctVal *float64
	var win *bool
	var entryIndicators string
	if pv, pp, ei, ok := e.closePnL(symbol, fillPx, res.FilledQty); ok {
		pnlVal, pnlPctVal, win, entryIndicators = &pv, &pp, winFromPnL(pv), ei
	}
	e.record(TradeRecord{
		Time: now(), Symbol: symbol, Side: broker.Sell, Qty: res.FilledQty,
		Price: fillPx, Value: fillPx * res.FilledQty, Status: orStr(res.Status, "filled"),
		Reason: tag, OrderID: res.ID,
		Indicators: indSnap, PnL: pnlVal, PnLPct: pnlPctVal, Win: win, EntryIndicators: entryIndicators,
	})
	e.recordClose(symbol, qty) // remove the open-position record (also clears the peak)
	e.markPendingSell(symbol, qty)
	e.syncPortfolio(ctx)
}
