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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/errlog"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// Saxo is a Broker backed by the Saxo Bank OpenAPI. It targets the simulation
// gateway by default; point BaseURL at the live gateway for real trading.
//
// Auth uses an OAuth bearer token. For development, paste a 24-hour simulation
// token from the Saxo Developer Portal; for production, supply a token obtained
// via the OAuth code flow (refresh handling is the caller's responsibility).
//
// Saxo trades whole shares, so fractional quantities are floored; orders that
// floor to zero shares are rejected.
//
// NOTE: this adapter follows the documented OpenAPI request/response shapes but
// has not been exercised against a live Saxo account here — validate against a
// simulation token before relying on it.
type Saxo struct {
	baseURL    string
	tokens     TokenSource
	assetTypes string // instrument search scope, e.g. "Stock,Etf"

	http *http.Client

	accountKey string
	clientKey  string

	preferCcy string // preferred reporting currency (e.g. EUR); empty = don't convert
	fx        *fxConverter

	mu  sync.Mutex
	uic map[string]instrument // symbol -> resolved instrument (cached)
	// uicSymbol is the FIRST watchlist symbol each Uic was resolved from:
	// symbolForUic used to pick from uic by map iteration, so two symbols
	// resolving to one listing (SAP and SAP.DE → SAP:xetr) named the
	// position at random from sync to sync — BR2-4's record/adoption
	// mismatch by another door (BR3-9).
	uicSymbol map[int]string
	// ticks caches each instrument's tick-size scheme (see roundToTick).
	ticks map[int]saxoTickScheme

	// Order pacing to respect Saxo's ~1 order/second per-session limit.
	orderMu          sync.Mutex
	lastOrder        time.Time
	minOrderInterval time.Duration
}

// SetMinOrderInterval sets the minimum gap enforced between order submissions.
func (s *Saxo) SetMinOrderInterval(d time.Duration) { s.minOrderInterval = d }

// pace blocks until at least minOrderInterval has elapsed since the previous
// order, honouring context cancellation. It serialises order submission so
// rapid bursts cannot exceed the broker's rate limit.
func (s *Saxo) pace(ctx context.Context) error {
	s.orderMu.Lock()
	defer s.orderMu.Unlock()

	if s.minOrderInterval <= 0 {
		return nil
	}

	wait := s.minOrderInterval - time.Since(s.lastOrder)
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}

	s.lastOrder = time.Now()

	return nil
}

type instrument struct {
	Uic       int
	AssetType string
	Currency  string // native trading currency, if the search response reported one
}

// NewSaxo constructs a Saxo broker and resolves the account/client keys. If
// accountKey is empty, the first account from /port/v1/accounts/me is used.
// The TokenSource supplies a (possibly auto-refreshing) bearer token per
// request. preferCcy, if set, converts position values reported in a foreign
// listing currency (e.g. a US stock trading in USD) and account balances into
// this currency (empty means "don't convert").
func NewSaxo(baseURL string, tokens TokenSource, accountKey, assetTypes, preferCcy string) (*Saxo, error) {
	if assetTypes == "" {
		assetTypes = "Stock,Etf"
	}

	httpClient := &http.Client{Timeout: 20 * time.Second}

	s := &Saxo{
		baseURL:    strings.TrimRight(baseURL, "/"),
		tokens:     tokens,
		assetTypes: assetTypes,
		http:       httpClient,
		accountKey: accountKey,
		preferCcy:  strings.ToUpper(preferCcy),
		fx:         newFXConverter(httpClient),
		uic:        make(map[string]instrument),
		uicSymbol:  make(map[int]string),
		ticks:      make(map[int]saxoTickScheme),
	}
	if err := s.resolveAccount(context.Background()); err != nil {
		return nil, err
	}

	return s, nil
}

// NewSaxoHistory builds a Saxo client for chart history only (no account
// resolution), so Saxo candles can warm up indicators even when the execution
// broker is something else (e.g. Trading 212). Candles is the only method
// guaranteed to work on the returned value.
func NewSaxoHistory(baseURL string, tokens TokenSource, assetTypes string) *Saxo {
	if assetTypes == "" {
		assetTypes = "Stock,Etf"
	}

	httpClient := &http.Client{Timeout: 20 * time.Second}

	return &Saxo{
		baseURL:    strings.TrimRight(baseURL, "/"),
		tokens:     tokens,
		assetTypes: assetTypes,
		http:       httpClient,
		fx:         newFXConverter(httpClient),
		uic:        make(map[string]instrument),
		uicSymbol:  make(map[int]string),
		ticks:      make(map[int]saxoTickScheme),
	}
}

// Name identifies the broker adapter.
func (*Saxo) Name() string { return "saxo" }

// saxoIdentifierParams are the query parameters several Saxo endpoints carry
// (ClientKey/AccountKey identify the account, not a credential) that do
// blank on being logged: an operator can freely quote a client/account key,
// but a full request path (with these attached, as orderWorking/GetAccount
// and others build it) landing verbatim in the UI-visible errlog ring is
// more than the error itself needs to say. See CODE_REVIEW_FINDINGS.md's
// Saxo-request-path-logging finding.
var saxoIdentifierParams = []string{"ClientKey", "AccountKey"}

// redactSaxoPath blanks saxoIdentifierParams' values out of path's query
// string, for use in error messages/logs. The real path (with the real
// values) is still what the actual HTTP request uses — only what reaches
// errlog is redacted.
// saxoTransportErr rebuilds a transport error around the redacted path. A
// DNS, TCP, TLS or timeout failure is net/http's *url.Error, whose Error()
// embeds the whole request URL — ClientKey and AccountKey included — so only
// the status paths were redacted, and every sync of a network outage wrote
// the raw identifiers into the UI-served errlog ring (EL3-3; notify's
// sanitizeTransportErr fixed the same thing there).
func saxoTransportErr(method, safePath string, err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return fmt.Errorf("saxo %s %s: %w", method, safePath, uerr.Err)
	}

	return fmt.Errorf("saxo %s %s: %w", method, safePath, err)
}

func redactSaxoPath(path string) string {
	base, query, hasQuery := strings.Cut(path, "?")
	if !hasQuery {
		return path
	}

	q, err := url.ParseQuery(query)
	if err != nil {
		return base + "?<redacted>"
	}

	redacted := false

	for _, p := range saxoIdentifierParams {
		if q.Has(p) {
			q.Set(p, "REDACTED")
			redacted = true
		}
	}

	if !redacted {
		return path
	}

	return base + "?" + q.Encode()
}

// do performs an authenticated Saxo OpenAPI call, recording failures in the
// error log and decoding the JSON response into out.
//
//nolint:cyclop // linear request/response handling; splitting hurts readability
func (s *Saxo) do(ctx context.Context, method, path string, body, out any) (err error) {
	// path itself (used below to build the actual request) may carry
	// ClientKey/AccountKey in its query string; every error message built
	// from it here uses this redacted copy instead, so those identifiers
	// never reach the UI-visible errlog ring. See redactSaxoPath.
	safePath := redactSaxoPath(path)

	defer func() {
		if err != nil {
			errlog.Record("saxo", err.Error())
		}
	}()

	var reader io.Reader

	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}

		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, reader)
	if err != nil {
		return saxoTransportErr(method, safePath, err)
	}

	tok, err := s.tokens.Token(ctx)
	if err != nil {
		return fmt.Errorf("saxo token: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := s.http.Do(req)
	if err != nil {
		return saxoTransportErr(method, safePath, err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf(
			"saxo %s %s: 401 unauthorized — token expired/invalid, or token/gateway environment mismatch "+
				"(SIM tokens only work on .../sim/openapi, live tokens only on .../openapi); base_url=%s",
			method,
			safePath,
			s.baseURL,
		)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("saxo %s %s: status %d: %s", method, safePath, resp.StatusCode, string(data))
	}

	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("saxo decode %s: %w", safePath, err)
		}
	}

	return nil
}

// saxoAccount is one entry of the /port/v1/accounts/me response.
type saxoAccount struct {
	AccountKey string `json:"AccountKey"`
	ClientKey  string `json:"ClientKey"`
	AccountID  string `json:"AccountId"`
	Active     bool   `json:"Active"`
}

// resolveAccount fetches the account and client keys for subsequent calls.
func (s *Saxo) resolveAccount(ctx context.Context) error {
	var resp struct {
		Data []saxoAccount `json:"Data"`
	}

	if err := s.do(ctx, http.MethodGet, "/port/v1/accounts/me", nil, &resp); err != nil {
		return fmt.Errorf("resolve account: %w", err)
	}

	if len(resp.Data) == 0 {
		return errors.New("saxo: no accounts returned")
	}

	for _, a := range resp.Data {
		if s.accountKey != "" && a.AccountKey != s.accountKey {
			continue
		}

		s.accountKey = a.AccountKey
		s.clientKey = a.ClientKey

		return nil
	}

	// Requested accountKey not found; fall back to the first account.
	s.accountKey = resp.Data[0].AccountKey
	s.clientKey = resp.Data[0].ClientKey

	return nil
}

// saxoInstrumentHit is one entry of the instrument-search response.
type saxoInstrumentHit struct {
	Identifier   int    `json:"Identifier"`
	AssetType    string `json:"AssetType"`
	Symbol       string `json:"Symbol"`
	Description  string `json:"Description"`
	CurrencyCode string `json:"CurrencyCode"`
}

// saxoExchangeForSuffix maps the Yahoo-style exchange suffixes the watchlist
// uses onto Saxo's exchange codes (the part after ":" in a Saxo symbol).
var saxoExchangeForSuffix = map[string]string{
	"DE": "xetr", "F": "xfra", "L": "xlon", "PA": "xpar", "AS": "xams",
	"MI": "xmil", "SW": "xswx", "MC": "xmad", "BR": "xbru", "VI": "xwbo",
	"ST": "xsto", "CO": "xcse", "HE": "xhel", "OL": "xosl", "TO": "xtse",
}

// pickSaxoInstrument chooses the search hit for symbol. A watchlist symbol
// like "SAP.DE" never equalled a Saxo symbol like "SAP:xetr", so the lookup fell
// back to the FIRST hit — often another listing, e.g. the NYSE ADR priced in
// USD (BR2-4). Preference: the base ticker on the exchange the suffix names,
// then the base ticker anywhere, then the first hit.
func pickSaxoInstrument(hits []saxoInstrumentHit, symbol string) saxoInstrumentHit {
	base, suffix, _ := strings.Cut(symbol, ".")
	exchange := saxoExchangeForSuffix[strings.ToUpper(suffix)]

	split := func(h saxoInstrumentHit) (string, string) {
		t, ex, _ := strings.Cut(h.Symbol, ":")

		return t, ex
	}

	if exchange != "" {
		for _, h := range hits {
			if t, ex := split(h); strings.EqualFold(t, base) && strings.EqualFold(ex, exchange) {
				return h
			}
		}
	}

	for _, h := range hits {
		if t, _ := split(h); strings.EqualFold(t, symbol) || strings.EqualFold(t, base) {
			return h
		}
	}

	return hits[0]
}

// symbolForUic returns the watchlist symbol an instrument was resolved from,
// or fallback when it was never resolved here. Positions and orders come back
// with Saxo's own bare ticker ("SAP" for SAP.DE); without this the engine's
// record for SAP.DE never matched the holding, a separate "SAP" was adopted,
// and reconcile cleared SAP.DE's double-sell guard while its sell was still
// working (BR2-4).
func (s *Saxo) symbolForUic(uic int, fallback string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sym, ok := s.uicSymbol[uic]; ok {
		return sym
	}

	// An instrument cached without a recorded first symbol: the
	// alphabetically first one that maps to it, never map order.
	var match []string

	for sym, inst := range s.uic {
		if inst.Uic == uic {
			match = append(match, sym)
		}
	}

	if len(match) > 0 {
		return slices.Min(match)
	}

	return fallback
}

// resolveInstrument maps a ticker to a Saxo Uic and asset type, with caching.
func (s *Saxo) resolveInstrument(ctx context.Context, symbol string) (instrument, error) {
	s.mu.Lock()

	if inst, ok := s.uic[symbol]; ok {
		s.mu.Unlock()

		return inst, nil
	}

	s.mu.Unlock()

	q := url.Values{}
	q.Set("Keywords", symbol)
	q.Set("AssetTypes", s.assetTypes)

	var resp struct {
		Data []saxoInstrumentHit `json:"Data"`
	}

	if err := s.do(ctx, http.MethodGet, "/ref/v1/instruments?"+q.Encode(), nil, &resp); err != nil {
		return instrument{}, fmt.Errorf("instrument lookup %s: %w", symbol, err)
	}

	if len(resp.Data) == 0 {
		return instrument{}, fmt.Errorf("saxo: no instrument found for %q", symbol)
	}

	pick := pickSaxoInstrument(resp.Data, symbol)

	inst := instrument{
		Uic: pick.Identifier, AssetType: pick.AssetType,
		Currency: strings.ToUpper(pick.CurrencyCode),
	}

	s.mu.Lock()

	s.uic[symbol] = inst
	if s.uicSymbol == nil {
		s.uicSymbol = make(map[int]string)
	}

	if _, taken := s.uicSymbol[inst.Uic]; !taken {
		s.uicSymbol[inst.Uic] = symbol
	}

	s.mu.Unlock()

	return inst, nil
}

// fxRateForCurrency returns the multiplier to convert 1 unit of ccy into
// preferCcy (1 if they're the same, ccy is unknown, no preferCcy is
// configured, or the rate can't be fetched — logged in that last case).
func (s *Saxo) fxRateForCurrency(ctx context.Context, ccy string) float64 {
	rate, _ := s.fxRateForCurrencyResolved(ctx, ccy)
	return rate
}

// fxRateForCurrencyResolved is fxRateForCurrency plus whether the rate is a
// genuine fact (same currency, or a successful fetch) rather than a 1:1
// fallback after a failed one. A caller pricing an order can use
// fxRateForCurrency's plain form — 1:1 is an acceptable stopgap there, the
// order still goes out. A caller reporting a position's FXRate (GetPositions)
// must use this form and leave FXRate at its zero value on !resolved, rather
// than reporting the fallback 1 as if it were real: the engine treats any
// nonzero FXRate as authoritative and caches it (see Engine.syncPortfolio),
// so a fallback value there would silently overwrite a previously-correct
// cached rate. See CODE_REVIEW_FINDINGS.md's FX-fallback finding.
func (s *Saxo) fxRateForCurrencyResolved(ctx context.Context, ccy string) (rate float64, resolved bool) {
	if s.preferCcy == "" || ccy == "" || ccy == s.preferCcy {
		return 1, true
	}

	rate, err := s.fx.Rate(ctx, ccy, s.preferCcy)
	if err != nil {
		recordFXFailure("saxo", err, "rate unresolved: orders that need it are refused, positions stay in their native currency")
		return 1, false
	}

	return rate, true
}

// InstrumentCurrency implements CurrencyResolver.
//
// Unlike fxRateForCurrency (used for order pricing, where falling back to
// 1:1 on a transient FX failure is an acceptable degraded mode — the order
// still goes out), this propagates a rate-fetch failure as a real error
// rather than silently returning 1. The engine's FX rate cache (see
// Engine.resolveFXRates/refreshFXRates) relies on that distinction to skip
// caching on failure instead of overwriting a previously-correct rate with
// 1 — see CODE_REVIEW_FINDINGS.md's FX-fallback finding.
func (s *Saxo) InstrumentCurrency(ctx context.Context, symbol string) (string, float64, error) {
	inst, err := s.resolveInstrument(ctx, symbol)
	if err != nil {
		return "", 1, err
	}

	rate, resolved := s.fxRateForCurrencyResolved(ctx, inst.Currency)
	if !resolved {
		return inst.Currency, 1, fmt.Errorf("fx rate %s->%s: fetch failed", inst.Currency, s.preferCcy)
	}

	return inst.Currency, rate, nil
}

// AssetClass reports symbol's Saxo AssetType ("Stock", "Etf", "Bond",
// "ContractFutures", ...), implementing AssetClassifier so the portfolio
// allocation caps can bucket a position the data provider could not classify.
//
// Note that the search is scoped to the configured saxo.asset_types, so an
// instrument outside that scope is not resolvable here at all — which is
// correct: the bot cannot trade it either.
func (s *Saxo) AssetClass(ctx context.Context, symbol string) (string, error) {
	inst, err := s.resolveInstrument(ctx, symbol)
	if err != nil {
		return "", err
	}

	if inst.AssetType == "" {
		return "", fmt.Errorf("saxo: no asset type for %q", symbol)
	}

	return inst.AssetType, nil
}

// SubmitOrder resolves the instrument, floors the quantity to whole shares,
// places a day order and, for market orders, waits for the fill confirmation.
//
//nolint:cyclop // validation, resolution and submission form one order flow
func (s *Saxo) SubmitOrder(ctx context.Context, o Order) (OrderResult, error) {
	// Validate before doing any work — reject malformed orders locally.
	if o.Side != Buy && o.Side != Sell {
		return OrderResult{}, fmt.Errorf("saxo: invalid side %q", o.Side)
	}

	if o.Qty <= 0 || math.IsNaN(o.Qty) || math.IsInf(o.Qty, 0) {
		return OrderResult{}, fmt.Errorf("saxo: invalid quantity %v", o.Qty)
	}

	if o.Type == Limit && (o.LimitPx <= 0 || math.IsNaN(o.LimitPx)) {
		return OrderResult{}, errors.New("saxo: limit order requires a positive limit price")
	}

	inst, err := s.resolveInstrument(ctx, o.Symbol)
	if err != nil {
		return OrderResult{}, err
	}

	// floorUnits, not math.Floor: a scale-out's 100 × 0.29 is
	// 28.999999999999996 and floored to 28 (BR3-6, BR2-5's fix applied here).
	amount := floorUnits(o.Qty)
	if amount < 1 {
		return OrderResult{}, fmt.Errorf(
			"saxo: order for %s floors to 0 whole shares (qty %.4f)",
			o.Symbol,
			o.Qty,
		)
	}

	// Throttle to respect Saxo's per-session order rate limit.
	if err := s.pace(ctx); err != nil {
		return OrderResult{}, err
	}

	buySell := "Buy"
	if o.Side == Sell {
		buySell = "Sell"
	}

	orderType := "Market"
	if o.Type == Limit {
		orderType = "Limit"
	}

	body := map[string]any{
		"AccountKey":    s.accountKey,
		"Uic":           inst.Uic,
		"AssetType":     inst.AssetType,
		"Amount":        amount,
		"BuySell":       buySell,
		"OrderType":     orderType,
		"ManualOrder":   true,
		"OrderDuration": map[string]any{"DurationType": "DayOrder"},
	}
	if o.Type == Limit {
		// o.LimitPx arrives in preferCcy (like everything else this adapter
		// reports); Saxo expects OrderPrice in the instrument's own currency.
		//
		// Using fxRateForCurrency's plain fallback-to-1 form here (as this
		// used to) silently sends an unconverted price when the rate fetch
		// fails — a €150 stop-loss transmitted as OrderPrice: 150 in USD on
		// a $163 stock is off by ~9%, not just mislabeled. Refuse the order
		// instead: a failed submission is loud and retried next cycle; a
		// mispriced protective order is neither. See
		// CODE_REVIEW_FINDINGS.md's Alpaca/Saxo order-pricing FX finding.
		rate, resolved := s.fxRateForCurrencyResolved(ctx, inst.Currency)
		if !resolved {
			return OrderResult{}, fmt.Errorf(
				"saxo: cannot price limit order for %s: fx rate %s->%s unavailable",
				o.Symbol, inst.Currency, s.preferCcy)
		}

		body["OrderPrice"] = s.roundToTick(ctx, inst, o.LimitPx/rate)
	}

	var resp struct {
		OrderID string `json:"OrderId"`
	}

	if err := s.do(ctx, http.MethodPost, "/trade/v2/orders", body, &resp); err != nil {
		return OrderResult{}, err
	}

	res := OrderResult{
		ID:          resp.OrderID,
		Symbol:      o.Symbol,
		Side:        o.Side,
		Qty:         amount,
		Status:      "submitted",
		SubmittedAt: time.Now(),
	}
	// Saxo fills market orders asynchronously and the placement response has no
	// fill information at all. Wait for the order to leave the working set so
	// the caller gets a usable FilledQty (realized P&L depends on it). Limit
	// orders may legitimately stay working, so they are not waited on.
	if o.Type != Limit {
		res = s.confirmFill(ctx, res)
	}

	return res, nil
}

// saxoFillWait / saxoPollInterval bound how long confirmFill waits for a
// market order to leave Saxo's working-order set.
const (
	saxoFillWait     = 25 * time.Second
	saxoPollInterval = time.Second
)

// confirmFill waits for a submitted day market order to disappear from the
// working-order list. Saxo's placement response carries no fill data, and the
// portfolio sync never feeds realized P&L — so the disappearance of an order
// we did not cancel (day-order expiry only happens at end of day) is taken as
// the fill, and the submitted amount is reported filled with Terminal: true —
// this is the only signal Saxo gives us, so unlike Trading 212 and Alpaca
// there is no separate "unconfirmed" outcome to fall back to here. An order
// still working at the deadline, or one whose check errored, is returned
// unchanged with Terminal left false — its honest zero fill is not confirmed.
//
// The first check waits one poll interval rather than firing immediately
// after the POST returns: /port/v1/orders/me is a separate portfolio-service
// read of an order the trade service has just accepted, and checking before
// it has had a chance to propagate risks reading "not yet listed" as "already
// filled".
func (s *Saxo) confirmFill(ctx context.Context, res OrderResult) OrderResult {
	if res.ID == "" {
		return res
	}

	deadline := time.Now().Add(saxoFillWait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return res
		case <-time.After(saxoPollInterval):
		}

		working, err := s.orderWorking(ctx, res.ID)
		if err != nil {
			return res
		}

		if !working {
			res.FilledQty = res.Qty
			res.Status = "filled"
			res.Terminal = true

			return res
		}
	}

	return res
}

// orderWorking reports whether orderID is still in the account's working-order
// set. It uses the list endpoint rather than the single-order GET so the
// expected "order gone" outcome is a normal 200 response, not a 404 pushed
// into the error log on every fill.
func (s *Saxo) orderWorking(ctx context.Context, orderID string) (bool, error) {
	q := url.Values{}
	q.Set("ClientKey", s.clientKey)
	q.Set("AccountKey", s.accountKey)

	var resp struct {
		Data []saxoOrderRef `json:"Data"`
	}

	if err := s.do(ctx, http.MethodGet, "/port/v1/orders/me?"+q.Encode(), nil, &resp); err != nil {
		return false, err
	}

	for _, d := range resp.Data {
		if d.OrderID == orderID {
			return true, nil
		}
	}

	return false, nil
}

// GetAccount returns the account's balances from /port/v1/balances, converted
// to preferCcy if it differs from the account's own base currency.
func (s *Saxo) GetAccount(ctx context.Context) (Account, error) {
	q := url.Values{}
	q.Set("ClientKey", s.clientKey)
	q.Set("AccountKey", s.accountKey)

	var resp struct {
		TotalValue      float64 `json:"TotalValue"`
		CashBalance     float64 `json:"CashBalance"`
		MarginAvailable float64 `json:"MarginAvailableForTrading"`
		Currency        string  `json:"Currency"`
	}

	if err := s.do(ctx, http.MethodGet, "/port/v1/balances?"+q.Encode(), nil, &resp); err != nil {
		return Account{}, err
	}

	bp := resp.MarginAvailable
	if bp == 0 {
		bp = resp.CashBalance
	}

	cash, equity, ccy := resp.CashBalance, resp.TotalValue, resp.Currency

	if s.preferCcy != "" && ccy != "" && ccy != s.preferCcy {
		if rate, err := s.fx.Rate(ctx, ccy, s.preferCcy); err != nil {
			recordFXFailure("saxo", err, "balances shown at 1:1 and will be wrong until the rate resolves")
		} else {
			cash, equity, bp = cash*rate, equity*rate, bp*rate
			ccy = s.preferCcy
		}
	}

	return Account{Cash: cash, Equity: equity, BuyingPower: bp, Currency: ccy}, nil
}

// saxoChartBar is one OHLCV sample from the chart endpoint.
type saxoChartBar struct {
	Time   time.Time `json:"Time"`
	Open   float64   `json:"Open"`
	High   float64   `json:"High"`
	Low    float64   `json:"Low"`
	Close  float64   `json:"Close"`
	Volume float64   `json:"Volume"`
}

// Candles fetches historical OHLC bars from Saxo's chart endpoint, letting the
// engine warm up indicators immediately (and regardless of market hours). It
// satisfies the engine's history-source interface. Time arguments bound the
// requested window; Saxo returns up to 1200 samples ending at `to`.
func (s *Saxo) Candles(
	ctx context.Context,
	symbol string,
	res market.Resolution,
	from, to time.Time,
) ([]market.Candle, error) {
	inst, err := s.resolveInstrument(ctx, symbol)
	if err != nil {
		return nil, err
	}

	horizon := horizonMinutes(res)
	count := int(to.Sub(from) / (time.Duration(horizon) * time.Minute))

	if count <= 0 {
		count = 200
	}

	if count > 1200 {
		count = 1200
	}

	// Without Mode/Time the chart endpoint returns the most recent `Count`
	// bars — exactly what indicator warmup needs, and available off-hours.
	q := url.Values{}
	q.Set("AssetType", inst.AssetType)
	q.Set("Uic", strconv.Itoa(inst.Uic))
	q.Set("Horizon", strconv.Itoa(horizon))
	q.Set("Count", strconv.Itoa(count))

	var resp struct {
		Data []saxoChartBar `json:"Data"`
	}

	if err := s.do(ctx, http.MethodGet, "/chart/v3/charts?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}

	out := make([]market.Candle, 0, len(resp.Data))
	for _, d := range resp.Data {
		out = append(out, market.Candle{
			Symbol: symbol, Time: d.Time,
			Open: d.Open, High: d.High, Low: d.Low, Close: d.Close, Volume: d.Volume,
		})
	}

	return out, nil
}

// horizonMinutes maps a market resolution to a Saxo chart horizon (minutes).
func horizonMinutes(res market.Resolution) int {
	switch res {
	case market.Res5Min:
		return 5
	case market.Res15Min:
		return 15
	case market.Res1Hour:
		return 60
	case market.Res1Day:
		return 1440
	case market.Res1Min: // the shared fallback below
	}

	return 1
}

// saxoNetPositionBase holds a net position's instrument and size.
type saxoNetPositionBase struct {
	Uic    int     `json:"Uic"`
	Amount float64 `json:"Amount"`
}

// saxoNetPositionView holds a net position's marked-to-market valuation.
type saxoNetPositionView struct {
	AverageOpenPrice  float64 `json:"AverageOpenPrice"`
	CurrentPrice      float64 `json:"CurrentPrice"`
	MarketValue       float64 `json:"MarketValue"`
	ProfitLossOnTrade float64 `json:"ProfitLossOnTrade"`
}

// saxoNetPosition is one entry of the /port/v1/netpositions response.
type saxoNetPosition struct {
	NetPositionBase  saxoNetPositionBase `json:"NetPositionBase"`
	NetPositionView  saxoNetPositionView `json:"NetPositionView"`
	DisplayAndFormat saxoDisplayFormat   `json:"DisplayAndFormat"`
}

// GetPositions returns the account's net positions mapped to plain symbols.
// AverageOpenPrice/CurrentPrice/MarketValue/ProfitLossOnTrade are reported by
// Saxo in the instrument's own listing currency (e.g. USD for a US stock),
// not the account's base currency; when preferCcy is set they're converted
// into it, matching Trading 212's adapter.
func (s *Saxo) GetPositions(ctx context.Context) ([]Position, error) {
	q := url.Values{}
	q.Set("ClientKey", s.clientKey)
	q.Set("AccountKey", s.accountKey)
	q.Set("FieldGroups", "DisplayAndFormat,NetPositionView")

	var resp struct {
		Data []saxoNetPosition `json:"Data"`
	}

	if err := s.do(
		ctx,
		http.MethodGet,
		"/port/v1/netpositions?"+q.Encode(),
		nil,
		&resp,
	); err != nil {
		return nil, err
	}

	out := make([]Position, 0, len(resp.Data))
	for _, d := range resp.Data {
		sym := s.symbolForUic(d.NetPositionBase.Uic, strings.Split(d.DisplayAndFormat.Symbol, ":")[0])
		ccy := strings.ToUpper(d.DisplayAndFormat.Currency)
		rate, resolved := s.fxRateForCurrencyResolved(ctx, ccy)

		// A failed fetch still uses rate (1, a stopgap) to convert THIS
		// position's prices, but must not claim FXRate as fact — see
		// fxRateForCurrencyResolved's doc comment.
		reportedRate := rate
		if !resolved {
			reportedRate = 0
		}

		out = append(out, Position{
			Symbol:       sym,
			Qty:          d.NetPositionBase.Amount,
			AvgPrice:     d.NetPositionView.AverageOpenPrice * rate,
			Current:      d.NetPositionView.CurrentPrice * rate,
			MarketValue:  d.NetPositionView.MarketValue * rate,
			UnrealizedPL: d.NetPositionView.ProfitLossOnTrade * rate,
			Currency:     ccy,
			FXRate:       reportedRate,
		})
	}

	return out, nil
}
