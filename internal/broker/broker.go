// Package broker abstracts order execution. The Broker interface is implemented
// by an Alpaca paper/live adapter and an internal simulated broker.
package broker

import (
	"context"
	"errors"
	"time"
)

// ErrInstrumentUnavailable reports that the venue will not trade an instrument
// for this account at all — not now, not in an hour, not on Monday. It is a
// property of the instrument and the account rather than of the order or the
// moment, so a caller that retries is guaranteed to fail again.
//
// The usual cause on an EU account is a US-domiciled ETF: PRIIPs requires a Key
// Information Document that US issuers do not produce, so the instrument
// resolves, prices and charts perfectly well — it simply cannot be bought. The
// bot would otherwise re-send the order on every evaluation, forever.
//
// Brokers wrap their permanent instrument-level refusals in this; callers test
// with errors.Is.
var ErrInstrumentUnavailable = errors.New("instrument not tradeable on this account")

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
//
// Terminal reports whether Status/FilledQty are a confirmed, final outcome —
// genuinely filled (FilledQty > 0), or genuinely rejected/cancelled with a
// confirmed zero fill — rather than "still working" or "the broker couldn't
// say either way" (a poll that hit its deadline, an ambiguous status, or a
// request error mid-poll). Callers must not book a position or realized P&L
// from a non-Terminal result: FilledQty on a non-Terminal result is whatever
// was last observed, not a confirmed fact, and a zero there does NOT mean
// "nothing happened" — it may just mean the broker hasn't said yet.
type OrderResult struct {
	ID          string    `json:"id"`
	Symbol      string    `json:"symbol"`
	Side        Side      `json:"side"`
	Qty         float64   `json:"qty"`
	FilledQty   float64   `json:"filled_qty"`
	FilledPx    float64   `json:"filled_avg_price"`
	Status      string    `json:"status"`
	SubmittedAt time.Time `json:"submitted_at"`
	Terminal    bool      `json:"terminal"`
}

// Position is a currently held position as reported by the broker.
//
// AvgPrice, Current and MarketValue are already converted to the account's
// preferred currency where the broker adapter knows how (e.g. Trading 212
// positions in a foreign-listed instrument). Currency and FXRate describe
// that conversion so a caller re-pricing from a live feed (which quotes in
// the instrument's native currency) can apply the same rate; FXRate is 1 (or
// the zero value, which callers should treat as 1) when no conversion was
// needed or the broker doesn't track it.
type Position struct {
	Symbol       string  `json:"symbol"`
	Qty          float64 `json:"qty"`
	AvgPrice     float64 `json:"avg_entry_price"`
	Current      float64 `json:"current_price"`
	MarketValue  float64 `json:"market_value"`
	UnrealizedPL float64 `json:"unrealized_pl"`
	Currency     string  `json:"currency,omitempty"` // instrument's native trading currency, if known
	FXRate       float64 `json:"fx_rate,omitempty"`  // Currency -> account currency multiplier (0 == 1, no conversion)
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

// MarketScheduler answers, from broker-provided exchange schedules, whether a
// symbol's venue is in a regular trading session at a given time — more accurate
// than a suffix-inferred calendar because it reflects the instrument's real
// exchange, holidays and breaks. Brokers that expose schedules implement it;
// callers fall back to their own calendar when known is false.
type MarketScheduler interface {
	Broker
	// TradingOpen reports whether symbol's exchange is open for regular trading
	// at t, allowing preOpen before the session open. known is false when the
	// broker cannot answer for this symbol (schedule not loaded, symbol unmapped,
	// or t outside the known window) — the caller should then fall back.
	TradingOpen(symbol string, t time.Time, preOpen time.Duration) (open, known bool)
}

// CurrencyResolver reports an instrument's native trading currency and the
// multiplier to convert 1 unit of it into the broker's preferred currency —
// the same conversion GetPositions/GetAccount/SubmitOrder already apply
// internally, exposed so a caller with its own price feed (which quotes in
// the instrument's native currency, not the account's) can convert prices
// the same way *before* combining them with broker-reported money values.
// Brokers whose instruments are always in the account's own currency still
// implement this; the rate is just always 1.
type CurrencyResolver interface {
	Broker
	// InstrumentCurrency returns symbol's native trading currency and the
	// FX multiplier to the broker's preferred currency (1 if they're the
	// same, unknown, or no preferred currency is configured).
	InstrumentCurrency(ctx context.Context, symbol string) (currency string, fxRate float64, err error)
}

// AssetClassifier reports what KIND of instrument a symbol is — a share, a
// tracker fund, a bond, an option — from the broker's own instrument metadata.
//
// A broker implements it when its instrument list carries a type at all; the
// simulator does not, and callers must treat its absence as "unknown", never as
// a default class. The answer is in the venue's own vocabulary ("Etf",
// "STOCK", "ContractFutures"); market.NormalizeAssetClass maps it onto the
// canonical names the portfolio allocation caps are keyed by.
//
// This is a fallback behind the data provider's own search: the broker knows
// the instrument it trades, which is exactly the symbol the provider may have
// no entry for.
type AssetClassifier interface {
	Broker
	// AssetClass returns symbol's instrument type in the broker's own
	// vocabulary. An unknown symbol is an error, not an empty string with a
	// nil error — the caller must be able to tell "not classified" from
	// "classified as nothing".
	AssetClass(ctx context.Context, symbol string) (string, error)
}
