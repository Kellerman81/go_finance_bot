package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
)

// AdvancedSupported reports whether the active broker supports the full set of
// bracket + modify + cancel + list capabilities.
func (e *Engine) AdvancedSupported() bool {
	_, ok := e.brk.(broker.AdvancedBroker)
	return ok
}

// PlaceBracket submits an entry order with attached stop-loss/take-profit legs.
// The entry leg passes through the risk manager (money limits apply and may
// clamp the size) and the order-rate guard before reaching the broker.
func (e *Engine) PlaceBracket(ctx context.Context, b broker.BracketOrder) (broker.BracketResult, error) {
	ab, ok := e.brk.(broker.BracketBroker)
	if !ok {
		return broker.BracketResult{}, fmt.Errorf("broker %q does not support bracket orders", e.brk.Name())
	}

	price := b.EntryPrice
	if price <= 0 {
		if p, ok := e.prices.Get(b.Symbol); ok {
			price = p
		}
	}
	if price <= 0 {
		return broker.BracketResult{}, fmt.Errorf("no price for %s to size the order", b.Symbol)
	}

	snap := e.pf.Snapshot()
	if b.Side == broker.Buy {
		snap = e.pendingExposure(snap)
	}
	authOrder, decision := e.rm.Authorize(
		broker.Order{Symbol: b.Symbol, Side: b.Side, Qty: b.Qty, Type: broker.Market}, price, snap)
	if !decision.Approved {
		e.record(TradeRecord{Time: time.Now(), Symbol: b.Symbol, Side: b.Side, Price: price,
			Status: "blocked", Reason: "bracket: " + decision.Reason})
		return broker.BracketResult{}, fmt.Errorf("blocked by risk: %s", decision.Reason)
	}
	b.Qty = authOrder.Qty

	if !e.guardOrderRate(false) {
		return broker.BracketResult{}, fmt.Errorf("order rate limited")
	}
	e.met.onOrder()
	res, err := ab.PlaceBracket(ctx, b)
	if err != nil {
		e.record(TradeRecord{Time: time.Now(), Symbol: b.Symbol, Side: b.Side, Qty: b.Qty,
			Price: price, Status: "error", Reason: "bracket", Err: err.Error()})
		return res, err
	}
	if b.Side == broker.Buy {
		e.rm.RecordBuy(b.Qty * price)
		e.persistSpend()
	}
	e.record(TradeRecord{Time: time.Now(), Symbol: b.Symbol, Side: b.Side, Qty: b.Qty,
		Price: price, Value: b.Qty * price, Status: "bracket-submitted", OrderID: res.EntryID,
		Reason: fmt.Sprintf("bracket entry, stop=%.2f tp=%.2f", b.StopLoss, b.TakeProfit)})
	e.syncPortfolio(ctx)
	return res, nil
}

// ModifyOrder changes a working order via the broker.
func (e *Engine) ModifyOrder(ctx context.Context, m broker.OrderModification) (broker.OrderResult, error) {
	ab, ok := e.brk.(broker.OrderModifier)
	if !ok {
		return broker.OrderResult{}, fmt.Errorf("broker %q does not support order modify", e.brk.Name())
	}
	res, err := ab.ModifyOrder(ctx, m)
	if err != nil {
		return res, err
	}
	e.record(TradeRecord{Time: time.Now(), Symbol: m.Symbol, Side: m.Side, Qty: m.Qty,
		Price: m.Price, Status: "modified", Reason: "modify " + m.OrderID, OrderID: res.ID})
	return res, nil
}

// CancelOrders cancels working orders via the broker.
func (e *Engine) CancelOrders(ctx context.Context, ids ...string) error {
	ab, ok := e.brk.(broker.OrderCanceller)
	if !ok {
		return fmt.Errorf("broker %q does not support order cancel", e.brk.Name())
	}
	if err := ab.CancelOrders(ctx, ids...); err != nil {
		return err
	}
	e.record(TradeRecord{Time: time.Now(), Status: "cancelled", Reason: "cancel " + strings.Join(ids, ",")})
	return nil
}

// Orderable reports whether a watchlist symbol can be ordered on the broker.
type Orderable struct {
	Symbol    string `json:"symbol"`
	Orderable bool   `json:"orderable"`
	Ticker    string `json:"ticker,omitempty"`
	Note      string `json:"note,omitempty"`
}

// CheckOrderable resolves every watched symbol against the broker and reports
// which ones can be ordered (and to which broker ticker).
func (e *Engine) CheckOrderable(ctx context.Context) ([]Orderable, error) {
	sr, ok := e.brk.(broker.SymbolResolver)
	if !ok {
		return nil, fmt.Errorf("broker %q does not support symbol resolution", e.brk.Name())
	}
	syms := e.Watchlist()
	out := make([]Orderable, 0, len(syms))
	for _, s := range syms {
		ticker, err := sr.ResolveSymbol(ctx, s)
		if err != nil {
			out = append(out, Orderable{Symbol: s, Orderable: false, Note: err.Error()})
			continue
		}
		out = append(out, Orderable{Symbol: s, Orderable: true, Ticker: ticker})
	}
	return out, nil
}

// OpenOrders lists working orders from the broker.
func (e *Engine) OpenOrders(ctx context.Context) ([]broker.OpenOrder, error) {
	ab, ok := e.brk.(broker.OrderLister)
	if !ok {
		return nil, fmt.Errorf("broker %q does not support listing orders", e.brk.Name())
	}
	return ab.OpenOrders(ctx)
}
