package market

import (
	"context"
	"fmt"
	"time"
)

// FallbackHistory tries each wrapped HistoryFetcher in order and returns the
// first non-empty result, so a primary source (e.g. the broker's own chart
// endpoint) can be backed by a general one (e.g. Yahoo). It centralises the
// fallback the engine previously hand-rolled in warmup.
type FallbackHistory struct {
	sources []HistoryFetcher
}

// NewFallbackHistory composes sources into a fallback chain, tried left to right.
func NewFallbackHistory(sources ...HistoryFetcher) *FallbackHistory {
	return &FallbackHistory{sources: sources}
}

// Candles returns the first source's non-empty candles. It keeps trying on error
// or empty results, returning the last error only if every source failed to
// produce data.
func (f *FallbackHistory) Candles(
	ctx context.Context,
	symbol string,
	res Resolution,
	from, to time.Time,
) ([]Candle, error) {
	var lastErr error

	for _, s := range f.sources {
		if s == nil {
			continue
		}

		candles, err := s.Candles(ctx, symbol, res, from, to)
		if err != nil {
			lastErr = err
			continue
		}

		if len(candles) > 0 {
			return candles, nil
		}
	}

	if lastErr != nil {
		return nil, lastErr
	}

	return nil, fmt.Errorf("no history for %s at %s from any source", symbol, res)
}
