package market

import "time"

// Resample aggregates finer-grained candles into the target resolution's bar
// size (e.g. 1-minute into 5-minute or hourly bars), for multi-timeframe
// analysis. Bars are bucketed by truncating each candle's time to the target
// interval; within a bucket open is the first bar's open, close the last, high/low
// the extremes and volume the sum. Input is assumed chronologically ordered.
// Candles already at or coarser than the target pass through unchanged in shape
// (one bucket per bar) — Resample never upsamples.
func Resample(candles []Candle, target Resolution) []Candle {
	step := resolutionDuration(target)
	if step <= 0 || len(candles) == 0 {
		return candles
	}

	out := make([]Candle, 0, len(candles))

	var (
		cur       *Candle
		curBucket time.Time
	)

	for _, c := range candles {
		bucket := c.Time.Truncate(step)
		if cur == nil || bucket.After(curBucket) {
			if cur != nil {
				out = append(out, *cur)
			}

			curBucket = bucket

			b := c

			b.Time = bucket
			cur = &b

			continue
		}

		// Same bucket: fold this candle into the aggregate.
		if c.High > cur.High {
			cur.High = c.High
		}

		if c.Low < cur.Low {
			cur.Low = c.Low
		}

		cur.Close = c.Close

		cur.Volume += c.Volume
	}

	if cur != nil {
		out = append(out, *cur)
	}

	return out
}

// ResampleEvery aggregates every n candles into one, counting back from the most
// recent bar so the newest aggregate is always complete and the partial bucket
// (if any) falls at the start of the series rather than the end.
//
// It groups by bar count rather than by wall-clock time on purpose. A strategy
// that wants "the trend one timeframe up" should get the same aggregation
// whether the base bars are 1-minute live ticks or daily candles from a CSV, and
// whether or not the venue had gaps. Anchoring to the newest bar also means the
// higher-timeframe reading never flickers as a bucket fills up: the last
// aggregate is the last n completed bars, not a half-formed one.
func ResampleEvery(candles []Candle, n int) []Candle {
	if n <= 1 || len(candles) == 0 {
		return candles
	}

	groups := len(candles) / n
	if groups == 0 {
		return nil // not even one complete higher-timeframe bar yet
	}

	// Drop the oldest remainder so every emitted bar aggregates exactly n bars.
	start := len(candles) - groups*n
	out := make([]Candle, 0, groups)

	for g := range groups {
		window := candles[start+g*n : start+(g+1)*n]
		agg := window[0]

		for _, c := range window[1:] {
			if c.High > agg.High {
				agg.High = c.High
			}

			if c.Low < agg.Low {
				agg.Low = c.Low
			}

			agg.Close = c.Close

			agg.Volume += c.Volume
		}

		// The aggregate is stamped with its last bar's time: it only became
		// knowable when that bar closed.
		agg.Time = window[len(window)-1].Time

		out = append(out, agg)
	}

	return out
}
