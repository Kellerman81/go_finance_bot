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
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

	mu            sync.Mutex
	byShort       map[string][]t212Inst   // shortName -> listings (multiple exchanges)
	byISIN        map[string][]t212Inst   // ISIN -> listings (multiple currencies)
	resolved      map[string]string       // input symbol -> Trading 212 ticker (cache)
	reverse       map[string]string       // Trading 212 ticker -> input symbol
	qtyPrec       map[string]int          // ticker -> accepted quantity decimal places (learned)
	schedByTicker map[string]int          // ticker -> workingScheduleId
	tickerCcy     map[string]string       // ticker -> instrument's native trading currency
	tickerType    map[string]string       // ticker -> instrument type (STOCK, ETF, ...)
	schedules     map[int][]t212TimeEvent // workingScheduleId -> sorted trading-session events
	loaded        bool
	metaLoadedAt  time.Time // when instruments+exchanges were last (re)loaded
	currency      string

	fx *fxConverter // shared currency-conversion cache/client

	metaRefreshing atomic.Bool // guards a single background metadata refresh

	// rlMu guards only the creation of a group's rate limiter below, never the
	// pacing sleep itself — see pace and t212RateLimiter.
	rlMu     sync.Mutex
	limiters map[string]*t212RateLimiter

	// warnedLimit fires the limit-order downgrade notice once per adapter
	// instance — see SubmitOrder. cmd/bot builds exactly one, so in the live
	// bot that is once per process. It is a fact about the configuration, not
	// about any one order, so repeating it per submission would be noise.
	warnedLimit sync.Once
}

// t212RateLimiter paces one endpoint group independently of every other
// group. Each group gets its own mutex/lastCall pair, so a slow group
// sleeping out its interval (up to ~50s for the widest one) cannot block
// order submission or any other group's pacing — a single shared mutex held
// across the sleep, as this used to be, serialized all of Trading 212's
// traffic behind whichever group happened to be waiting.
type t212RateLimiter struct {
	mu       sync.Mutex
	lastCall time.Time
}

// t212TimeEvent is one trading-session transition from a working schedule, with
// the timestamp normalised to UTC (Trading 212 returns RFC3339 with a zone).
type t212TimeEvent struct {
	At   time.Time
	Type string // OPEN | CLOSE | BREAK_START | BREAK_END | PRE_MARKET_OPEN | …
}

// metaTTL bounds how long instrument/exchange metadata (including trading
// schedules, which carry holidays) is cached before a background refresh.
const metaTTL = 12 * time.Hour

// t212Limits are conservative per-endpoint minimum intervals (the Public API
// throttles aggressively; metadata especially).
var t212Limits = map[string]time.Duration{
	"cash":          2 * time.Second,
	"portfolio":     5 * time.Second,
	"orders":        2 * time.Second,
	"order_status":  2 * time.Second,  // fill polling (GET /equity/orders/{id})
	"order_history": 10 * time.Second, // GET /equity/history/orders (6 req / 1m0s)
	"info":          30 * time.Second,
	"instruments":   50 * time.Second,
	"exchanges":     30 * time.Second, // /equity/metadata/exchanges (1 req / 30s)
	"default":       2 * time.Second,
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

	httpClient := &http.Client{Timeout: 20 * time.Second}

	return &Trading212{
		baseURL:       strings.TrimRight(baseURL, "/"),
		apiKey:        apiKey,
		secret:        secret,
		isins:         isins,
		preferCcy:     strings.ToUpper(preferCcy),
		http:          httpClient,
		resolved:      make(map[string]string),
		reverse:       make(map[string]string),
		qtyPrec:       make(map[string]int),
		schedByTicker: make(map[string]int),
		tickerCcy:     make(map[string]string),
		tickerType:    make(map[string]string),
		schedules:     make(map[int][]t212TimeEvent),
		limiters:      make(map[string]*t212RateLimiter),
		fx:            newFXConverter(httpClient),
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
// context cancellation, so bursts cannot trip the API rate limiter. Only
// group's own limiter is held for the sleep — every other group paces
// independently.
func (t *Trading212) pace(ctx context.Context, group string) error {
	rl := t.rateLimiter(group)

	rl.mu.Lock()
	defer rl.mu.Unlock()

	limit, ok := t212Limits[group]
	if !ok {
		limit = t212Limits["default"]
	}

	if wait := limit - time.Since(rl.lastCall); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}

	rl.lastCall = time.Now()

	return nil
}

// rateLimiter returns group's limiter, creating it on first use. rlMu is
// held only for this lookup/creation, never across a pacing sleep.
func (t *Trading212) rateLimiter(group string) *t212RateLimiter {
	t.rlMu.Lock()
	defer t.rlMu.Unlock()

	rl, ok := t.limiters[group]
	if !ok {
		rl = &t212RateLimiter{}
		t.limiters[group] = rl
	}

	return rl
}

// do performs an authenticated read (GET/DELETE) and records failures in the
// error log. Order placement, which retries across quantity precisions, uses
// doRaw directly so intermediate rejections don't flood the log — it logs once
// on the final outcome.
func (t *Trading212) do(
	ctx context.Context,
	method, path, group string,
	out any,
) error {
	err := t.doRaw(ctx, method, path, group, nil, out)
	if err != nil && !errors.Is(err, context.Canceled) {
		errlog.Record("trading212", err.Error())
	}

	return err
}

// doRaw is do without the error-log side effect: it paces the endpoint group,
// makes the authenticated request, maps a non-2xx status to an error and
// decodes a 2xx body into out.
//
//nolint:cyclop // linear request/response handling; splitting hurts readability
func (t *Trading212) doRaw(
	ctx context.Context,
	method, path, group string,
	body, out any,
) (err error) {
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

	case instrumentUnavailable(resp.StatusCode, data):
		return fmt.Errorf(
			"%w: trading212 %s %s: %s",
			ErrInstrumentUnavailable,
			method,
			path,
			string(data),
		)

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
		t.maybeRefreshMeta() // keep schedules (holidays) fresh over long runs

		return tk, nil
	}

	t.mu.Unlock()

	if err := t.ensureMetadata(ctx); err != nil {
		return "", err
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

// ensureMetadata loads instrument metadata once (and, best-effort, the exchange
// schedules). A schedule-load failure is non-fatal: symbol resolution and order
// placement still work, only the broker-schedule market-hours gate falls back.
func (t *Trading212) ensureMetadata(ctx context.Context) error {
	t.mu.Lock()
	loaded := t.loaded
	t.mu.Unlock()

	if loaded {
		return nil
	}

	if err := t.loadInstruments(ctx); err != nil {
		return err
	}

	if err := t.loadExchanges(ctx); err != nil {
		errlog.Record("trading212", err.Error())
	}

	return nil
}

// loadInstruments downloads the instrument metadata and indexes it by short
// name and ISIN for symbol resolution.
func (t *Trading212) loadInstruments(ctx context.Context) error {
	var raw []struct {
		Ticker            string `json:"ticker"`
		ShortName         string `json:"shortName"`
		ISIN              string `json:"isin"`
		CurrencyCode      string `json:"currencyCode"`
		Type              string `json:"type"`
		WorkingScheduleID int    `json:"workingScheduleId"`
	}

	if err := t.do(
		ctx,
		http.MethodGet,
		"/equity/metadata/instruments",
		"instruments",
		&raw,
	); err != nil {
		return fmt.Errorf("load instruments: %w", err)
	}

	byShort := make(map[string][]t212Inst, len(raw))
	byISIN := make(map[string][]t212Inst, len(raw))
	schedByTicker := make(map[string]int, len(raw))
	tickerCcy := make(map[string]string, len(raw))
	tickerType := make(map[string]string, len(raw))

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

		schedByTicker[in.Ticker] = in.WorkingScheduleID
		tickerCcy[in.Ticker] = strings.ToUpper(in.CurrencyCode)
		tickerType[in.Ticker] = in.Type
	}

	t.mu.Lock()

	t.byShort = byShort
	t.byISIN = byISIN
	t.schedByTicker = schedByTicker
	t.tickerCcy = tickerCcy
	t.tickerType = tickerType
	t.loaded = true
	t.metaLoadedAt = time.Now()
	t.mu.Unlock()

	return nil
}

// t212RawTimeEvent, t212RawSchedule and t212RawExchange mirror the JSON shape of
// the /equity/metadata/exchanges response.
type t212RawTimeEvent struct {
	Date string `json:"date"`
	Type string `json:"type"`
}

type t212RawSchedule struct {
	ID         int                `json:"id"`
	TimeEvents []t212RawTimeEvent `json:"timeEvents"`
}

type t212RawExchange struct {
	ID               int               `json:"id"`
	Name             string            `json:"name"`
	WorkingSchedules []t212RawSchedule `json:"workingSchedules"`
}

// loadExchanges downloads the exchange working schedules and indexes each
// workingScheduleId to its trading-session events (sorted, UTC). Schedules are
// optional: a failure here leaves symbol resolution and order placement working,
// only the broker-schedule market-hours gate falls back to the suffix calendar.
func (t *Trading212) loadExchanges(ctx context.Context) error {
	var raw []t212RawExchange

	if err := t.do(
		ctx,
		http.MethodGet,
		"/equity/metadata/exchanges",
		"exchanges",
		&raw,
	); err != nil {
		return fmt.Errorf("load exchanges: %w", err)
	}

	schedules := make(map[int][]t212TimeEvent)

	for _, ex := range raw {
		for _, ws := range ex.WorkingSchedules {
			events := make([]t212TimeEvent, 0, len(ws.TimeEvents))

			for _, te := range ws.TimeEvents {
				at, err := time.Parse(time.RFC3339, te.Date)
				if err != nil {
					continue // skip an unparseable timestamp rather than fail the lot
				}

				events = append(events, t212TimeEvent{At: at.UTC(), Type: strings.ToUpper(te.Type)})
			}

			slices.SortFunc(events, func(a, b t212TimeEvent) int { return a.At.Compare(b.At) })

			schedules[ws.ID] = events
		}
	}

	t.mu.Lock()

	t.schedules = schedules
	t.mu.Unlock()

	return nil
}

// maybeRefreshMeta reloads instrument/exchange metadata in the background when
// it has aged past metaTTL, so long-running bots pick up schedule changes
// (holidays) without a restart. At most one refresh runs at a time, and it never
// blocks the caller.
func (t *Trading212) maybeRefreshMeta() {
	t.mu.Lock()
	stale := !t.metaLoadedAt.IsZero() && time.Since(t.metaLoadedAt) > metaTTL
	t.mu.Unlock()

	if !stale || !t.metaRefreshing.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer t.metaRefreshing.Store(false)

		bg := context.Background()
		if err := t.loadInstruments(bg); err != nil {
			return
		}

		_ = t.loadExchanges(bg)
	}()
}

// TradingOpen implements broker.MarketScheduler using Trading 212's exchange
// schedules. It is cache-only (never blocks on I/O): it reports known=false when
// the symbol isn't resolved yet, has no schedule, or t falls outside the cached
// window, so the caller falls back to its own calendar.
func (t *Trading212) TradingOpen(
	symbol string,
	at time.Time,
	preOpen time.Duration,
) (open, known bool) {
	t.mu.Lock()

	ticker, ok := t.resolved[symbol]
	if !ok && strings.Contains(symbol, "_") {
		ticker, ok = symbol, true // caller passed a Trading 212 ticker directly
	}

	sid, hasSched := t.schedByTicker[ticker]
	events := t.schedules[sid]
	t.mu.Unlock()

	if !ok || !hasSched || len(events) == 0 {
		return false, false
	}

	return sessionOpenAt(events, at.UTC(), preOpen)
}

// sessionOpenAt decides, from a schedule's sorted session events, whether the
// venue is in a regular trading session at `at` (allowing preOpen before the
// open). It returns known=false when `at` lies outside the events' covered
// window, so the caller can fall back rather than trust a guess. Only regular
// OPEN/CLOSE (and BREAK_START/BREAK_END) drive the decision; pre/after-hours
// events are ignored so the bot trades only the regular session.
func sessionOpenAt(events []t212TimeEvent, at time.Time, preOpen time.Duration) (open, known bool) {
	openCutoff := at.Add(preOpen)

	var lastOpen, lastClose, lastBreakStart, lastBreakEnd time.Time

	first, last := events[0].At, events[len(events)-1].At

	for _, e := range events {
		// OPEN may be looked at up to preOpen ahead of now; every other event
		// only counts once it has actually happened.
		cutoff := at
		if e.Type == "OPEN" {
			cutoff = openCutoff
		}

		if e.At.After(cutoff) {
			continue
		}

		switch e.Type {
		case "OPEN":
			lastOpen = e.At
		case "CLOSE":
			lastClose = e.At
		case "BREAK_START":
			lastBreakStart = e.At
		case "BREAK_END":
			lastBreakEnd = e.At
		}
	}

	// Outside the covered window: can't decide — let the caller fall back. A
	// generous lower margin still answers "closed" for the quiet hours before the
	// first known session; only genuinely stale/uncovered times defer.
	if at.Before(first.Add(-24*time.Hour)) || at.After(last) {
		return false, false
	}

	if lastOpen.IsZero() || (!lastClose.IsZero() && lastClose.After(lastOpen)) {
		return false, true // before the session's open, or after its close
	}

	// In session — unless currently within a lunch/trading break.
	if !lastBreakStart.IsZero() && (lastBreakEnd.IsZero() || lastBreakStart.After(lastBreakEnd)) {
		return false, true
	}

	return true, true
}

// instrumentUnavailable reports whether a Trading 212 error names the
// INSTRUMENT as untradeable rather than something about this particular order.
//
// Deliberately narrow. Order-level refusals — insufficient funds, below the
// minimum order value, selling stock you do not own — are transient and must
// not land here: treating one as permanent would disable a symbol for the rest
// of the process over a condition that clears by itself.
//
// The observed live case is `/api-errors/instrument-invisible` with
// "Instrument can not be traded", which Trading 212 returns for a US-domiciled
// ETF on an EU account.
func instrumentUnavailable(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusForbidden {
		return false
	}

	s := strings.ToLower(string(body))

	return strings.Contains(s, "instrument-invisible") ||
		strings.Contains(s, "instrumentnotfound") ||
		strings.Contains(s, "instrument-not-found") ||
		strings.Contains(s, "can not be traded") ||
		strings.Contains(s, "cannot be traded")
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

// pickUSListing picks a US line from candidates. A US ISIN says where the
// company is registered, not where the line trades: AAPL_GB_EQ (LSE) carries
// one too. So a real _US_ ticker wins, and the ISIN match is only the
// fallback — taking the first of a mixed bucket let the instruments payload's
// order decide between NASDAQ and London (BR2-8).
func pickUSListing(candidates []t212Inst) (t212Inst, bool) {
	var usISIN []t212Inst

	for _, c := range candidates {
		if strings.Contains(c.Ticker, "_US_") {
			return c, true // US listings are USD; currency preference doesn't apply
		}

		if strings.HasPrefix(strings.ToUpper(c.ISIN), "US") {
			usISIN = append(usISIN, c)
		}
	}

	if len(usISIN) == 0 {
		return t212Inst{}, false
	}

	return preferCurrency(usISIN, "USD"), true
}

// pickListing chooses a listing: for a US symbol prefer the US listing; for a
// suffixed symbol narrow to the country's listings (by ISIN prefix); then prefer
// the configured currency (e.g. EUR) among what remains.
func pickListing(candidates []t212Inst, country, ccy string) t212Inst {
	switch {
	case country == "US":
		if inst, ok := pickUSListing(candidates); ok {
			return inst
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

// maxT212QtyAttempts bounds the quantity-precision probing so a rejection can't
// trigger a barrage of order submissions against the live endpoint. The probe
// converges fast: the initial precision, the max the API reports, then whole
// shares as the decisive last attempt.
const maxT212QtyAttempts = 4

// SubmitOrder resolves the ticker and submits a market order at a quantity
// precision the instrument accepts, then waits for the asynchronous fill.
//
// Always a MARKET order: Trading 212's REST API exposes /equity/orders/market
// and nothing this adapter can place a resting limit order with. A
// Limit-typed order (what execution.type: "limit" produces) is therefore sent
// to market and crosses the spread its limit price was meant to avoid — a
// real difference in execution, so it is recorded to errlog once per adapter
// instance rather than absorbed silently.
//
// Trading 212 enforces a per-instrument decimal precision and rejects orders
// that exceed it ("quantity-precision-mismatch" / "must be limited to N decimal
// spaces"); some instruments accept only whole shares. The precision isn't known
// up front, so this rounds the quantity DOWN and probes: start at the cached (or
// 8-dp) precision, and on a precision rejection jump straight to the precision
// the API reports, then to whole shares — never a slow one-decimal walk. The
// precision that works is cached, so later orders (and, crucially, BUYs) use it
// immediately and never open a fractional position the instrument can't later
// sell. If no precision is accepted, or the quantity rounds to zero shares, the
// order is abandoned with a single clear error instead of repeated attempts.
//
//nolint:cyclop,funlen // the precision-probe loop is one cohesive algorithm
func (t *Trading212) SubmitOrder(ctx context.Context, o Order) (OrderResult, error) {
	if o.Side != Buy && o.Side != Sell {
		return OrderResult{}, fmt.Errorf("trading212: invalid side %q", o.Side)
	}

	if o.Qty <= 0 {
		return OrderResult{}, fmt.Errorf("trading212: invalid quantity %v", o.Qty)
	}

	// This adapter has only /equity/orders/market. A Limit order — what
	// execution.type: "limit" makes every non-urgent clip — is therefore sent
	// to market, crossing the spread the limit price existed to avoid. That
	// is a real difference in what the operator gets, so it is surfaced
	// rather than silently absorbed; execution.type is broker-agnostic, so it
	// cannot be rejected at config load.
	if o.Type == Limit {
		t.warnedLimit.Do(func() {
			errlog.Record("trading212", "execution.type is \"limit\" but this adapter has no "+
				"limit-order endpoint: orders are sent to market and cross the spread "+
				"(set execution.type: \"market\" to stop this, or use a broker with limit support)")
		})
	}

	ticker, err := t.resolveTicker(ctx, o.Symbol)
	if err != nil {
		return OrderResult{}, err
	}

	prec := defaultT212QtyPrecision
	if p, ok := t.cachedQtyPrecision(ticker); ok {
		prec = p
	}

	var (
		lastErr error
		capped  bool // the sell was already cut to the holding once
	)

	for attempt := 0; attempt < maxT212QtyAttempts && prec >= 0; attempt++ {
		qty := roundDownQty(o.Qty, prec)
		if qty <= 0 {
			// The only precision left rounds the order to zero shares — e.g. a
			// whole-share-only instrument and a sub-1-share order. Placing it is
			// impossible, so fail fast and clearly rather than looping.
			msg := fmt.Sprintf(
				"trading212: %s accepts at most %d decimal place(s), where quantity %v rounds to zero shares — order not placed (increase order size to trade whole shares)",
				o.Symbol,
				prec,
				o.Qty,
			)
			errlog.Record("trading212", msg)

			return OrderResult{}, errors.New(msg)
		}

		res, err := t.postMarketOrder(ctx, ticker, o.Symbol, o.Side, qty)
		if err == nil {
			// Cache the accepted precision so subsequent orders — especially
			// BUYs — round to it up front and never open a position finer than
			// this instrument can sell.
			t.rememberQtyPrecision(ticker, prec)

			// Fills are asynchronous: the POST response reports status NEW with
			// filledQuantity 0. Wait for the real fill so the caller gets a
			// usable FilledQty/FilledPx (realized P&L depends on them).
			result := t.confirmFill(ctx, ticker, o.Symbol, o.Side, res)

			// FilledPx comes back in the instrument's native currency (same as
			// GetPositions' averagePrice/currentPrice); convert it so callers can
			// combine it with account-currency money values (spend limits,
			// realized PnL) without repeating this conversion themselves.
			//
			// An UNRESOLVED rate means the fill price would go out in the
			// instrument's currency labelled as the account's — a $163 fill
			// booked as €163 skews entry price, daily spend, realized P&L and
			// the stop anchors by the exchange rate (BR2-3). The "keep 1:1 so
			// the order still goes out" reasoning does not apply: the order
			// has already gone out. Report no price instead; the engine falls
			// back to its own converted reference price.
			if result.FilledPx > 0 {
				if _, rate, resolved := t.currencyAndRate(ctx, ticker); resolved {
					result.FilledPx *= rate
				} else {
					result.FilledPx = 0
				}
			}

			return result, nil
		}

		// "Selling equity not owned" names no precision and is not a precision
		// problem: the sell asked for more than the account holds (a float
		// tail like 1.50000001 against 1.5). It was retried as one, which
		// jumped straight to whole shares — selling 1 of 1.5 — and cached
		// precision 0 for the ticker until restart, so the remaining 0.5
		// "rounds to zero shares" on every later exit (blocked for good,
		// invariant 1) and every fractional buy was floored (BR2-1). Cut the
		// sell to the real holding once, at the same precision, instead.
		if isNotOwnedErr(err) {
			if o.Side == Sell && !capped {
				if held, ok := t.heldTickerQty(ctx, ticker); ok && held > 0 && held < o.Qty {
					o.Qty, capped, lastErr = held, true, err

					continue
				}
			}

			errlog.Record("trading212", err.Error())

			return res, err
		}

		if !isRetryableQtyErr(err) {
			errlog.Record("trading212", err.Error()) // auth, rate limit, 5xx, …

			return res, err
		}

		lastErr = err

		if prec == 0 {
			break // whole shares already rejected — no lower precision to try
		}

		// Jump straight to the precision the API reports (its stated max), else
		// straight to whole shares — the decisive test — rather than walking one
		// decimal at a time and hammering the endpoint.
		next := 0
		if n, ok := parseReportedPrecision(err); ok && n >= 0 && n < prec {
			next = n
		}

		prec = next
	}

	// No precision the instrument accepts: it likely requires whole shares or is
	// not tradeable via the API. Report once, clearly, instead of leaving a wall
	// of rejected-attempt errors in the log.
	msg := fmt.Sprintf(
		"trading212: %s rejected quantity %v at every quantity precision tried (instrument likely requires whole shares or is not API-tradeable) — order not placed: %v",
		o.Symbol,
		o.Qty,
		lastErr,
	)
	errlog.Record("trading212", msg)

	return OrderResult{}, errors.New(msg)
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

	// doRaw (not do): SubmitOrder probes several quantity precisions and logs
	// once on the final outcome, so intermediate rejections must not be logged.
	if err := t.doRaw(
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

// t212FinalStatus reports whether an order status is final: filled,
// cancelled or rejected. Everything else — the working states, CANCELLING,
// and any status this code does not know — keeps polling, and an order still
// in one at the deadline stays UNCONFIRMED.
//
// It used to be the other way round: a list of working states, everything
// else terminal. CANCELLING was not on it, so an order polled mid-cancel was
// booked as a confirmed fill of whatever had filled so far, usually 0 — the
// reservation and pendingSell released, the shares landing later untracked,
// a sale booked as zero leaving a phantom position (BR3-4; Alpaca had the
// same inversion fixed as BR2-6). Unknown is not zero.
func t212FinalStatus(s string) bool {
	switch strings.ToUpper(s) {
	case "FILLED", "CANCELLED", "CANCELED", "REJECTED":
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
// A 404 means the order left the working set without us cancelling it — a
// liquid instrument's market order routinely does this before the very
// first poll even runs, so a 404 is the ordinary outcome, not the
// exception. It is also what a purged rejection or a wrong-account id look
// like, and this endpoint alone gives no way to tell them apart — so rather
// than guess, a 404 first falls through to settledFromHistory, which reads
// the order's actual terminal outcome from the account's order history
// instead of assuming one. Only when that lookup itself can't resolve it
// either is the order reported as unconfirmed (Terminal: false, Status:
// "unknown"), so the caller flags it for reconciliation instead of risking
// either a phantom position or a phantom loss. Genuinely terminal states
// (FILLED, or a broker-reported rejection/cancellation) set Terminal: true.
// On timeout, context cancellation or a non-transient error the best-known,
// non-terminal state is returned unchanged. See CODE_REVIEW_FINDINGS.md's
// "Trading212: every order reports unconfirmed" entry.
func (t *Trading212) confirmFill(
	ctx context.Context,
	ticker, symbol string,
	side Side,
	res OrderResult,
) OrderResult {
	if res.ID == "" {
		return res
	}

	deadline := time.Now().Add(t212FillWait)
	cur := res

	for ctx.Err() == nil && time.Now().Before(deadline) {
		o, status, err := t.fetchOrder(ctx, res.ID)

		if status == http.StatusNotFound {
			if h, ok := t.settledFromHistory(ctx, ticker, symbol, side, res.ID); ok {
				return h
			}

			cur.Status = "unknown"
			cur.Terminal = false

			return cur
		}

		switch {
		case status == http.StatusTooManyRequests:
			continue // transient; the pacer spaces the next attempt
		case err != nil:
			return cur
		}

		cur = o.toResult(symbol, side)
		cur.FilledPx = o.avgFillPrice()

		if strings.EqualFold(o.Status, "FILLED") {
			cur.Status = "filled"
			cur.Terminal = true

			return cur
		}

		if t212FinalStatus(o.Status) {
			cur.Terminal = true // rejected/cancelled — a confirmed, real zero fill

			return cur
		}
	}

	return cur
}

// settledFromHistory looks up id in the account's order history, for when
// confirmFill's poll finds it already gone from the working set (a 404).
// Trading 212 has no get-by-id form for history, so this lists the most
// recent orders for ticker — id is always at or near the front of a
// just-submitted order's page — and searches for a match.
//
// ok is false if the lookup itself fails, or id genuinely isn't there:
// confirmFill's caller falls back to its existing unconfirmed/"unknown"
// result either way, exactly as it did before this lookup existed. This
// deliberately does not retry on a miss — a single extra read, not another
// poll loop — so a genuine failure (network, auth, an account this endpoint
// can't reach) still surfaces promptly instead of running out the fill-wait
// deadline for nothing.
func (t *Trading212) settledFromHistory(
	ctx context.Context,
	ticker, symbol string,
	side Side,
	id string,
) (OrderResult, bool) {
	var page t212HistoryPage

	if err := t.do(
		ctx,
		http.MethodGet,
		"/equity/history/orders?ticker="+url.QueryEscape(ticker)+"&limit=50",
		"order_history",
		&page,
	); err != nil {
		return OrderResult{}, false
	}

	for _, item := range page.Items {
		if item.Order.ID.String() != id || !t212FinalStatus(item.Order.Status) {
			continue
		}

		res := item.Order.toResult(symbol, side)
		res.FilledPx = item.Order.avgFillPrice()
		res.Terminal = true

		if strings.EqualFold(item.Order.Status, "FILLED") {
			res.Status = "filled"
		}

		return res, true
	}

	return OrderResult{}, false
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

// qtyPrecisionRe captures the decimal-place count in either wording Trading 212
// uses: "invalid quantity precision 3" and "must be limited to 0 decimal spaces".
var qtyPrecisionRe = regexp.MustCompile(`(?:precision|limited to)\s+(\d+)`)

// parseReportedPrecision extracts the decimal precision Trading 212 names in a
// quantity-precision error ("invalid quantity precision 3", or "must be limited
// to 0 decimal spaces").
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
// quantity with fewer decimals: a precision mismatch (in either wording Trading
// 212 uses), or an over-sell that a rounding artifact pushed just above the
// owned amount.
func isRetryableQtyErr(err error) bool {
	if err == nil {
		return false
	}

	s := err.Error()

	return strings.Contains(s, "quantity-precision-mismatch") ||
		strings.Contains(s, "decimal space") // "must be limited to N decimal spaces"
}

// isNotOwnedErr reports Trading 212's "selling equity not owned" rejection,
// in either spelling it uses.
func isNotOwnedErr(err error) bool {
	if err == nil {
		return false
	}

	s := err.Error()

	return strings.Contains(s, "selling-equity-not-owned") || strings.Contains(s, "SellingEquityNotOwned")
}

// heldTickerQty returns how much of ticker the account holds, straight from
// the portfolio endpoint (no FX — a quantity needs none).
func (t *Trading212) heldTickerQty(ctx context.Context, ticker string) (float64, bool) {
	var raw []struct {
		Ticker   string  `json:"ticker"`
		Quantity float64 `json:"quantity"`
	}

	if err := t.do(ctx, http.MethodGet, "/equity/portfolio", "portfolio", &raw); err != nil {
		return 0, false
	}

	for _, p := range raw {
		if p.Ticker == ticker {
			return p.Quantity, true
		}
	}

	return 0, false
}

// GetAccount returns the account's free/total cash in the account currency.
// GetAccount returns the account's cash/equity/buying power, converted to
// preferCcy if it differs from the account's own currency.
//
// Without this conversion, GetAccount reported cash in the account's native
// currency while GetPositions (below) already converted position values to
// preferCcy — Engine.syncPortfolio sums the two into one equity figure with
// no idea they're in different currencies. preferCcy defaults to "EUR" with
// nothing prompting a user to set it to match their actual account currency,
// so this was reachable on ordinary defaults, not just an edge case. See
// CODE_REVIEW_FINDINGS.md's T212 cash/position currency mismatch finding.
func (t *Trading212) GetAccount(ctx context.Context) (Account, error) {
	var cash struct {
		Free     float64 `json:"free"`
		Total    float64 `json:"total"`
		Invested float64 `json:"invested"`
		Blocked  float64 `json:"blocked"`
	}

	if err := t.do(ctx, http.MethodGet, "/equity/account/cash", "cash", &cash); err != nil {
		return Account{}, err
	}

	ccy := t.accountCurrency(ctx)
	free, total := cash.Free, cash.Total

	if t.preferCcy != "" && ccy != "" && ccy != t.preferCcy {
		if rate, err := t.fx.Rate(ctx, ccy, t.preferCcy); err != nil {
			recordFXFailure("trading212", err, "balances shown at 1:1 and will be wrong until the rate resolves")
		} else {
			free, total = free*rate, total*rate
			ccy = t.preferCcy
		}
	}

	return Account{
		Cash:        free,
		Equity:      total,
		BuyingPower: free,
		Currency:    ccy,
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

	if err := t.do(ctx, http.MethodGet, "/equity/account/info", "info", &info); err == nil {
		t.mu.Lock()

		t.currency = info.CurrencyCode
		t.mu.Unlock()

		return info.CurrencyCode
	}

	return ""
}

// GetPositions returns the portfolio mapped back to watchlist symbols.
//
// Trading 212 reports averagePrice/currentPrice in the instrument's own
// listing currency (e.g. USD for a US-domiciled stock like NVDA), not the
// account currency — even when preferCcy is EUR, since preferCcy only picks
// among multiple listings of the *same* instrument and most US stocks have
// no EUR listing to pick. Left unconverted, that price gets summed straight
// into EUR cash elsewhere and inflates equity by roughly the FX rate. ppl,
// by contrast, is already reported in account currency, so it's passed
// through as-is.
func (t *Trading212) GetPositions(ctx context.Context) ([]Position, error) {
	// tickerCcy (needed to know which positions require FX conversion) is
	// populated by loadInstruments; ResolveSymbol usually triggers that
	// first, but GetPositions must not rely on that ordering.
	if err := t.ensureMetadata(ctx); err != nil {
		errlog.Recordf("trading212", "positions: metadata unavailable, currency conversion skipped: %s", err)
	}

	var raw []struct {
		Ticker       string  `json:"ticker"`
		Quantity     float64 `json:"quantity"`
		AveragePrice float64 `json:"averagePrice"`
		CurrentPrice float64 `json:"currentPrice"`
		PPL          float64 `json:"ppl"`
	}

	if err := t.do(ctx, http.MethodGet, "/equity/portfolio", "portfolio", &raw); err != nil {
		return nil, err
	}

	out := make([]Position, 0, len(raw))
	for _, p := range raw {
		ccy, rate, resolved := t.currencyAndRate(ctx, p.Ticker)

		// A failed fetch still uses rate (1, a stopgap) to convert THIS
		// position's prices — better than reporting native-currency numbers
		// mislabeled as preferCcy — but must not claim FXRate as a fact: the
		// engine treats any nonzero FXRate as authoritative and caches it
		// (see Engine.syncPortfolio), so a fallback value there would
		// silently overwrite a previously-correct cached rate. See
		// CODE_REVIEW_FINDINGS.md's FX-fallback finding.
		reportedRate := rate
		if !resolved {
			reportedRate = 0
		}

		out = append(out, Position{
			Symbol:       t.symbolFor(p.Ticker),
			Qty:          p.Quantity,
			AvgPrice:     p.AveragePrice * rate,
			Current:      p.CurrentPrice * rate,
			MarketValue:  p.CurrentPrice * p.Quantity * rate,
			UnrealizedPL: p.PPL,
			Currency:     ccy,
			FXRate:       reportedRate,
		})
	}

	return out, nil
}

// currencyAndRate returns ticker's native trading currency and the
// multiplier to convert 1 unit of it into preferCcy (1 if they're the same,
// unknown, or the rate can't be fetched — logged in that last case so a
// stuck rate is visible in diagnostics rather than silently mispricing).
// resolved is false only when a genuine fetch was attempted and failed —
// callers that report a position's FXRate (as opposed to ones just pricing
// an order, where 1:1 is an acceptable stopgap) must use it to leave FXRate
// at its zero value rather than reporting a fallback 1 as if it were a real
// rate: see GetPositions and CODE_REVIEW_FINDINGS.md's FX-fallback finding.
func (t *Trading212) currencyAndRate(ctx context.Context, ticker string) (ccy string, rate float64, resolved bool) {
	t.mu.Lock()
	ccy = t.tickerCcy[ticker]
	t.mu.Unlock()

	if ccy == "" || ccy == t.preferCcy {
		return ccy, 1, true
	}

	rate, err := t.fx.Rate(ctx, ccy, t.preferCcy)
	if err != nil {
		recordFXFailure("trading212", err, "rate unresolved: orders that need it are refused, positions stay in their native currency")
		return ccy, 1, false
	}

	return ccy, rate, true
}

// InstrumentCurrency implements CurrencyResolver.
//
// Unlike currencyAndRate (used for order pricing, where falling back to 1:1
// on a transient FX failure is an acceptable degraded mode — the order
// still goes out), this propagates a rate-fetch failure as a real error
// rather than silently returning 1. The engine's FX rate cache (see
// Engine.resolveFXRates/refreshFXRates) relies on that distinction to skip
// caching on failure instead of overwriting a previously-correct rate with
// 1 — see CODE_REVIEW_FINDINGS.md's FX-fallback finding.
func (t *Trading212) InstrumentCurrency(ctx context.Context, symbol string) (string, float64, error) {
	ticker, err := t.resolveTicker(ctx, symbol)
	if err != nil {
		return "", 1, err
	}

	t.mu.Lock()
	ccy := t.tickerCcy[ticker]
	t.mu.Unlock()

	if ccy == "" || ccy == t.preferCcy {
		return ccy, 1, nil
	}

	rate, err := t.fx.Rate(ctx, ccy, t.preferCcy)
	if err != nil {
		return ccy, 1, fmt.Errorf("fx rate %s->%s: %w", ccy, t.preferCcy, err)
	}

	return ccy, rate, nil
}

// AssetClass reports symbol's instrument type from Trading 212's own metadata
// ("STOCK", "ETF", ...), implementing AssetClassifier so the portfolio
// allocation caps can bucket a position the data provider could not classify.
//
// The metadata is already loaded and cached for symbol resolution, so this is
// normally a map lookup with no network call of its own.
func (t *Trading212) AssetClass(ctx context.Context, symbol string) (string, error) {
	ticker, err := t.resolveTicker(ctx, symbol)
	if err != nil {
		return "", err
	}

	t.mu.Lock()
	class := t.tickerType[ticker]
	t.mu.Unlock()

	if class == "" {
		return "", fmt.Errorf("trading212: no instrument type for %q", symbol)
	}

	return class, nil
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

	if err := t.do(ctx, http.MethodGet, "/equity/orders", "orders", &raw); err != nil {
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

// t212HistoryOrder is one entry from GET /equity/history/orders. The "order"
// side of the response mirrors t212Order's fields exactly, so it's reused
// here rather than duplicated.
type t212HistoryOrder struct {
	Order t212Order `json:"order"`
}

// t212HistoryPage is one (possibly partial) page of order history.
type t212HistoryPage struct {
	Items []t212HistoryOrder `json:"items"`
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
//
// Floor goes through floorUnits, which absorbs binary representation error:
// 0.29*100 is 28.999999999999996, which a bare Floor turned into 0.28 — a
// full exit that left 0.01 behind, above the dust threshold, for a second
// order or an adoption to deal with (BR2-5).
func roundDownQty(q float64, dp int) float64 {
	p := math.Pow(10, float64(dp))
	return floorUnits(q*p) / p
}

// unitsUlps is how many units in the last place of a scaled value count as
// representation error. Scaling q by 10^dp costs about one ulp, q's own
// decimal-to-binary error about one more; eight absorbs both.
//
// It was 1e-12 RELATIVE to the scaled value, which at 8 dp is half a unit at
// q ≈ 5000 and at 9 dp at q ≈ 500: above that "floor" rounded to nearest, so
// roundDownQty(600.0000000006, 9) came out 600.000000001 — above the holding,
// an Alpaca sell refused for insufficient quantity and the exit failed
// (BR3-3). An ulp tracks the representation error at every magnitude.
const unitsUlps = 8

// unitsTolerance is the representation error allowed around s.
func unitsTolerance(s float64) float64 {
	a := math.Abs(s)

	return unitsUlps * (math.Nextafter(a, math.Inf(1)) - a)
}

// floorUnits is Floor that treats a value within representation error of an
// integer as that integer.
func floorUnits(s float64) float64 {
	if r := math.Round(s); math.Abs(s-r) <= unitsTolerance(s) {
		return r
	}

	return math.Floor(s)
}

// ceilUnits is floorUnits' Ceil counterpart.
func ceilUnits(s float64) float64 {
	if r := math.Round(s); math.Abs(s-r) <= unitsTolerance(s) {
		return r
	}

	return math.Ceil(s)
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
