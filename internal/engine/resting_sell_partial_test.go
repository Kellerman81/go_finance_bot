package engine

import (
	"context"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
)

// EN3-5: a resting limit SELL of 100 that filled 60 before its timeout was
// never booked. The fallback sold the 40 still held, recordClose(40) left a
// 60-share record for stock that was gone, and the 60 never got realized P&L —
// so the round trip never closed for the protections, and pendingExposure
// overlaid the phantom onto equity.
func TestARestingSellsPartialFillIsBooked(t *testing.T) {
	var sent []broker.Order

	brk := cancellingBroker(&sent, []broker.Position{{Symbol: "X", Qty: 40, Current: 100}})
	brk.fallback = broker.OrderResult{ID: "ord-2", Terminal: true, FilledQty: 40, FilledPx: 100, Status: "filled"}
	e := watchLimitEngine(brk)
	e.cfg.Execution.MarketFallback = true
	e.openPos["X"] = &OpenPosition{Symbol: "X", Qty: 100, OrigQty: 100, EntryPrice: 90}

	order := broker.Order{Symbol: "X", Side: broker.Sell, Qty: 100, Type: broker.Limit, LimitPx: 100}
	e.watchLimitOrder(context.Background(), broker.OrderResult{ID: "ord-1"}, order, 0)

	booked := 0.0

	for _, tr := range e.Trades() {
		if tr.Side == broker.Sell && tr.PnL != nil {
			booked += tr.Qty
		}
	}

	e.mu.RLock()
	p, open := e.openPos["X"]
	e.mu.RUnlock()

	if open {
		t.Errorf("a %.0f-share record remains after the position fully exited", p.Qty)
	}

	if booked != 100 {
		t.Errorf("realized sells booked for %.0f of 100 shares", booked)
	}
}
