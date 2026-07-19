package market

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/errlog"
)

// yahooHosts are the interchangeable Yahoo chart hosts; query2 is tried when
// query1 fails (Yahoo load-balances between them and one can be throttled).
var yahooHosts = []string{"query1.finance.yahoo.com", "query2.finance.yahoo.com"}

// YahooHistory fetches historical candles from Yahoo Finance's public chart
// endpoint. It needs no API key and works for any broker, so it's a reliable
// default warmup source where the live feed (e.g. Finnhub free tier) has no
// historical candles. It satisfies the engine's history-source interface.
//
// Yahoo throttles aggressively, so requests are paced by a shared minimum
// interval and retried with backoff on 429/5xx.
type YahooHistory struct {
	http        *http.Client
	minInterval time.Duration // minimum gap between outbound requests
	maxRetries  int

	mu     sync.Mutex
	nextAt time.Time // earliest time the next request may go out
}

// NewYahooHistory constructs a Yahoo Finance history provider.
func NewYahooHistory() *YahooHistory {
	return &YahooHistory{
		http:        &http.Client{Timeout: 15 * time.Second},
		minInterval: 250 * time.Millisecond,
		maxRetries:  3,
	}
}

// wait blocks until the shared rate limiter allows the next request or ctx is
// done, then reserves the following slot.
func (y *YahooHistory) wait(ctx context.Context) error {
	y.mu.Lock()

	now := time.Now()
	start := now

	if y.nextAt.After(now) {
		start = y.nextAt
	}

	y.nextAt = start.Add(y.minInterval)
	y.mu.Unlock()

	d := time.Until(start)
	if d <= 0 {
		return nil
	}

	t := time.NewTimer(d)

	defer t.Stop()

	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Candles returns OHLCV bars for the symbol at the requested resolution. If the
// requested (typically 1-minute) interval has no data — common for illiquid
// instruments with little intraday activity — it falls back to progressively
// coarser intervals (hourly, then daily) so warmup still gets bars.
func (y *YahooHistory) Candles(
	ctx context.Context,
	symbol string,
	res Resolution,
	_, _ time.Time, // Yahoo serves fixed ranges per interval; the window is implied
) ([]Candle, error) {
	interval, rng := yahooParams(res)
	attempts := [][2]string{{interval, rng}}

	for _, fb := range [][2]string{{"1h", "3mo"}, {"1d", "2y"}} {
		if fb[0] != interval {
			attempts = append(attempts, fb)
		}
	}

	// Enough bars to cover the strategy warmup; below this, try a coarser
	// interval (illiquid instruments return only a few intraday bars).
	const enough = 60

	var (
		best    []Candle
		lastErr error
	)

	for _, a := range attempts {
		candles, err := y.fetch(ctx, symbol, a[0], a[1])
		if err != nil {
			lastErr = err
			continue
		}

		if len(candles) > len(best) {
			best = candles
		}

		if len(best) >= enough {
			return best, nil
		}
	}

	if len(best) > 0 {
		return best, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}

	return nil, fmt.Errorf("yahoo: no data for %s at any interval", symbol)
}

// fetch retrieves one symbol/interval/range from Yahoo's chart endpoint,
// pacing via the shared limiter and retrying on 429/5xx with exponential
// backoff. It tries query1 then query2 on each attempt.
func (y *YahooHistory) fetch(
	ctx context.Context,
	symbol, interval, rng string,
) (_ []Candle, err error) {
	defer func() {
		if err != nil {
			errlog.Recordf("yahoo", "%s (%s): %s", symbol, interval, err.Error())
		}
	}()

	backoff := 400 * time.Millisecond

	var lastErr error

	for attempt := 0; attempt <= y.maxRetries; attempt++ {
		if attempt > 0 {
			// Jittered backoff between retries; abort early if ctx is done.
			jitter := rand.Int63n(int64(backoff)) //nolint:gosec // retry jitter only
			t := time.NewTimer(backoff + time.Duration(jitter))

			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()

				return nil, ctx.Err()
			}

			backoff *= 2
		}

		host := yahooHosts[attempt%len(yahooHosts)]

		candles, retryable, ferr := y.fetchOnce(ctx, host, symbol, interval, rng)
		if ferr == nil {
			return candles, nil
		}

		lastErr = ferr
		if !retryable {
			return nil, ferr
		}
	}

	return nil, lastErr
}

// yahooQuote holds the parallel OHLCV arrays of one chart result (null-padded
// on market-closed minutes).
type yahooQuote struct {
	Open   []*float64 `json:"open"`
	High   []*float64 `json:"high"`
	Low    []*float64 `json:"low"`
	Close  []*float64 `json:"close"`
	Volume []*float64 `json:"volume"`
}

// yahooIndicators wraps the quote arrays of one chart result.
type yahooIndicators struct {
	Quote []yahooQuote `json:"quote"`
}

// yahooResult is one result entry of a chart response.
type yahooResult struct {
	Timestamp  []int64         `json:"timestamp"`
	Indicators yahooIndicators `json:"indicators"`
}

// yahooChart is the payload of Yahoo's chart API response.
type yahooChart struct {
	Result []yahooResult `json:"result"`
	Error  any           `json:"error"`
}

// yahooChartBody is the envelope of Yahoo's chart API response.
type yahooChartBody struct {
	Chart yahooChart `json:"chart"`
}

// fetchOnce performs a single request against one host. retryable reports
// whether the error is worth another attempt (throttling / transient 5xx / a
// network error), as opposed to a permanent failure.
//
//nolint:cyclop // linear fetch → validate → decode → convert flow
func (y *YahooHistory) fetchOnce(
	ctx context.Context,
	host, symbol, interval, rng string,
) (_ []Candle, retryable bool, err error) {
	if err := y.wait(ctx); err != nil {
		return nil, false, err
	}

	url := fmt.Sprintf("https://%s/v8/finance/chart/%s?interval=%s&range=%s",
		host, symbol, interval, rng)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}

	// Yahoo rejects requests without a browser-like User-Agent.
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; go_finance_bot/1.0)")

	resp, err := y.http.Do(req)
	if err != nil {
		return nil, true, err // network error — retry
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		retry := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return nil, retry, fmt.Errorf("yahoo %s: status %d", symbol, resp.StatusCode)
	}

	var body yahooChartBody

	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, false, fmt.Errorf("yahoo decode %s: %w", symbol, err)
	}

	if len(body.Chart.Result) == 0 || len(body.Chart.Result[0].Indicators.Quote) == 0 {
		return nil, false, nil // no data at this interval — let the caller try a coarser one
	}

	r := body.Chart.Result[0]
	q := r.Indicators.Quote[0]
	out := make([]Candle, 0, len(r.Timestamp))

	for i, ts := range r.Timestamp {
		// Skip gaps where Yahoo returns null OHLC (market closed minutes).
		if i >= len(q.Close) || q.Close[i] == nil || q.Open[i] == nil || q.High[i] == nil ||
			q.Low[i] == nil {
			continue
		}

		var vol float64

		if i < len(q.Volume) && q.Volume[i] != nil {
			vol = *q.Volume[i]
		}

		out = append(out, Candle{
			Symbol: symbol,
			Open:   *q.Open[i],
			High:   *q.High[i],
			Low:    *q.Low[i],
			Close:  *q.Close[i],
			Volume: vol,
			Time:   time.Unix(ts, 0),
		})
	}

	return out, false, nil // may be empty; caller falls back to a coarser interval
}

// yahooParams maps a resolution to Yahoo's interval + a range that comfortably
// covers indicator warmup.
func yahooParams(res Resolution) (interval, rng string) {
	switch res {
	case Res5Min:
		return "5m", "1mo"
	case Res15Min:
		return "15m", "1mo"
	case Res1Hour:
		return "60m", "3mo"
	case Res1Day:
		return "1d", "1y"
	case Res1Min: // the shared fallback below
	}

	return "1m", "5d"
}
