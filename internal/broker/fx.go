package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
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

// fxConverter fetches and caches currency-conversion rates, shared by broker
// adapters that can report prices/balances in a currency other than the
// user's configured preferred_currency (e.g. a US-listed stock held through
// a EUR-preferring account).
type fxConverter struct {
	http    *http.Client
	baseURL string

	mu    sync.Mutex
	cache map[string]fxQuote
}

// newFXConverter builds a converter using client for outbound requests.
func newFXConverter(client *http.Client) *fxConverter {
	return &fxConverter{
		http:    client,
		baseURL: defaultFXBaseURL,
		cache:   make(map[string]fxQuote),
	}
}

// Rate returns the multiplier to convert 1 unit of currency `from` into `to`
// (1 when they're equal or either is unknown/empty — the common case for
// brokers/instruments that don't report a currency, or a caller that hasn't
// configured preferred_currency).
func (c *fxConverter) Rate(ctx context.Context, from, to string) (float64, error) {
	if from == "" || to == "" || from == to {
		return 1, nil
	}

	key := from + to

	c.mu.Lock()
	q, ok := c.cache[key]
	c.mu.Unlock()

	if ok && time.Since(q.at) < fxTTL {
		return q.rate, nil
	}

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

	c.mu.Lock()
	c.cache[key] = fxQuote{rate: rate, at: time.Now()}
	c.mu.Unlock()

	return rate, nil
}
