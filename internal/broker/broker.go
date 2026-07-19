// Package broker abstracts order execution. The Broker interface is implemented
// by an Alpaca paper/live adapter and an internal simulated broker.
package broker

import (
	"context"
	"time"
)

// Side is the direction of an order.
type Side string

const (
	Buy  Side = "buy"
	Sell Side = "sell"
)

// OrderType is the order execution type.
type OrderType string

const (
	Market OrderType = "market"
	Limit  OrderType = "limit"
)

// Order is a request to trade.
type Order struct {
	Symbol   string    `json:"symbol"`
	Side     Side      `json:"side"`
	Qty      float64   `json:"qty"`
	Type     OrderType `json:"type"`
	LimitPx  float64   `json:"limit_price,omitempty"`
	Notional float64   `json:"notional,omitempty"` // intended dollar value, informational
}

// OrderResult is the outcome of submitting an order.
type OrderResult struct {
	ID          string    `json:"id"`
	Symbol      string    `json:"symbol"`
	Side        Side      `json:"side"`
	Qty         float64   `json:"qty"`
	FilledQty   float64   `json:"filled_qty"`
	FilledPx    float64   `json:"filled_avg_price"`
	Status      string    `json:"status"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// Position is a currently held position as reported by the broker.
type Position struct {
	Symbol       string  `json:"symbol"`
	Qty          float64 `json:"qty"`
	AvgPrice     float64 `json:"avg_entry_price"`
	Current      float64 `json:"current_price"`
	MarketValue  float64 `json:"market_value"`
	UnrealizedPL float64 `json:"unrealized_pl"`
}

// Account is a snapshot of broker account balances.
type Account struct {
	Cash        float64 `json:"cash"`
	Equity      float64 `json:"equity"`
	BuyingPower float64 `json:"buying_power"`
	Currency    string  `json:"currency"`
}

// Broker executes orders and reports account/position state.
type Broker interface {
	// Name identifies the broker adapter (e.g. "sim", "alpaca").
	Name() string
	// SubmitOrder places an order and reports its (possibly confirmed) fill.
	SubmitOrder(ctx context.Context, o Order) (OrderResult, error)
	// GetAccount returns the account balances.
	GetAccount(ctx context.Context) (Account, error)
	// GetPositions returns the currently held positions.
	GetPositions(ctx context.Context) ([]Position, error)
}

// BracketOrder is an entry order with optional attached protective legs: a
// stop-loss and/or a take-profit. The protective legs trade the opposite side
// of the entry, in the same quantity, and form an OCO pair once the entry fills
// (whichever triggers first cancels the other).
type BracketOrder struct {
	Symbol     string
	Side       Side // entry direction
	Qty        float64
	EntryType  OrderType // Market or Limit
	EntryPrice float64   // required when EntryType == Limit
	StopLoss   float64   // protective stop price (0 = omit)
	TakeProfit float64   // protective take-profit price (0 = omit)
	Duration   string    // "DayOrder" | "GoodTillCancel" (default GoodTillCancel)
}

// BracketResult holds the ids of the orders created by a bracket placement.
type BracketResult struct {
	EntryID      string        `json:"entry_id"`
	StopID       string        `json:"stop_id,omitempty"`
	TakeProfitID string        `json:"take_profit_id,omitempty"`
	Orders       []OrderResult `json:"orders"`
}

// OrderModification changes price and/or quantity of a working order.
type OrderModification struct {
	OrderID string
	Symbol  string // used to resolve the instrument
	Side    Side
	Type    OrderType
	Qty     float64 // new amount
	Price   float64 // new price (limit/stop)
}

// OpenOrder is a working (unfilled) order as reported by the broker.
type OpenOrder struct {
	OrderID  string  `json:"order_id"`
	Symbol   string  `json:"symbol"`
	Side     Side    `json:"side"`
	Type     string  `json:"type"`
	Qty      float64 `json:"qty"`
	Price    float64 `json:"price"`
	Status   string  `json:"status"`
	Duration string  `json:"duration"`
}

// Capability interfaces — brokers implement the subset they support, and
// callers type-assert for the specific capability and degrade gracefully when
// it is absent. (Trading 212 lists/cancels orders but has no native brackets;
// Saxo supports all of them.)

// OrderLister lists working (unfilled) orders.
type OrderLister interface {
	Broker
	// OpenOrders returns the account's working (unfilled) orders.
	OpenOrders(ctx context.Context) ([]OpenOrder, error)
}

// OrderCanceller cancels working orders by id.
type OrderCanceller interface {
	Broker
	// CancelOrders cancels the working orders with the given ids.
	CancelOrders(ctx context.Context, ids ...string) error
}

// OrderModifier changes a working order's price/quantity.
type OrderModifier interface {
	Broker
	// ModifyOrder applies m to a working order and reports the updated order.
	ModifyOrder(ctx context.Context, m OrderModification) (OrderResult, error)
}

// SymbolResolver resolves an input symbol (e.g. LXS.DE or an ISIN) to the
// broker's tradeable ticker, reporting whether it is orderable.
type SymbolResolver interface {
	Broker
	// ResolveSymbol maps symbol to the broker's tradeable ticker.
	ResolveSymbol(ctx context.Context, symbol string) (ticker string, err error)
}

// BracketBroker places native entry + stop-loss + take-profit orders.
type BracketBroker interface {
	Broker
	// PlaceBracket places b's entry with its protective OCO legs attached.
	PlaceBracket(ctx context.Context, b BracketOrder) (BracketResult, error)
}

// AdvancedBroker is the union of all order capabilities (e.g. Saxo).
type AdvancedBroker interface {
	OrderLister
	OrderCanceller
	OrderModifier
	BracketBroker
}
