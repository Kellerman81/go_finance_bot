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

	// resIDs are the risk.Manager reservations this order still holds. They
	// stay held while the outcome is unknown — fail closed — and are released
	// when an operator reconciles the order (ClearUnconfirmed). Never
	// released, an unconfirmed buy that DID fill was counted twice against
	// max_total_invested (once in the snapshot, once reserved) until restart.
	resIDs []uint64
}

// flagUnconfirmed records an order the broker did not confirm and fires a
// loud alert via the trade log (see Engine.alert — "unknown" status maps to
// a halt-priority notification) rather than silently booking a guess. It
// returns the trade record it wrote, so a caller that reports back to an
// operator (e.g. ManualOrder) can hand it back too.
//
// resIDs are the reservations the order still holds; reservations a sliced
// plan parked for the symbol (parkUnconfirmedReservation) are taken too.
func (e *Engine) flagUnconfirmed(
	symbol string, side broker.Side, qty float64, res broker.OrderResult, reason string, resIDs ...uint64,
) TradeRecord {
	id := res.ID
	if id == "" {
		id = fmt.Sprintf("%s-%s-%d", symbol, side, time.Now().UnixNano())
	}

	e.mu.Lock()

	held := slices.Concat(e.parkedRes[symbol], resIDs)
	delete(e.parkedRes, symbol)

	u := UnconfirmedOrder{
		Symbol: symbol, Side: side, Qty: qty, OrderID: id,
		Status: res.Status, Time: now(), Reason: reason, resIDs: held,
	}

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
	return e.countUnconfirmed(symbol, broker.Buy) > 0
}

// countUnconfirmed counts symbol's outstanding unconfirmed orders on side.
func (e *Engine) countUnconfirmed(symbol string, side broker.Side) int {
	e.mu.RLock()
	defer e.mu.RUnlock()

	n := 0

	for _, u := range e.unconfirmed {
		if u.Symbol == symbol && u.Side == side {
			n++
		}
	}

	return n
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
//
// The order's reservations are released with it: reconciling by hand is the
// moment its outcome becomes known, and whatever filled is in the broker's
// snapshot from then on.
func (e *Engine) ClearUnconfirmed(orderID string) bool {
	e.mu.Lock()

	u, ok := e.unconfirmed[orderID]
	if ok {
		delete(e.unconfirmed, orderID)
	}

	e.mu.Unlock()

	for _, id := range u.resIDs {
		if e.rm == nil {
			break
		}

		e.rm.Release(id)
	}

	return ok
}

// parkUnconfirmedReservation holds a sliced plan's unconfirmed clip
// reservation for symbol until the caller flags the order (act does so right
// after submitPlan returns).
func (e *Engine) parkUnconfirmedReservation(symbol string, resID uint64) {
	if resID == 0 {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.parkedRes == nil {
		e.parkedRes = make(map[string][]uint64)
	}

	e.parkedRes[symbol] = append(e.parkedRes[symbol], resID)
}
