package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/errlog"
)

// fxQuote is a cached currency-conversion rate.
type fxQuote struct {
	rate float64
	at   time.Time
}

// fxTTL bounds how long a fetched FX rate is trusted before being refetched.
// Reference rates move slowly enough that this only needs to be accurate for
// display and risk-limit purposes, not tick-precise.
const fxTTL = time.Hour

// defaultFXBaseURL is the free, no-key, ECB-backed rate API used to convert a
// broker-reported price into the account's configured preferred_currency.
const defaultFXBaseURL = "https://api.frankfurter.dev"

// fxFailBackoff is how long a failed rate lookup is remembered before the
// pair is fetched again, and fxTimeout bounds one lookup. Failures used to be
// retried on every call, under the broker's own 15–20 s client timeout: with
// the rate API hanging, each foreign position of every portfolio sync waited
// a full timeout — on the goroutine that runs checkExits, so stops fired
// minutes late (BR3-2).
const (
	fxFailBackoff = 2 * time.Minute
	fxTimeout     = 5 * time.Second
)

// errFXBackoff marks a rate failure answered from the backoff memory rather
// than a fresh lookup — already reported, see recordFXFailure.
var errFXBackoff = errors.New("rate lookup backing off after a failure")

// recordFXFailure puts a rate failure in the diagnostics list once per actual
// failure, with what it means for the caller. Each foreign position of every
// sync used to record its own entry — under the broker's source, so an FX
// outage evicted that broker's order rejections within minutes — with the
// "fx rate X->Y:" prefix twice, and "(using 1:1)" even where the caller refuses
// the order instead (EL3-2, EL3-5).
func recordFXFailure(source string, err error, consequence string) {
	if errors.Is(err, errFXBackoff) {
		return
	}

	errlog.Recordf(source, "%s — %s", err, consequence)
}

// minorUnits maps a minor-unit currency code (case-sensitive: "GBp" is pence,
// "GBP" pounds) to its major currency and the factor between them. Trading
// 212 reports London lines in GBX and the ECB-backed rate API knows no GBX,
// so every lookup failed and the 1:1 stopgap valued pence as euros — 1000
// shares at 72p counted as 72000, inflating equity and every pct limit, and
// tripping the drawdown halt on the sale (BR3-1).
var minorUnits = map[string]struct {
	major string
	scale float64
}{
	"GBX": {"GBP", 0.01},
	"GBp": {"GBP", 0.01},
	"ZAc": {"ZAR", 0.01},
	"ZAX": {"ZAR", 0.01},
	"ILA": {"ILS", 0.01},
}

// majorUnit resolves a minor-unit code to its major currency and the factor
// that converts one minor unit into it; any other code maps to itself at 1.
func majorUnit(ccy string) (string, float64) {
	if m, ok := minorUnits[ccy]; ok {
		return m.major, m.scale
	}

	return ccy, 1
}

// fxConverter fetches and caches currency-conversion rates, shared by broker
// adapters that can report prices/balances in a currency other than the
// user's configured preferred_currency (e.g. a US-listed stock held through
// a EUR-preferring account).
type fxConverter struct {
	http    *http.Client
	baseURL string

	mu     sync.Mutex
	cache  map[string]fxQuote
	failed map[string]fxFailure // see fxFailBackoff
}

// fxFailure is a remembered failed lookup.
type fxFailure struct {
	err error
	at  time.Time
}

// newFXConverter builds a converter using client for outbound requests.
//
// It gets its own short-timeout client on client's transport, so a hanging
// rate API costs fxTimeout, not the broker's order timeout (BR3-2).
func newFXConverter(client *http.Client) *fxConverter {
	fx := &http.Client{Timeout: fxTimeout}
	if client != nil {
		fx.Transport = client.Transport
		fx.CheckRedirect = client.CheckRedirect
	}

	return &fxConverter{
		http:    fx,
		baseURL: defaultFXBaseURL,
		cache:   make(map[string]fxQuote),
		failed:  make(map[string]fxFailure),
	}
}

// Rate returns the multiplier to convert 1 unit of currency `from` into `to`
// (1 when they're equal or either is unknown/empty — the common case for
// brokers/instruments that don't report a currency, or a caller that hasn't
// configured preferred_currency).
//
// Minor units (GBX, ZAc, ILA) are converted through their major currency.
func (c *fxConverter) Rate(ctx context.Context, from, to string) (float64, error) {
	if from == "" || to == "" || from == to {
		return 1, nil
	}

	fromMajor, fromScale := majorUnit(from)
	toMajor, toScale := majorUnit(to)

	rate, err := c.majorRate(ctx, fromMajor, toMajor)
	if err != nil {
		return 0, err
	}

	return rate * fromScale / toScale, nil
}

// majorRate is Rate for two major currencies: cached on success, and a
// failure is remembered for fxFailBackoff rather than re-fetched per call.
func (c *fxConverter) majorRate(ctx context.Context, from, to string) (float64, error) {
	if from == to {
		return 1, nil
	}

	key := from + to

	c.mu.Lock()
	q, ok := c.cache[key]
	f, failed := c.failed[key]
	c.mu.Unlock()

	if ok && time.Since(q.at) < fxTTL {
		return q.rate, nil
	}

	if failed && time.Since(f.at) < fxFailBackoff {
		return 0, fmt.Errorf("%w: %w", errFXBackoff, f.err)
	}

	rate, err := c.fetch(ctx, from, to)

	c.mu.Lock()
	defer c.mu.Unlock()

	if err != nil {
		if c.failed == nil {
			c.failed = make(map[string]fxFailure)
		}

		c.failed[key] = fxFailure{err: err, at: time.Now()}

		return 0, err
	}

	delete(c.failed, key)
	c.cache[key] = fxQuote{rate: rate, at: time.Now()}

	return rate, nil
}

// fetch asks the rate API for one pair.
func (c *fxConverter) fetch(ctx context.Context, from, to string) (float64, error) {
	endpoint := fmt.Sprintf(
		"%s/v1/latest?base=%s&symbols=%s",
		c.baseURL, url.QueryEscape(from), url.QueryEscape(to),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, fmt.Errorf("fx rate %s->%s: %w", from, to, err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("fx rate %s->%s: %w", from, to, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("fx rate %s->%s: status %d", from, to, resp.StatusCode)
	}

	var out struct {
		Rates map[string]float64 `json:"rates"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("fx rate %s->%s: decode: %w", from, to, err)
	}

	rate, ok := out.Rates[to]
	if !ok || rate <= 0 {
		return 0, fmt.Errorf("fx rate %s->%s: no rate in response", from, to)
	}

	return rate, nil
}
