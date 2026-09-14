package engine

import (
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// maxFillSpan bounds how much silence is filled with flat bars.
//
// Within a trading session, a gap means nobody traded — the instrument still
// existed at the last price, and a bar belongs there. Across a session break it
// means the venue was shut, and manufacturing bars for a closed market would be
// a lie that also floods the ring: 17 hours of overnight silence is 1020 fake
// one-minute bars, a third of the history the engine keeps.
//
// 90 minutes is comfortably longer than any plausible run of quiet minutes in an
// open market and far shorter than any close, so the two cases separate cleanly
// without needing an exchange calendar here.
//
// Deliberately wall-clock rather than a bar count, and deliberately NOT scaled
// by b.interval: "is the venue shut?" is a question about elapsed time, not
// about how many bars the answer happens to span. The consequence is that
// coarse bar intervals get little or no filling — on hourly bars only a
// single missing bar is ever filled, and at bar_interval >= 90m none is — so
// a thin instrument on coarse bars can still produce unevenly spaced history.
// That is a real limitation (see CODE_REVIEW_FINDINGS.md's EN-3 note) and an
// accepted one: filling a 3-hour hourly gap would as often manufacture bars
// across a lunch break or an auction as across genuine illiquidity, and
// TestFillScalesWithTheInterval pins the current behaviour.
const maxFillSpan = 90 * time.Minute

// barBuilder aggregates live ticks into fixed-interval OHLCV candles. Completed
// bars are appended to history; the in-progress bar is exposed so indicators can
// use the freshest price without waiting for the bar to close.
type barBuilder struct {
	symbol   string
	interval time.Duration
	cur      *market.Candle
	curStart time.Time
}

// newBarBuilder creates a builder for fixed-interval bars (default 1 minute).
func newBarBuilder(symbol string, interval time.Duration) *barBuilder {
	if interval <= 0 {
		interval = time.Minute
	}

	return &barBuilder{symbol: symbol, interval: interval}
}

// add applies a tick and returns the bars it completed, oldest first.
//
// volume is the tick's own trade size when the provider reports one (e.g.
// Finnhub's trade messages), or 0 when it doesn't (e.g. Binance's ticker-price
// poll, which has no per-tick size) — a zero falls back to counting the tick
// itself, same as before this parameter existed. Without this distinction,
// EVERY live bar's volume was a tick count regardless of what the feed
// actually knew, permanently understating true share volume by orders of
// magnitude on any provider that reports it, and silently killing any
// detector gated on a realistic volume reading (see
// CODE_REVIEW_FINDINGS.md CRIT-13). This does not fully solve CRIT-13 — a
// tick-count fallback is still not real volume — but it makes real volume
// flow through wherever the feed actually has it, instead of being
// discarded unconditionally.
//
// It returns more than one when the feed went quiet: a bar is only closed by the
// arrival of a later tick, so an instrument that trades ten times an hour used to
// produce ten bars an hour and call them one-minute bars. That is not a display
// problem. Every indicator period in this bot is counted in BARS — trend_period
// is documented as "a week of 1-minute bars", the EMAs are 12 and 26 bars, ATR
// is 14 — and on a thin instrument those silently became a week and a half, or a
// month. Worse, warmup seeds from the provider's candles, which DO include every
// interval, so a symbol's history changed shape partway along and the backtester
// (fed evenly spaced bars) measured a different strategy than the live engine ran.
//
// So a quiet interval now produces a flat bar at the last price with zero volume,
// which is what every market-data provider does and what the backtester already
// assumes.
func (b *barBuilder) add(price, volume float64, ts time.Time) []market.Candle {
	bucket := ts.Truncate(b.interval)

	if b.cur == nil {
		b.start(bucket, price, volume)

		return nil
	}

	if bucket.Before(b.curStart) {
		// A tick for a bar that has already been closed and pushed. Feeds that
		// carry the venue's own trade timestamp (Finnhub batches trades and
		// stamps them with the exchange clock) can deliver these out of order,
		// and folding one into the CURRENT bar made a stale price that bar's
		// close and dragged its high/low — the bar it actually belongs to is
		// gone, so there is nowhere honest to put it. See
		// CODE_REVIEW_FINDINGS.md's EN-4 finding.
		return nil
	}

	if !bucket.After(b.curStart) {
		// Same bucket: update OHLCV.
		if price > b.cur.High {
			b.cur.High = price
		}

		if price < b.cur.Low {
			b.cur.Low = price
		}

		b.cur.Close = price
		b.cur.Volume += tickVolume(volume)

		return nil
	}

	completed := *b.cur
	filled := b.fillGap(completed.Close, bucket)

	out := make([]market.Candle, 0, len(filled)+1)

	out = append(out, completed)
	out = append(out, filled...)

	b.start(bucket, price, volume)

	return out
}

// tickVolume is the amount one tick contributes to a bar's volume: the
// provider's own reported trade size when it has one, or 1 (a tick count) as
// the fallback for providers that report none.
func tickVolume(reported float64) float64 {
	if reported > 0 {
		return reported
	}

	return 1
}

// fillGap returns flat bars for the intervals between the bar just closed and
// the one starting at next. It returns nothing when the silence is long enough
// to be a closed market rather than an illiquid one.
func (b *barBuilder) fillGap(lastClose float64, next time.Time) []market.Candle {
	gap := next.Sub(b.curStart)
	if gap <= b.interval || gap > maxFillSpan {
		return nil
	}

	missing := int(gap/b.interval) - 1

	out := make([]market.Candle, 0, missing)

	for i := 1; i <= missing; i++ {
		out = append(out, market.Candle{
			Symbol: b.symbol,
			Time:   b.curStart.Add(time.Duration(i) * b.interval),
			Open:   lastClose,
			High:   lastClose,
			Low:    lastClose,
			Close:  lastClose,
			// Zero volume is the honest record and the signal a reader needs:
			// nothing traded in this interval.
			Volume: 0,
		})
	}

	return out
}

// start begins a new in-progress bar at the given bucket start, price and
// first tick's volume contribution (see tickVolume).
func (b *barBuilder) start(start time.Time, price, volume float64) {
	b.curStart = start
	b.cur = &market.Candle{
		Symbol: b.symbol,
		Open:   price,
		High:   price,
		Low:    price,
		Close:  price,
		Volume: tickVolume(volume),
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
