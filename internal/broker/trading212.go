package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/errlog"
)

// Trading212 is a Broker backed by the Trading 212 Public API. API keys are
// generated self-service in the Trading 212 app (no app-approval gate). Point
// BaseURL at the demo endpoint for paper trading first.
//
// The API is account/order focused (no market-data stream) and strictly rate
// limited per endpoint, so this adapter paces requests accordingly. Trading 212
// uses its own ticker scheme (e.g. AAPL_US_EQ); plain symbols are resolved to
// it via the instruments metadata (cached). Fractional shares are supported.
//
// NOTE: built to the documented API but not exercised against a live account
// here — validate on the demo endpoint before relying on it.
type Trading212 struct {
	baseURL string
	apiKey  string
	secret  string
	http    *http.Client

	isins     map[string]string // input symbol -> ISIN override (from config)
	preferCcy string            // preferred listing currency (e.g. EUR)

	mu       sync.Mutex
	byShort  map[string][]t212Inst // shortName -> listings (multiple exchanges)
	byISIN   map[string][]t212Inst // ISIN -> listings (multiple currencies)
	resolved map[string]string     // input symbol -> Trading 212 ticker (cache)
	reverse  map[string]string     // Trading 212 ticker -> input symbol
	qtyPrec  map[string]int        // ticker -> accepted quantity decimal places (learned)
	loaded   bool
	currency string

	rlMu     sync.Mutex
	lastCall map[string]time.Time
}

// t212Limits are conservative per-endpoint minimum intervals (the Public API
// throttles aggressively; metadata especially).
var t212Limits = map[string]time.Duration{
	"cash":         2 * time.Second,
	"portfolio":    5 * time.Second,
	"orders":       2 * time.Second,
	"order_status": 2 * time.Second, // fill polling (GET /equity/orders/{id})
	"info":         30 * time.Second,
	"instruments":  50 * time.Second,
	"default":      2 * time.Second,
}

// NewTrading212 constructs a Trading 212 broker client. apiKey and secret are
// the two credentials from the app, sent as HTTP Basic auth. isins maps
// watchlist symbols to ISINs for instruments not resolvable by ticker, and
// preferCcy is the listing currency to prefer when several exist (default EUR).
func NewTrading212(
	baseURL, apiKey, secret string,
	isins map[string]string,
	preferCcy string,
) *Trading212 {
	if preferCcy == "" {
		preferCcy = "EUR"
	}

	if isins == nil {
		isins = make(map[string]string)
	}

	return &Trading212{
		baseURL:   strings.TrimRight(baseURL, "/"),
		apiKey:    apiKey,
		secret:    secret,
		isins:     isins,
		preferCcy: strings.ToUpper(preferCcy),
		http:      &http.Client{Timeout: 20 * time.Second},
		resolved:  make(map[string]string),
		reverse:   make(map[string]string),
		qtyPrec:   make(map[string]int),
		lastCall:  make(map[string]time.Time),
	}
}

// t212Inst is a Trading 212 instrument listing.
type t212Inst struct {
	Ticker    string
	ShortName string
	ISIN      string
	Currency  string
}

// Name identifies the broker adapter.
func (*Trading212) Name() string { return "trading212" }

// pace blocks until the per-endpoint minimum interval has elapsed, honouring
// context cancellation, so bursts cannot trip the API rate limiter.
func (t *Trading212) pace(ctx context.Context, group string) error {
	t.rlMu.Lock()
	defer t.rlMu.Unlock()

	limit, ok := t212Limits[group]
	if !ok {
		limit = t212Limits["default"]
	}

	if wait := limit - time.Since(t.lastCall[group]); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}

	t.lastCall[group] = time.Now()

	return nil
}

// do performs an authenticated Trading 212 call after pacing its endpoint
// group, recording failures in the error log and decoding into out.
//
//nolint:cyclop // linear request/response handling; splitting hurts readability
func (t *Trading212) do(
	ctx context.Context,
	method, path, group string,
	body, out any,
) (err error) {
	defer func() {
		if err != nil && !errors.Is(err, context.Canceled) {
			errlog.Record("trading212", err.Error())
		}
	}()

	if err := t.pace(ctx, group); err != nil {
		return err
	}

	var reader io.Reader

	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}

		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, t.baseURL+path, reader)
	if err != nil {
		return err
	}

	req.SetBasicAuth(t.apiKey, t.secret) // Authorization: Basic base64(key:secret)
	req.Header.Set("Accept", "application/json")

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := t.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf(
			"trading212 %s %s: 401 unauthorized (check api_key/secret and that base_url matches the key's environment)",
			method,
			path,
		)

	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("trading212 %s %s: 429 rate limited", method, path)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return fmt.Errorf(
			"trading212 %s %s: status %d: %s",
			method,
			path,
			resp.StatusCode,
			string(data),
		)
	}

	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("trading212 decode %s: %w", path, err)
		}
	}

	return nil
}

// ResolveSymbol maps an input symbol to a tradeable Trading 212 ticker (for
// order placement and orderability checks). It accepts:
//   - a Trading 212 ticker directly (contains "_"), e.g. AAPL_US_EQ
//   - a plain symbol, e.g. AAPL (the US listing is preferred)
//   - an exchange-suffixed symbol, e.g. LXS.DE -> the German listing LXSd_EQ
//   - an ISIN, e.g. DE0005470405 or DE000SLA7M77.SG
func (t *Trading212) ResolveSymbol(ctx context.Context, symbol string) (string, error) {
	return t.resolveTicker(ctx, symbol)
}

// resolveTicker implements ResolveSymbol with caching; it lazily loads the
// instrument metadata on first use.
func (t *Trading212) resolveTicker(ctx context.Context, symbol string) (string, error) {
	if strings.Contains(symbol, "_") {
		t.remember(symbol, symbol)

		return symbol, nil
	}

	t.mu.Lock()

	if tk, ok := t.resolved[symbol]; ok {
		t.mu.Unlock()

		return tk, nil
	}

	loaded := t.loaded
	t.mu.Unlock()

	if !loaded {
		if err := t.loadInstruments(ctx); err != nil {
			return "", err
		}
	}

	base, suffix := splitSymbol(symbol)
	country := suffixCountry(suffix)

	t.mu.Lock()
	defer t.mu.Unlock()

	// 1) Explicit ISIN override from config, or an ISIN given as the symbol.
	if isin, ok := t.isins[symbol]; ok {
		if listings := t.byISIN[strings.ToUpper(isin)]; len(listings) > 0 {
			return t.cache(symbol, preferCurrency(listings, t.preferCcy).Ticker), nil
		}
	}

	if isISIN(base) {
		if listings := t.byISIN[strings.ToUpper(base)]; len(listings) > 0 {
			return t.cache(symbol, preferCurrency(listings, t.preferCcy).Ticker), nil
		}
	}

	// 2) Match by short name, disambiguating by exchange suffix and currency.
	candidates := t.byShort[strings.ToUpper(base)]
	if len(candidates) == 0 {
		return "", fmt.Errorf("trading212: no instrument for %q", symbol)
	}

	pick := pickListing(candidates, country, t.preferCcy)

	return t.cache(symbol, pick.Ticker), nil
}

// cache records the resolution both ways and returns the ticker. Caller holds mu.
func (t *Trading212) cache(symbol, ticker string) string {
	t.resolved[symbol] = ticker
	t.reverse[ticker] = symbol
	return ticker
}

// remember caches a symbol↔ticker resolution in both directions.
func (t *Trading212) remember(symbol, ticker string) {
	t.mu.Lock()

	t.resolved[symbol] = ticker
	t.reverse[ticker] = symbol
	t.mu.Unlock()
}

// symbolFor maps a Trading 212 ticker back to the input symbol the bot used
// (so positions/orders match the watchlist), falling back to a heuristic.
func (t *Trading212) symbolFor(ticker string) string {
	t.mu.Lock()

	s, ok := t.reverse[ticker]
	t.mu.Unlock()

	if ok {
		return s
	}

	return symbolFromTicker(ticker)
}

// loadInstruments downloads the instrument metadata and indexes it by short
// name and ISIN for symbol resolution.
func (t *Trading212) loadInstruments(ctx context.Context) error {
	var raw []struct {
		Ticker       string `json:"ticker"`
		ShortName    string `json:"shortName"`
		ISIN         string `json:"isin"`
		CurrencyCode string `json:"currencyCode"`
		Type         string `json:"type"`
	}

	if err := t.do(
		ctx,
		http.MethodGet,
		"/equity/metadata/instruments",
		"instruments",
		nil,
		&raw,
	); err != nil {
		return fmt.Errorf("load instruments: %w", err)
	}

	byShort := make(map[string][]t212Inst, len(raw))
	byISIN := make(map[string][]t212Inst, len(raw))

	for _, in := range raw {
		inst := t212Inst{
			Ticker:    in.Ticker,
			ShortName: in.ShortName,
			ISIN:      in.ISIN,
			Currency:  in.CurrencyCode,
		}
		key := strings.ToUpper(in.ShortName)

		if key == "" {
			key = strings.ToUpper(strings.Split(in.Ticker, "_")[0])
		}

		byShort[key] = append(byShort[key], inst)
		if in.ISIN != "" {
			isin := strings.ToUpper(in.ISIN)

			byISIN[isin] = append(byISIN[isin], inst)
		}
	}

	t.mu.Lock()

	t.byShort = byShort
	t.byISIN = byISIN
	t.loaded = true
	t.mu.Unlock()

	return nil
}

// splitSymbol splits "LXS.DE" into ("LXS","DE"); "AAPL" into ("AAPL","").
func splitSymbol(symbol string) (base, suffix string) {
	if i := strings.LastIndexByte(symbol, '.'); i > 0 {
		return symbol[:i], strings.ToUpper(symbol[i+1:])
	}

	return symbol, ""
}

// isISIN reports whether s looks like an ISIN (2 letters + 10 alphanumerics).
//
//nolint:cyclop // simple character-class validation
func isISIN(s string) bool {
	if len(s) != 12 {
		return false
	}

	isAlpha := func(c byte) bool { return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' }
	isAlnum := func(c byte) bool { return isAlpha(c) || c >= '0' && c <= '9' }

	if !isAlpha(s[0]) || !isAlpha(s[1]) {
		return false
	}

	for i := 2; i < 12; i++ {
		if !isAlnum(s[i]) {
			return false
		}
	}

	return true
}

// suffixCountry maps a Yahoo-style exchange suffix to the ISIN country prefix of
// the listing it denotes (used to disambiguate same-name listings). "" suffix
// means prefer the US listing.
//
//nolint:cyclop // one switch case per exchange suffix; a map would obscure grouping
func suffixCountry(suffix string) string {
	switch suffix {
	case "DE", "SG", "F", "MU", "DU", "HM", "HA", "BE", "BM": // German exchanges
		return "DE"
	case "PA":
		return "FR"
	case "AS":
		return "NL"
	case "BR":
		return "BE"
	case "L", "IL":
		return "GB"
	case "MI":
		return "IT"
	case "MC":
		return "ES"
	case "SW", "VX":
		return "CH"
	case "ST":
		return "SE"
	case "HE":
		return "FI"
	case "OL":
		return "NO"
	case "CO":
		return "DK"
	case "LS":
		return "PT"
	case "VI":
		return "AT"
	case "IR":
		return "IE"
	case "":
		return "US"
	default:
		return ""
	}
}

// preferCurrency returns the listing in the preferred currency, else the first.
func preferCurrency(listings []t212Inst, ccy string) t212Inst {
	if ccy != "" {
		for _, l := range listings {
			if strings.EqualFold(l.Currency, ccy) {
				return l
			}
		}
	}

	return listings[0]
}

// pickListing chooses a listing: for a US symbol prefer the US listing; for a
// suffixed symbol narrow to the country's listings (by ISIN prefix); then prefer
// the configured currency (e.g. EUR) among what remains.
func pickListing(candidates []t212Inst, country, ccy string) t212Inst {
	switch {
	case country == "US":
		var us []t212Inst

		for _, c := range candidates {
			if strings.Contains(c.Ticker, "_US_") ||
				strings.HasPrefix(strings.ToUpper(c.ISIN), "US") {
				us = append(us, c)
			}
		}

		if len(us) > 0 {
			return us[0] // US listings are USD; currency preference doesn't apply
		}

	case country != "":
		var inCountry []t212Inst

		for _, c := range candidates {
			if strings.HasPrefix(strings.ToUpper(c.ISIN), country) {
				inCountry = append(inCountry, c)
			}
		}

		if len(inCountry) > 0 {
			candidates = inCountry
		}
	}

	return preferCurrency(candidates, ccy)
}

// defaultT212QtyPrecision is the decimal precision a quantity is first rounded
// to. Trading 212 enforces a (lower) per-instrument precision, learned and
// cached from the API's error on the first order for each instrument.
const defaultT212QtyPrecision = 8

// SubmitOrder resolves the ticker, submits a market order at a quantity
// precision the instrument accepts (learning it from rejections), and waits
// for the asynchronous fill confirmation.
//
//nolint:cyclop // the precision-retry loop is one cohesive algorithm
func (t *Trading212) SubmitOrder(ctx context.Context, o Order) (OrderResult, error) {
	if o.Side != Buy && o.Side != Sell {
		return OrderResult{}, fmt.Errorf("trading212: invalid side %q", o.Side)
	}

	if o.Qty <= 0 {
		return OrderResult{}, fmt.Errorf("trading212: invalid quantity %v", o.Qty)
	}

	ticker, err := t.resolveTicker(ctx, o.Symbol)
	if err != nil {
		return OrderResult{}, err
	}

	// Trading 212 enforces two quantity rules and rejects an order that breaks
	// either: a per-instrument decimal precision ("quantity-precision-mismatch")
	// and "don't sell more than you own" ("selling-equity-not-owned"). Both are
	// fixed by sending FEWER decimals: round the quantity DOWN (never above the
	// owned amount) to a precision the instrument accepts. We don't know that
	// precision up front, so start at 8 dp (or the value learned earlier for this
	// ticker) and, on a quantity rejection, reduce precision — guided by the
	// precision the API reports — and retry, caching what finally works.
	prec := defaultT212QtyPrecision
	if p, ok := t.cachedQtyPrecision(ticker); ok {
		prec = p
	}

	var lastErr error

	for prec >= 0 {
		qty := roundDownQty(o.Qty, prec)
		if qty <= 0 {
			return OrderResult{}, fmt.Errorf(
				"trading212: quantity %v too small at precision %d",
				o.Qty,
				prec,
			)
		}

		res, err := t.postMarketOrder(ctx, ticker, o.Symbol, o.Side, qty)
		if err == nil {
			t.rememberQtyPrecision(ticker, prec)

			// Fills are asynchronous: the POST response reports status NEW with
			// filledQuantity 0. Wait for the real fill so the caller gets a
			// usable FilledQty/FilledPx (realized P&L depends on them).
			return t.confirmFill(ctx, o.Symbol, o.Side, qty, res), nil
		}

		if !isRetryableQtyErr(err) {
			return res, err // a different failure (auth, rate limit, …) — don't loop
		}

		lastErr = err
		// Retry with strictly fewer decimals than we actually sent (the sent
		// count may be below prec when the value has trailing zeros, so this skips
		// pointless identical retries). Jump straight to the precision the API
		// reports if it is lower still.
		next := decimalPlaces(qty) - 1
		if n, ok := parseReportedPrecision(err); ok && n < next {
			next = n
		}

		if next >= prec {
			next = prec - 1 // guarantee progress
		}

		prec = next
	}

	return OrderResult{}, lastErr
}

// decimalPlaces returns the number of fractional digits in v's shortest exact
// decimal representation.
func decimalPlaces(v float64) int {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return len(s) - i - 1
	}

	return 0
}

// postMarketOrder submits one market order at the given (already-rounded)
// quantity. Trading 212 encodes direction in the sign: positive buys, negative
// sells.
func (t *Trading212) postMarketOrder(
	ctx context.Context,
	ticker, symbol string,
	side Side,
	qty float64,
) (OrderResult, error) {
	signed := qty
	if side == Sell {
		signed = -qty
	}

	body := map[string]any{"ticker": ticker, "quantity": signed}

	var resp t212Order

	if err := t.do(
		ctx,
		http.MethodPost,
		"/equity/orders/market",
		"orders",
		body,
		&resp,
	); err != nil {
		return OrderResult{}, err
	}

	return resp.toResult(symbol, side), nil
}

// t212FillWait bounds how long confirmFill waits for Trading 212 to confirm an
// asynchronous market-order fill before returning the best-known state.
const t212FillWait = 25 * time.Second

// t212WorkingStatus reports whether an order status means "still heading for a
// fill" (keep polling). Anything unrecognised is treated as terminal so an
// unexpected status can never poll until the deadline for nothing.
func t212WorkingStatus(s string) bool {
	switch strings.ToUpper(s) {
	case "", "LOCAL", "UNCONFIRMED", "CONFIRMED", "NEW", "SUBMITTED",
		"PROCESSING", "WORKING", "PARTIALLY_FILLED", "REPLACING", "REPLACED":
		return true
	}

	return false
}

// confirmFill polls the order until Trading 212 reports a terminal state, so
// the returned OrderResult carries the REAL filled quantity and average price
// instead of the status:NEW/filledQuantity:0 snapshot the submission response
// contains (fills are asynchronous). Realized P&L / win-loss tracking depends
// on FilledQty > 0 on closing sells, so without this every round trip would be
// recorded as unfilled.
//
// A 404 means the order left the working set without us cancelling it — for a
// day market order that is a fill — so the submitted quantity is assumed (the
// caller's reference price stands in for the unknown fill price). On timeout,
// context cancellation or a non-transient error the best-known state is
// returned unchanged; rejected/cancelled orders keep their honest zero fill.
func (t *Trading212) confirmFill(
	ctx context.Context,
	symbol string,
	side Side,
	submittedQty float64,
	res OrderResult,
) OrderResult {
	if res.ID == "" {
		return res
	}

	deadline := time.Now().Add(t212FillWait)
	cur := res

	for ctx.Err() == nil && time.Now().Before(deadline) {
		o, status, err := t.fetchOrder(ctx, res.ID)
		switch {
		case status == http.StatusNotFound:
			cur.Status = "filled"
			if cur.FilledQty <= 0 {
				cur.FilledQty = submittedQty
			}

			return cur

		case status == http.StatusTooManyRequests:
			continue // transient; the pacer spaces the next attempt
		case err != nil:
			return cur
		}

		cur = o.toResult(symbol, side)
		cur.FilledPx = o.avgFillPrice()

		if strings.EqualFold(o.Status, "FILLED") {
			cur.Status = "filled"
			return cur
		}

		if !t212WorkingStatus(o.Status) {
			return cur // rejected/cancelled — report the real (zero) fill
		}
	}

	return cur
}

// fetchOrder GETs a single order for fill polling. Unlike do() it does NOT
// push 404s into the error log — a 404 is the expected "filled and gone from
// the working set" outcome, not a failure. It has its own pace group: order
// status is a separate endpoint from order placement with its own rate budget.
func (t *Trading212) fetchOrder(ctx context.Context, id string) (t212Order, int, error) {
	var o t212Order

	if err := t.pace(ctx, "order_status"); err != nil {
		return o, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.baseURL+"/equity/orders/"+id, nil)
	if err != nil {
		return o, 0, err
	}

	req.SetBasicAuth(t.apiKey, t.secret)
	req.Header.Set("Accept", "application/json")

	resp, err := t.http.Do(req)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			errlog.Record("trading212", err.Error())
		}

		return o, 0, err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusTooManyRequests:
		return o, resp.StatusCode, nil
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		err := fmt.Errorf(
			"trading212 GET /equity/orders/%s: status %d: %s",
			id,
			resp.StatusCode,
			string(data),
		)
		errlog.Record("trading212", err.Error())

		return o, resp.StatusCode, err
	}

	if err := json.Unmarshal(data, &o); err != nil {
		return o, resp.StatusCode, fmt.Errorf("trading212 decode order %s: %w", id, err)
	}

	return o, resp.StatusCode, nil
}

// cachedQtyPrecision returns the learned quantity precision for ticker.
func (t *Trading212) cachedQtyPrecision(ticker string) (int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	p, ok := t.qtyPrec[ticker]

	return p, ok
}

// rememberQtyPrecision caches the quantity precision that worked for ticker.
func (t *Trading212) rememberQtyPrecision(ticker string, p int) {
	t.mu.Lock()

	t.qtyPrec[ticker] = p
	t.mu.Unlock()
}

var qtyPrecisionRe = regexp.MustCompile(`precision (\d+)`)

// parseReportedPrecision extracts the decimal precision Trading 212 names in a
// quantity-precision error ("invalid quantity precision 3").
func parseReportedPrecision(err error) (int, bool) {
	if err == nil {
		return 0, false
	}

	m := qtyPrecisionRe.FindStringSubmatch(err.Error())
	if m == nil {
		return 0, false
	}

	n, e := strconv.Atoi(m[1])
	if e != nil || n < 0 {
		return 0, false
	}

	return n, true
}

// isRetryableQtyErr reports whether a failed order can be retried by sending a
// quantity with fewer decimals: a precision mismatch, or an over-sell that a
// rounding artifact pushed just above the owned amount.
func isRetryableQtyErr(err error) bool {
	if err == nil {
		return false
	}

	s := err.Error()

	return strings.Contains(s, "quantity-precision-mismatch") ||
		strings.Contains(s, "selling-equity-not-owned")
}

// GetAccount returns the account's free/total cash in the account currency.
func (t *Trading212) GetAccount(ctx context.Context) (Account, error) {
	var cash struct {
		Free     float64 `json:"free"`
		Total    float64 `json:"total"`
		Invested float64 `json:"invested"`
		Blocked  float64 `json:"blocked"`
	}

	if err := t.do(ctx, http.MethodGet, "/equity/account/cash", "cash", nil, &cash); err != nil {
		return Account{}, err
	}

	return Account{
		Cash:        cash.Free,
		Equity:      cash.Total,
		BuyingPower: cash.Free,
		Currency:    t.accountCurrency(ctx),
	}, nil
}

// accountCurrency returns the account currency, fetched once and cached.
func (t *Trading212) accountCurrency(ctx context.Context) string {
	t.mu.Lock()

	c := t.currency
	t.mu.Unlock()

	if c != "" {
		return c
	}

	var info struct {
		CurrencyCode string `json:"currencyCode"`
	}

	if err := t.do(ctx, http.MethodGet, "/equity/account/info", "info", nil, &info); err == nil {
		t.mu.Lock()

		t.currency = info.CurrencyCode
		t.mu.Unlock()

		return info.CurrencyCode
	}

	return ""
}

// GetPositions returns the portfolio mapped back to watchlist symbols.
func (t *Trading212) GetPositions(ctx context.Context) ([]Position, error) {
	var raw []struct {
		Ticker       string  `json:"ticker"`
		Quantity     float64 `json:"quantity"`
		AveragePrice float64 `json:"averagePrice"`
		CurrentPrice float64 `json:"currentPrice"`
		PPL          float64 `json:"ppl"`
	}

	if err := t.do(ctx, http.MethodGet, "/equity/portfolio", "portfolio", nil, &raw); err != nil {
		return nil, err
	}

	out := make([]Position, 0, len(raw))
	for _, p := range raw {
		out = append(out, Position{
			Symbol:       t.symbolFor(p.Ticker),
			Qty:          p.Quantity,
			AvgPrice:     p.AveragePrice,
			Current:      p.CurrentPrice,
			MarketValue:  p.CurrentPrice * p.Quantity,
			UnrealizedPL: p.PPL,
		})
	}

	return out, nil
}

// OpenOrders lists the account's working (unfilled) orders.
func (t *Trading212) OpenOrders(ctx context.Context) ([]OpenOrder, error) {
	var raw []struct {
		ID         json.Number `json:"id"`
		Ticker     string      `json:"ticker"`
		Quantity   float64     `json:"quantity"`
		Type       string      `json:"type"`
		Status     string      `json:"status"`
		LimitPrice float64     `json:"limitPrice"`
		StopPrice  float64     `json:"stopPrice"`
	}

	if err := t.do(ctx, http.MethodGet, "/equity/orders", "orders", nil, &raw); err != nil {
		return nil, err
	}

	out := make([]OpenOrder, 0, len(raw))
	for _, o := range raw {
		// Trading 212 encodes direction in the sign of the quantity.
		side := Buy
		if o.Quantity < 0 {
			side = Sell
		}

		price := o.LimitPrice
		if price == 0 {
			price = o.StopPrice
		}

		out = append(out, OpenOrder{
			OrderID: o.ID.String(),
			Symbol:  t.symbolFor(o.Ticker),
			Side:    side,
			Type:    o.Type,
			Qty:     absf(o.Quantity),
			Price:   price,
			Status:  o.Status,
		})
	}

	return out, nil
}

// CancelOrders cancels working orders by id (Trading 212 cancels one at a time).
func (t *Trading212) CancelOrders(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return errors.New("trading212: no order ids to cancel")
	}

	for _, id := range ids {
		if err := t.do(
			ctx,
			http.MethodDelete,
			"/equity/orders/"+id,
			"orders",
			nil,
			nil,
		); err != nil {
			return err
		}
	}

	return nil
}

// symbolFromTicker reduces a Trading 212 ticker (AAPL_US_EQ) to a plain symbol
// (AAPL) so it matches the engine watchlist and price feed.
func symbolFromTicker(ticker string) string {
	if i := strings.IndexByte(ticker, '_'); i > 0 {
		return ticker[:i]
	}

	return ticker
}

type t212Order struct {
	ID             json.Number `json:"id"`
	Ticker         string      `json:"ticker"`
	Quantity       float64     `json:"quantity"`
	FilledQuantity float64     `json:"filledQuantity"`
	FilledValue    float64     `json:"filledValue"`
	Status         string      `json:"status"`
}

// avgFillPrice derives the average fill price from filledValue/filledQuantity;
// 0 when nothing has filled (callers fall back to their reference price).
func (o t212Order) avgFillPrice() float64 {
	if q := absf(o.FilledQuantity); q > 0 && o.FilledValue != 0 {
		return absf(o.FilledValue) / q
	}

	return 0
}

// toResult maps a Trading 212 order payload onto the neutral OrderResult.
func (o t212Order) toResult(symbol string, side Side) OrderResult {
	return OrderResult{
		ID:          o.ID.String(),
		Symbol:      symbol,
		Side:        side,
		Qty:         absf(o.Quantity),
		FilledQty:   absf(o.FilledQuantity),
		Status:      orDefault(o.Status, "submitted"),
		SubmittedAt: time.Now(),
	}
}

// roundDownQty truncates a share quantity to dp decimal places. Rounding DOWN
// (never up) keeps a full-position sell at or below the owned amount, so
// Trading 212 doesn't reject it as "selling more than owned".
func roundDownQty(q float64, dp int) float64 {
	p := math.Pow(10, float64(dp))
	return math.Floor(q*p) / p
}

// absf returns the absolute value of v.
func absf(v float64) float64 {
	if v < 0 {
		return -v
	}

	return v
}

// orDefault returns s, or def when s is blank.
func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}

	return s
}
