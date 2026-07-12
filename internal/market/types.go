// Package market defines the data feed abstraction and concrete providers.
// The DataProvider interface lets the engine consume live ticks and historical
// candles without binding to a specific vendor.
package market

import (
	"context"
	"time"
)

// Quote is a single live trade tick for a symbol.
type Quote struct {
	Symbol string    `json:"symbol"`
	Price  float64   `json:"price"`
	Volume float64   `json:"volume"`
	Time   time.Time `json:"time"`
}

// Candle is an OHLCV bar used for indicator computation.
type Candle struct {
	Symbol string    `json:"symbol"`
	Open   float64   `json:"open"`
	High   float64   `json:"high"`
	Low    float64   `json:"low"`
	Close  float64   `json:"close"`
	Volume float64   `json:"volume"`
	Time   time.Time `json:"time"`
}

// Resolution describes a candle bar size for historical requests.
type Resolution string

const (
	Res1Min  Resolution = "1"
	Res5Min  Resolution = "5"
	Res15Min Resolution = "15"
	Res1Hour Resolution = "60"
	Res1Day  Resolution = "D"
)

// DataProvider is implemented by every market data feed.
type DataProvider interface {
	// Subscribe registers symbols for live streaming. It may be called
	// multiple times to add symbols.
	Subscribe(symbols ...string) error
	// Unsubscribe stops streaming the given symbols.
	Unsubscribe(symbols ...string) error
	// Quotes returns the channel of live ticks. The channel is closed when
	// the provider is closed.
	Quotes() <-chan Quote
	// LastQuote returns the most recent tick seen for symbol, and whether one
	// exists. It is an O(1) snapshot of the live stream, so consumers that only
	// need the latest price avoid a Candles round-trip.
	LastQuote(symbol string) (Quote, bool)
	// Candles fetches historical bars for indicator warmup. The context bounds
	// the request; providers should honour cancellation/deadline.
	Candles(ctx context.Context, symbol string, res Resolution, from, to time.Time) ([]Candle, error)
	// Close releases all resources and stops streaming.
	Close() error
}
