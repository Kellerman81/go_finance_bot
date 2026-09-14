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

// yahooUserAgent is required: Yahoo rejects requests without a browser-like one.
const yahooUserAgent = "Mozilla/5.0 (compatible; go_finance_bot/1.0)"

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

	// cooldownUntil, while in the future, makes fetch fail immediately with
	// no network calls at all — see coolDown.
	cooldownUntil time.Time

	// base overrides the upstream host. Empty in production, where the two
	// interchangeable Yahoo hosts are used; a test points it at a local server.
	base string
}

// yahooCooldown is how long fetch backs off entirely after exhausting every
// retry against a retryable (429/5xx/network) error. One symbol's Candles
// call can already generate up to (fallback intervals) × (1 + maxRetries)
// requests trying to get *some* data; without this, every other symbol's
// Candles call during the same throttling episode — a screener rescan can be
// dozens in a row — repeats that same full request storm, which only makes
// the throttling worse right when the upstream is already struggling. See
// CODE_REVIEW_FINDINGS.md's Yahoo retry-storm finding.
const yahooCooldown = 30 * time.Second

// endpoints returns the base URLs to try, in order.
func (y *YahooHistory) endpoints() []string {
	if y.base != "" {
		return []string{y.base}
	}

	out := make([]string, 0, len(yahooHosts))
	for _, h := range yahooHosts {
		out = append(out, "https://"+h)
	}

	return out
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

	for i, a := range attempts {
		candles, err := y.fetch(ctx, symbol, a[0], a[1])
		if err != nil {
			lastErr = err
			continue
		}

		if len(candles) > len(best) {
			best = candles
		}

		if len(best) >= enough {
			if i > 0 {
				// A caller asked for interval (the first attempt) and silently
				// got a[0] instead. venuePrice treats the last candle as "the
				// venue's current price" for order sizing and the deviation
				// gate — a stale daily close mistaken for a live price is a
				// real mispricing risk, not just a display quirk. This
				// doesn't change what's returned (the coarser data is still
				// the best available and warmup genuinely wants it), but it
				// makes the substitution visible instead of silent. See
				// CODE_REVIEW_FINDINGS.md's Yahoo-granularity finding.
				errlog.Recordf("yahoo", "%s: requested %s bars, serving %s (only %d bars available at the finer interval)",
					symbol, interval, a[0], len(best))
			}

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

// throttled reports whether fetch is in a post-exhaustion cooldown (see
// yahooCooldown) and should fail immediately without making any request.
func (y *YahooHistory) throttled() bool {
	y.mu.Lock()
	defer y.mu.Unlock()

	return time.Now().Before(y.cooldownUntil)
}

// coolDown starts (or extends) the cooldown window after a fetch exhausts
// every retry against a retryable error.
func (y *YahooHistory) coolDown() {
	y.mu.Lock()
	y.cooldownUntil = time.Now().Add(yahooCooldown)
	y.mu.Unlock()
}

// clearCooldown ends an active cooldown once a request actually succeeds —
// the upstream is reachable again, so there is no reason to keep refusing
// requests for the rest of the window.
func (y *YahooHistory) clearCooldown() {
	y.mu.Lock()
	y.cooldownUntil = time.Time{}
	y.mu.Unlock()
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

	if y.throttled() {
		return nil, fmt.Errorf(
			"yahoo: cooling down after repeated throttling; not retrying %s yet", symbol)
	}

	backoff := 400 * time.Millisecond

	var (
		lastErr       error
		lastRetryable bool
	)

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

		// y.base (set by tests) previously only applied to the search path
		// (endpoints() below) — the chart path hardcoded the public Yahoo
		// hosts regardless, which is why Candles had no test coverage at
		// all. Routing through the same endpoints() a mirror/proxy override
		// would also apply here. See CODE_REVIEW_FINDINGS.md's
		// YahooHistory.base finding.
		endpoints := y.endpoints()
		base := endpoints[attempt%len(endpoints)]

		candles, retryable, ferr := y.fetchOnce(ctx, base, symbol, interval, rng)
		if ferr == nil {
			y.clearCooldown() // upstream is reachable; end any earlier cooldown
			return candles, nil
		}

		lastErr = ferr
		lastRetryable = retryable

		if !retryable {
			return nil, ferr
		}
	}

	// Every retry against every host was exhausted and the last error was
	// still retryable (429/5xx/network) — a strong signal the throttling
	// isn't a one-off blip. Cool down so the next Candles call, for this or
	// any other symbol, doesn't immediately repeat the same request storm.
	if lastRetryable {
		y.coolDown()
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

// fetchOnce performs a single request against one base URL (scheme +
// host — see endpoints()). retryable reports whether the error is worth
// another attempt (throttling / transient 5xx / a network error), as
// opposed to a permanent failure.
//
//nolint:cyclop // linear fetch → validate → decode → convert flow
func (y *YahooHistory) fetchOnce(
	ctx context.Context,
	base, symbol, interval, rng string,
) (_ []Candle, retryable bool, err error) {
	if err := y.wait(ctx); err != nil {
		return nil, false, err
	}

	url := fmt.Sprintf("%s/v8/finance/chart/%s?interval=%s&range=%s",
		base, symbol, interval, rng)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}

	req.Header.Set("User-Agent", yahooUserAgent)

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
