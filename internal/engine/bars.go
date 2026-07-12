package engine

import (
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// barBuilder aggregates live ticks into fixed-interval OHLCV candles. Completed
// bars are appended to history; the in-progress bar is exposed so indicators can
// use the freshest price without waiting for the bar to close.
type barBuilder struct {
	symbol   string
	interval time.Duration
	cur      *market.Candle
	curStart time.Time
}

func newBarBuilder(symbol string, interval time.Duration) *barBuilder {
	if interval <= 0 {
		interval = time.Minute
	}
	return &barBuilder{symbol: symbol, interval: interval}
}

// add applies a tick. It returns a completed candle (and true) when the tick
// rolls over into a new bar period.
func (b *barBuilder) add(price float64, ts time.Time) (market.Candle, bool) {
	bucket := ts.Truncate(b.interval)
	var completed market.Candle
	done := false

	if b.cur == nil {
		b.start(bucket, price)
		return completed, false
	}
	if bucket.After(b.curStart) {
		completed = *b.cur
		done = true
		b.start(bucket, price)
		return completed, done
	}

	// Same bucket: update OHLCV.
	if price > b.cur.High {
		b.cur.High = price
	}
	if price < b.cur.Low {
		b.cur.Low = price
	}
	b.cur.Close = price
	b.cur.Volume++
	return completed, false
}

func (b *barBuilder) start(start time.Time, price float64) {
	b.curStart = start
	b.cur = &market.Candle{
		Symbol: b.symbol,
		Open:   price,
		High:   price,
		Low:    price,
		Close:  price,
		Volume: 1,
		Time:   start,
	}
}

// current returns the in-progress bar, if any.
func (b *barBuilder) current() (market.Candle, bool) {
	if b.cur == nil {
		return market.Candle{}, false
	}
	return *b.cur, true
}
