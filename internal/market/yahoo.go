package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	// cooldownNoted is set once the current cooldown has been recorded — see
	// noteCooldownOnce.
	cooldownNoted bool

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
//
//nolint:cyclop // one branch per fallback interval outcome
func (y *YahooHistory) Candles(
	ctx context.Context,
	symbol string,
	res Resolution,
	from, to time.Time,
) ([]Candle, error) {
	interval, defRange := yahooParams(res)

	attempts := [][2]string{{interval, rangeFor(interval, defRange, from)}}

	// Only STRICTLY coarser intervals. The list used to be added whenever it
	// differed by name, so a daily request with few bars fell back to HOURLY
	// ones — finer, not coarser: young ETFs passed min_history_bars and were
	// judged on hourly bars against daily thresholds while the verdict said
	// "D" (MK2-2) — and "60m" never matched "1h", so an hourly request fetched
	// the same data twice (MK2-3).
	for _, fb := range [][2]string{{"1h", "3mo"}, {"1d", "2y"}} {
		if yahooIntervalMinutes(fb[0]) > yahooIntervalMinutes(interval) {
			attempts = append(attempts, [2]string{fb[0], rangeFor(fb[0], fb[1], from)})
		}
	}

	// Enough bars to cover the strategy warmup; below this, try a coarser
	// interval (illiquid instruments return only a few intraday bars).
	const enough = 60

	var (
		best    []Candle
		lastErr error
		// bestInterval is the interval best came from.
		bestInterval string
	)

	for i, a := range attempts {
		candles, err := y.fetch(ctx, symbol, a[0], a[1])
		if err != nil {
			lastErr = err

			if errors.Is(err, errYahooThrottled) {
				break // the coarser attempts hit the same host and would fail the same way
			}

			continue
		}

		// The REQUESTED interval is windowed to what was asked for: Yahoo
		// serves whole ranges, and a caller's lookback_days was otherwise
		// silently whatever the range happened to cover (MK2-6). A coarser
		// fallback is not: it is a substitution that exists to supply
		// history the fine interval lacks (and is recorded as one below),
		// and cutting it to warmup's 9-day window left an illiquid symbol
		// ~60 hourly bars where it used to get three months — found by the
		// verify-change pass.
		//
		// Whether the interval has ENOUGH bars is judged on what Yahoo
		// serves at it, not on the window: the coarser fallback exists for
		// illiquid instruments, not for a short window. Judging the window
		// sent venuePrice's 2-day 1m request to hourly bars every Monday
		// morning (the weekend is empty), and venuePrice then refused every
		// entry until 60 one-minute bars of the new session existed — found
		// by the verify-change pass.
		served := len(candles)

		if i == 0 {
			candles = sliceByTime(candles, from, to)

			if served >= enough {
				return candles, nil
			}
		}

		if len(candles) > len(best) {
			best, bestInterval = candles, a[0]
		}

		if len(best) >= enough {
			if bestInterval != interval {
				// A caller asked for interval (the first attempt) and silently
				// got a[0] instead. venuePrice treats the last candle as "the
				// venue's current price" for order sizing and the deviation
				// gate — a stale daily close mistaken for a live price is a
				// real mispricing risk, not just a display quirk. This
				// doesn't change what's returned (the coarser data is still
				// the best available and warmup genuinely wants it), but it
				// makes the substitution visible instead of silent. See
				// CODE_REVIEW_FINDINGS.md's Yahoo-granularity finding.
				noteSubstitution(symbol, interval, bestInterval)
			}

			return best, nil
		}
	}

	if len(best) > 0 {
		// No interval reached `enough`, and the most bars may still have come
		// from a fallback: that substitution was served with no note — 40
		// hourly bars for a 1m request, silently (MK3-4).
		if bestInterval != interval {
			noteSubstitution(symbol, interval, bestInterval)
		}

		return best, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}

	return nil, fmt.Errorf("yahoo: no data for %s at any interval", symbol)
}

// noteSubstitution records, once while it stands, that symbol's requested
// interval was served by a coarser one. It is a property of the instrument,
// not an event, and venuePrice asks on every evaluation: recorded per call —
// with the varying bar count in the text, so no two lines matched — one
// illiquid symbol filled Yahoo's 100-entry share in 50 minutes and evicted
// the real 429 an operator needed (EL3-1, the errlog side of VP-1).
func noteSubstitution(symbol, requested, served string) {
	errlog.RecordOncef("yahoo",
		"%s: requested %s bars, serving %s (too few bars at the finer interval)",
		symbol, requested, served)
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
	y.cooldownNoted = false
	y.mu.Unlock()
}

// errYahooThrottled is returned for a request refused locally while a
// cooldown is in force — no request was sent.
var errYahooThrottled = errors.New("yahoo: cooling down after repeated throttling")

// noteCooldownOnce records the cooldown in errlog once per cooldown window.
//
// Each refused request used to record its own entry — three per Candles call
// (1m, 1h, 1d attempts) for 30 s — so about 34 calls filled yahoo's 100-entry
// share of the ring and evicted the real 429 or 5xx that started the
// cooldown: the diagnostics tab showed 100 identical "cooling down" lines and
// no cause (EL2-1).
func (y *YahooHistory) noteCooldownOnce() {
	y.mu.Lock()

	first := !y.cooldownNoted
	until := y.cooldownUntil

	y.cooldownNoted = true

	y.mu.Unlock()

	if first {
		errlog.Recordf("yahoo", "cooling down after repeated throttling: requests refused until %s",
			until.Format(time.TimeOnly))
	}
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
//
//nolint:cyclop // retry loop with cooldown, backoff and host rotation
func (y *YahooHistory) fetch(
	ctx context.Context,
	symbol, interval, rng string,
) (_ []Candle, err error) {
	defer func() {
		if err != nil && !errors.Is(err, errYahooThrottled) {
			errlog.Recordf("yahoo", "%s (%s): %s", symbol, interval, err.Error())
		}
	}()

	if y.throttled() {
		y.noteCooldownOnce()

		return nil, fmt.Errorf("%w; not retrying %s yet", errYahooThrottled, symbol)
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

	// Bounded by the SHORTEST of the arrays, not by Close alone: Yahoo emits
	// ragged arrays around corporate actions and partial days, and indexing
	// Open/High/Low past their end panicked — on the engine goroutine that
	// is caught by runSupervised, which disarms trading (MK2-7).
	rows := min(len(r.Timestamp), len(q.Close), len(q.Open), len(q.High), len(q.Low))

	for i, ts := range r.Timestamp[:rows] {
		// Skip gaps where Yahoo returns null OHLC (market closed minutes).
		if q.Close[i] == nil || q.Open[i] == nil || q.High[i] == nil || q.Low[i] == nil {
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
// yahooIntervalMinutes is an interval's length, for ordering intervals.
func yahooIntervalMinutes(interval string) int {
	switch interval {
	case "1m":
		return 1
	case "5m":
		return 5
	case "15m":
		return 15
	case "60m", "1h":
		return 60
	case "1d":
		return 1440
	}

	return 0
}

// yahooRanges lists, per interval, the range values Yahoo accepts for it
// (shortest first) with the days each covers. The last entry is the
// interval's ceiling: Yahoo refuses intraday ranges past ~60 days (1m: ~7).
var yahooRanges = map[string][]struct {
	rng  string
	days int
}{
	"1m":  {{"1d", 1}, {"5d", 5}},
	"5m":  {{"1d", 1}, {"5d", 5}, {"1mo", 31}},
	"15m": {{"1d", 1}, {"5d", 5}, {"1mo", 31}},
	"60m": {{"1mo", 31}, {"3mo", 92}, {"6mo", 183}, {"1y", 366}, {"2y", 731}},
	"1h":  {{"1mo", 31}, {"3mo", 92}, {"6mo", 183}, {"1y", 366}, {"2y", 731}},
	"1d": {
		{"1mo", 31},
		{"3mo", 92},
		{"6mo", 183},
		{"1y", 366},
		{"2y", 731},
		{"5y", 1827},
		{"10y", 3653},
		{"max", 1 << 30},
	},
}

// rangeFor widens interval's default range until it covers from — never
// narrows the range string; the screener's lookback_days used to be silently
// whatever the fixed range covered (90 days at "15" gave 30) (MK2-6). A
// lookback longer than Yahoo serves at that interval is capped at the
// ceiling. Not recorded in errlog: engine warmup asks for 9 days at 1m on
// every start, which would be a standing diagnostics entry about a constant
// the operator cannot change; the ceilings are documented instead (README,
// config.example.yaml, the lookback_days help).
func rangeFor(interval, def string, from time.Time) string {
	if from.IsZero() {
		return def
	}

	need := int(math.Ceil(time.Since(from).Hours() / 24))
	opts := yahooRanges[interval]

	have := 0

	for _, o := range opts {
		if o.rng == def {
			have = o.days
		}
	}

	if need <= have || len(opts) == 0 {
		return def
	}

	for _, o := range opts {
		if o.days >= need {
			return o.rng
		}
	}

	return opts[len(opts)-1].rng
}

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
