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
