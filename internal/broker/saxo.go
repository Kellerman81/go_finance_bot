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

	mu  sync.Mutex
	uic map[string]instrument // symbol -> resolved instrument (cached)

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
}

// NewSaxo constructs a Saxo broker and resolves the account/client keys. If
// accountKey is empty, the first account from /port/v1/accounts/me is used. The
// TokenSource supplies a (possibly auto-refreshing) bearer token per request.
func NewSaxo(baseURL string, tokens TokenSource, accountKey, assetTypes string) (*Saxo, error) {
	if assetTypes == "" {
		assetTypes = "Stock,Etf"
	}

	s := &Saxo{
		baseURL:    strings.TrimRight(baseURL, "/"),
		tokens:     tokens,
		assetTypes: assetTypes,
		http:       &http.Client{Timeout: 20 * time.Second},
		accountKey: accountKey,
		uic:        make(map[string]instrument),
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

	return &Saxo{
		baseURL:    strings.TrimRight(baseURL, "/"),
		tokens:     tokens,
		assetTypes: assetTypes,
		http:       &http.Client{Timeout: 20 * time.Second},
		uic:        make(map[string]instrument),
	}
}

// Name identifies the broker adapter.
func (*Saxo) Name() string { return "saxo" }

// do performs an authenticated Saxo OpenAPI call, recording failures in the
// error log and decoding the JSON response into out.
//
//nolint:cyclop // linear request/response handling; splitting hurts readability
func (s *Saxo) do(ctx context.Context, method, path string, body, out any) (err error) {
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
		return err
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
		return err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf(
			"saxo %s %s: 401 unauthorized — token expired/invalid, or token/gateway environment mismatch "+
				"(SIM tokens only work on .../sim/openapi, live tokens only on .../openapi); base_url=%s",
			method,
			path,
			s.baseURL,
		)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("saxo %s %s: status %d: %s", method, path, resp.StatusCode, string(data))
	}

	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("saxo decode %s: %w", path, err)
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
	Identifier  int    `json:"Identifier"`
	AssetType   string `json:"AssetType"`
	Symbol      string `json:"Symbol"`
	Description string `json:"Description"`
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

	// Prefer an exact symbol match; otherwise take the first result.
	pick := resp.Data[0]
	for _, d := range resp.Data {
		if strings.EqualFold(strings.Split(d.Symbol, ":")[0], symbol) {
			pick = d
			break
		}
	}

	inst := instrument{Uic: pick.Identifier, AssetType: pick.AssetType}

	s.mu.Lock()

	s.uic[symbol] = inst
	s.mu.Unlock()

	return inst, nil
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

	amount := math.Floor(o.Qty)
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
		body["OrderPrice"] = o.LimitPx
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
// the fill, and the submitted amount is reported filled. The fill price is
// left 0; callers fall back to their confirmed reference price. An order still
// working at the deadline is returned unchanged with its honest zero fill.
func (s *Saxo) confirmFill(ctx context.Context, res OrderResult) OrderResult {
	if res.ID == "" {
		return res
	}

	deadline := time.Now().Add(saxoFillWait)
	for time.Now().Before(deadline) {
		working, err := s.orderWorking(ctx, res.ID)
		if err != nil {
			return res
		}

		if !working {
			res.FilledQty = res.Qty
			res.Status = "filled"
			return res
		}

		select {
		case <-ctx.Done():
			return res
		case <-time.After(saxoPollInterval):
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

// GetAccount returns the account's balances from /port/v1/balances.
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

	return Account{
		Cash:        resp.CashBalance,
		Equity:      resp.TotalValue,
		BuyingPower: bp,
		Currency:    resp.Currency,
	}, nil
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
		sym := strings.Split(d.DisplayAndFormat.Symbol, ":")[0]

		out = append(out, Position{
			Symbol:       sym,
			Qty:          d.NetPositionBase.Amount,
			AvgPrice:     d.NetPositionView.AverageOpenPrice,
			Current:      d.NetPositionView.CurrentPrice,
			MarketValue:  d.NetPositionView.MarketValue,
			UnrealizedPL: d.NetPositionView.ProfitLossOnTrade,
		})
	}

	return out, nil
}
