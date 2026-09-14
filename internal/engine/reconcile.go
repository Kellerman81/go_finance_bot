package engine

import (
	"fmt"
	"slices"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
)

// UnconfirmedOrder is an order whose fill outcome the broker did not confirm
// — see broker.OrderResult.Terminal. It was deliberately NOT booked into
// openPos, realized P&L, or the daily-spend ledger, because the engine does
// not know whether it filled, filled partially, or never filled at all.
//
// An operator must reconcile it against the broker (check the broker's own
// order history / current positions) and clear it with ClearUnconfirmed —
// the engine will not do this automatically, since guessing either way is
// exactly the failure mode this exists to avoid.
type UnconfirmedOrder struct {
	Symbol  string      `json:"symbol"`
	Side    broker.Side `json:"side"`
	Qty     float64     `json:"qty"` // the authorised quantity; the actual fill (if any) is unknown
	OrderID string      `json:"order_id"`
	Status  string      `json:"status"`
	Time    time.Time   `json:"time"`
	Reason  string      `json:"reason"`
}

// flagUnconfirmed records an order the broker did not confirm and fires a
// loud alert via the trade log (see Engine.alert — "unknown" status maps to
// a halt-priority notification) rather than silently booking a guess. It
// returns the trade record it wrote, so a caller that reports back to an
// operator (e.g. ManualOrder) can hand it back too.
func (e *Engine) flagUnconfirmed(symbol string, side broker.Side, qty float64, res broker.OrderResult, reason string) TradeRecord {
	id := res.ID
	if id == "" {
		id = fmt.Sprintf("%s-%s-%d", symbol, side, time.Now().UnixNano())
	}

	u := UnconfirmedOrder{
		Symbol: symbol, Side: side, Qty: qty, OrderID: id,
		Status: res.Status, Time: now(), Reason: reason,
	}

	e.mu.Lock()
	e.unconfirmed[id] = u
	e.mu.Unlock()

	rec := TradeRecord{
		Time: u.Time, Symbol: symbol, Side: side, Qty: qty,
		Status: "unknown", Reason: reason, OrderID: res.ID,
	}
	e.record(rec)

	return rec
}

// hasUnconfirmedBuy reports whether an unconfirmed buy is outstanding for
// symbol — a guard against submitting a second buy while the broker hasn't
// said whether the first one filled. Unconfirmed buys are deliberately not
// booked into openPos (see settledQty), so the ordinary pyramid guard can't
// see them on its own.
func (e *Engine) hasUnconfirmedBuy(symbol string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()

	for _, u := range e.unconfirmed {
		if u.Symbol == symbol && u.Side == broker.Buy {
			return true
		}
	}

	return false
}

// Unconfirmed returns outstanding unconfirmed orders, oldest first.
func (e *Engine) Unconfirmed() []UnconfirmedOrder {
	e.mu.RLock()
	defer e.mu.RUnlock()

	out := make([]UnconfirmedOrder, 0, len(e.unconfirmed))
	for _, u := range e.unconfirmed {
		out = append(out, u)
	}

	slices.SortFunc(out, func(a, b UnconfirmedOrder) int { return a.Time.Compare(b.Time) })

	return out
}

// ClearUnconfirmed removes an unconfirmed order once an operator has
// reconciled it against the broker by hand. It reports whether an entry with
// that order ID existed.
func (e *Engine) ClearUnconfirmed(orderID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, ok := e.unconfirmed[orderID]; !ok {
		return false
	}

	delete(e.unconfirmed, orderID)

	return true
}
