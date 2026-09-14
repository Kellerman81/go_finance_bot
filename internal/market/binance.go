package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/errlog"
)

// Binance is a read-only market-data provider for crypto, using Binance's
// public REST endpoints. It needs no API key and no account.
//
// Scope, deliberately: this is a *data* feed only. The bot can track, signal on
// and backtest crypto through it, and paper-trade it against the built-in
// simulator — but it places no crypto orders. An exchange execution adapter
// carries withdrawal-capable keys and cannot be meaningfully tested without a
// funded account; shipping one that had never filled a real order would be
// worse than shipping none. Execution stays on the audited broker adapters.
//
// Crypto trades continuously, so the market-hours gate is a no-op for these
// symbols and warmup history is always available — which makes it a much easier
// feed to develop against than equities.
type Binance struct {
	base     string
	hc       *http.Client
	interval time.Duration

	mu      sync.RWMutex
	symbols map[string]struct{}
	last    map[string]Quote
	quotes  chan Quote

	// ctx/cancel bound in-flight requests so Close doesn't wait out a poll's
	// full HTTP timeout; done/stopped hand the quotes channel's lifecycle
	// entirely to poll — only the goroutine that sends on a channel may close
	// it, so Close signals and waits rather than closing quotes itself (a
	// concurrent close while pollOnce is mid-send panics).
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	done      chan struct{}
	stopped   chan struct{}
}

// binanceDefaultBase is the public REST host. api.binance.com is geo-blocked in
// some jurisdictions; the .us host and the data-only mirrors take the same
// paths, so BaseURL is configurable.
const binanceDefaultBase = "https://api.binance.com"

// binancePollInterval is how often the ticker endpoint is polled for live
// prices. Polling rather than streaming keeps the dependency surface to
// net/http; it is ample for a bot evaluating on 30-second cadence.
const binancePollInterval = 5 * time.Second

// binanceMaxKlines is the most candles one klines request may return.
const binanceMaxKlines = 1000

// NewBinance creates a Binance public-data provider and starts polling. base
// may be empty for the default host.
func NewBinance(base string) *Binance {
	return newBinance(base, binancePollInterval)
}

// newBinance is NewBinance with an injectable poll interval, so tests can
// force poll to reach the handler quickly without racing a post-construction
// mutation of the field the poll goroutine reads.
func newBinance(base string, interval time.Duration) *Binance {
	if strings.TrimSpace(base) == "" {
		base = binanceDefaultBase
	}

	ctx, cancel := context.WithCancel(context.Background())

	b := &Binance{
		base:     strings.TrimRight(base, "/"),
		hc:       &http.Client{Timeout: 15 * time.Second},
		interval: interval,
		symbols:  make(map[string]struct{}),
		last:     make(map[string]Quote),
		quotes:   make(chan Quote, 256),
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}

	go b.poll()

	return b
}

// Subscribe registers symbols for live polling. Symbols are Binance pairs
// ("BTCUSDT", "ETHEUR"), upper-cased.
func (b *Binance) Subscribe(symbols ...string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, s := range symbols {
		if s = normalizePair(s); s != "" {
			b.symbols[s] = struct{}{}
		}
	}

	return nil
}

// Unsubscribe stops polling the given symbols.
func (b *Binance) Unsubscribe(symbols ...string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, s := range symbols {
		delete(b.symbols, normalizePair(s))
	}

	return nil
}

// Quotes returns the live tick channel.
func (b *Binance) Quotes() <-chan Quote { return b.quotes }

// LastQuote returns the most recent tick seen for symbol.
func (b *Binance) LastQuote(symbol string) (Quote, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	q, ok := b.last[normalizePair(symbol)]

	return q, ok
}

// Close stops polling and closes the quote channel. It waits for poll to
// actually exit (aborting any in-flight request via ctx) before returning, so
// a caller that immediately discards the provider never races poll's own
// close of the quotes channel.
func (b *Binance) Close() error {
	b.closeOnce.Do(func() {
		close(b.done)
		b.cancel()
		<-b.stopped
	})

	return nil
}

// poll fetches prices for every subscribed symbol on a fixed cadence and
// publishes them as ticks. It owns the quotes channel: as the sole sender, it
// is the only goroutine allowed to close it, and only after its loop has
// truly exited — never concurrently with a send in pollOnce.
func (b *Binance) poll() {
	defer close(b.stopped)
	defer close(b.quotes)

	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()

	for {
		select {
		case <-b.done:
			return
		case <-ticker.C:
			b.pollOnce()
		}
	}
}

// pollOnce fetches the current price of every subscribed symbol in one request
// and emits a tick for each. One batched call keeps a 50-symbol watchlist
// within the public rate limits.
func (b *Binance) pollOnce() {
	b.mu.RLock()

	symbols := make([]string, 0, len(b.symbols))
	for s := range b.symbols {
		symbols = append(symbols, s)
	}

	b.mu.RUnlock()

	if len(symbols) == 0 {
		return
	}

	prices, err := b.tickerPrices(b.ctx, symbols)
	if err != nil {
		errlog.Recordf("binance", "ticker poll: %v", err)

		return
	}

	now := time.Now()

	for sym, px := range prices {
		if px <= 0 {
			continue
		}

		q := Quote{Symbol: sym, Price: px, Time: now}

		b.mu.Lock()

		b.last[sym] = q
		b.mu.Unlock()

		// Never block the poller on a slow consumer: a dropped tick is
		// recoverable, a stalled feed goroutine is not.
		select {
		case b.quotes <- q:
		default:
		}
	}
}

// tickerPrices fetches the latest price for the given symbols.
func (b *Binance) tickerPrices(
	ctx context.Context,
	symbols []string,
) (map[string]float64, error) {
	// The batched form takes a JSON array of symbols in the query string.
	list, err := json.Marshal(symbols)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("%s/api/v3/ticker/price?symbols=%s", b.base, string(list))

	var out []struct {
		Symbol string `json:"symbol"`
		Price  string `json:"price"`
	}

	if err := b.getJSON(ctx, url, &out); err != nil {
		return nil, err
	}

	prices := make(map[string]float64, len(out))

	for _, t := range out {
		px, err := strconv.ParseFloat(t.Price, 64)
		if err != nil {
			continue
		}

		prices[t.Symbol] = px
	}

	return prices, nil
}

// Candles fetches historical klines for indicator warmup and backtests.
func (b *Binance) Candles(
	ctx context.Context,
	symbol string,
	res Resolution,
	from, to time.Time,
) ([]Candle, error) {
	sym := normalizePair(symbol)
	if sym == "" {
		return nil, errors.New("binance: empty symbol")
	}

	url := fmt.Sprintf("%s/api/v3/klines?symbol=%s&interval=%s&limit=%d",
		b.base, sym, binanceInterval(res), binanceMaxKlines)

	if !from.IsZero() {
		url += fmt.Sprintf("&startTime=%d", from.UnixMilli())
	}

	if !to.IsZero() {
		url += fmt.Sprintf("&endTime=%d", to.UnixMilli())
	}

	// A kline is a heterogeneous array: [openTime, open, high, low, close,
	// volume, closeTime, ...]. Decoding into []any and asserting is the least
	// bad option for a positional JSON array.
	var raw [][]any

	if err := b.getJSON(ctx, url, &raw); err != nil {
		return nil, err
	}

	out := make([]Candle, 0, len(raw))

	for _, k := range raw {
		if len(k) < 6 {
			continue
		}

		openMs, ok := k[0].(float64)
		if !ok {
			continue
		}

		c := Candle{
			Symbol: sym,
			Time:   time.UnixMilli(int64(openMs)).UTC(),
			Open:   klineFloat(k[1]),
			High:   klineFloat(k[2]),
			Low:    klineFloat(k[3]),
			Close:  klineFloat(k[4]),
			Volume: klineFloat(k[5]),
		}
		if c.Close > 0 {
			out = append(out, c)
		}
	}

	return out, nil
}

// klineFloat parses one field of a kline array, which Binance encodes as a
// decimal string.
func klineFloat(v any) float64 {
	s, ok := v.(string)
	if !ok {
		return 0
	}

	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}

	return f
}

// binanceInterval maps a Resolution to Binance's interval string.
func binanceInterval(res Resolution) string {
	switch res {
	case Res5Min:
		return "5m"
	case Res15Min:
		return "15m"
	case Res1Hour:
		return "1h"
	case Res1Day:
		return "1d"
	case Res1Min:
		return "1m"
	}

	return "1m"
}

// normalizePair upper-cases a pair and strips the separators people habitually
// type ("BTC/USDT", "btc-usdt"), which Binance's API does not accept.
func normalizePair(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "/", "")
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "_", "")

	return s
}

// getJSON performs a GET and decodes the JSON response, turning a non-2xx into
// an error carrying a snippet of the body.
func (b *Binance) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := b.hc.Do(req)
	if err != nil {
		return err
	}

	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}

		return fmt.Errorf("binance: http %d: %s", resp.StatusCode, snippet)
	}

	return json.Unmarshal(body, out)
}
