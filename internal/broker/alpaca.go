package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/errlog"
)

// Alpaca is a Broker backed by the Alpaca REST trading API. Point BaseURL at
// https://paper-api.alpaca.markets for paper trading (recommended) or the live
// endpoint once a strategy is proven.
//
// Alpaca accounts and all their positions are denominated in a single
// currency (USD in practice) — unlike Trading 212, no per-instrument FX
// mismatch is possible. preferCcy instead lets the whole account be reported
// in a different currency (e.g. EUR) than it's actually held in.
type Alpaca struct {
	baseURL string
	keyID   string
	secret  string
	http    *http.Client

	preferCcy string
	fx        *fxConverter

	mu       sync.Mutex
	currency string // cached account currency (e.g. "USD")
}

// NewAlpaca constructs an Alpaca broker client. preferCcy, if set, converts
// reported balances/positions from the account's own currency into it
// (empty preferCcy leaves everything in the account's native currency).
func NewAlpaca(baseURL, keyID, secret, preferCcy string) *Alpaca {
	httpClient := &http.Client{Timeout: 15 * time.Second}

	return &Alpaca{
		baseURL:   baseURL,
		keyID:     keyID,
		secret:    secret,
		http:      httpClient,
		preferCcy: strings.ToUpper(preferCcy),
		fx:        newFXConverter(httpClient),
	}
}

// Name identifies the broker adapter.
func (*Alpaca) Name() string { return "alpaca" }

// do performs an authenticated Alpaca REST call, recording failures in the
// error log and decoding the JSON response into out.
//
//nolint:cyclop // linear request/response handling; splitting hurts readability
func (a *Alpaca) do(ctx context.Context, method, path string, body, out any) (err error) {
	defer func() {
		if err != nil {
			errlog.Record("alpaca", err.Error())
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

	req, err := http.NewRequestWithContext(ctx, method, a.baseURL+path, reader)
	if err != nil {
		return err
	}

	req.Header.Set("APCA-API-KEY-ID", a.keyID)
	req.Header.Set("APCA-API-SECRET-KEY", a.secret)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf(
			"alpaca %s %s: status %d: %s",
			method,
			path,
			resp.StatusCode,
			string(data),
		)
	}

	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("alpaca decode %s: %w", path, err)
		}
	}

	return nil
}

// alpacaMaxQtyDecimals is Alpaca's fractional-quantity limit: qty may carry up
// to 9 decimal places (down to 0.000000001 of a share). A computed quantity
// (order_size/price) often has more, which Alpaca rejects, so it is rounded
// down to this precision before submission.
const alpacaMaxQtyDecimals = 9

// alpacaNotionalScale is Alpaca's notional-order precision: dollar amounts
// may carry up to 4 decimal places. See the "notional" handling in
// SubmitOrder.
const alpacaNotionalScale = 1e4

// SubmitOrder places a day order and, for market orders, waits for the
// asynchronous fill so the result carries real fill data. o.LimitPx/Notional
// are expected in preferCcy (as everything else this adapter reports) and
// converted to the account's own currency for the API call; the returned
// FilledPx is converted back.
//
//nolint:cyclop // one guarded step per order type, FX case and fill outcome
func (a *Alpaca) SubmitOrder(ctx context.Context, o Order) (OrderResult, error) {
	rate := 1.0
	needsConversion := false
	accCcy := ""

	if a.preferCcy != "" {
		accCcy = a.accountCurrency(ctx)
		if accCcy != "" && accCcy != a.preferCcy {
			needsConversion = true

			if r, err := a.fx.Rate(ctx, a.preferCcy, accCcy); err != nil {
				errlog.Recordf("alpaca", "fx rate %s->%s: %s", a.preferCcy, accCcy, err)
			} else {
				rate, needsConversion = r, false
			}
		}
	}

	// A stale/unconverted rate is a reporting inaccuracy for balances (see
	// GetAccount/GetPositions), but here it directly determines what price
	// gets sent to the broker — submitting o.LimitPx unconverted when
	// conversion was actually needed silently prices a limit/stop order
	// ~preferCcy/ccy off, not merely mislabels it. Only Type == Limit (and a
	// nonzero Notional, currently unused by any caller in this repo) are
	// affected, so only refuse those, not every order. See
	// CODE_REVIEW_FINDINGS.md's Alpaca/Saxo order-pricing FX finding.
	if needsConversion && (o.Type == Limit || o.Notional > 0) {
		return OrderResult{}, fmt.Errorf(
			"alpaca: cannot price %s order for %s: fx rate %s->%s unavailable",
			o.Type, o.Symbol, a.preferCcy, accCcy)
	}

	reqBody := map[string]any{
		"symbol":        o.Symbol,
		"side":          string(o.Side),
		"type":          string(o.Type),
		"time_in_force": "day",
	}
	if o.Qty > 0 {
		// Round DOWN to Alpaca's 9-dp fractional limit (never oversell a position
		// or exceed the intended budget). Non-fractionable assets still require
		// whole shares — Alpaca rejects a fractional qty for those with a clear
		// error, which surfaces as a single failed order.
		qty := roundDownQty(o.Qty, alpacaMaxQtyDecimals)
		if qty <= 0 {
			return OrderResult{}, fmt.Errorf("alpaca: quantity %v too small to place", o.Qty)
		}

		reqBody["qty"] = strconv.FormatFloat(qty, 'f', -1, 64)
	} else if o.Notional > 0 {
		// Rounded (not truncated at a flat 2 decimals) to Alpaca's 4-decimal
		// notional precision: a flat 2 silently threw away meaningful
		// precision for a sub-$1 instrument (e.g. $0.1234 became $0.12, a
		// ~3% error) — more precision is never rejected for a higher-priced
		// one, so this applies uniformly rather than branching on price.
		// FormatFloat's -1 precision then trims any trailing zeros the
		// rounding left, rather than always padding to 4 decimals. See
		// CODE_REVIEW_FINDINGS.md's Alpaca decimal-hardcode finding.
		notional := math.Round(o.Notional*rate*alpacaNotionalScale) / alpacaNotionalScale
		reqBody["notional"] = strconv.FormatFloat(notional, 'f', -1, 64)
	}

	if o.Type == Limit {
		reqBody["limit_price"] = alpacaLimitPrice(o.LimitPx*rate, o.Side)
	}

	var raw alpacaOrder

	if err := a.do(ctx, http.MethodPost, "/v2/orders", reqBody, &raw); err != nil {
		return OrderResult{}, err
	}

	res := raw.toResult()
	// Market orders fill asynchronously — the POST response reports status
	// "accepted"/"new" with filled_qty 0. Wait for the terminal state so the
	// caller gets the real FilledQty/FilledPx (realized P&L depends on them).
	// Limit orders may legitimately stay working, so they are not waited on.
	if o.Type != Limit {
		res = a.confirmFill(ctx, res)
	}

	// A market order is still sent when the rate could not be fetched (rate
	// stays 1), so its fill price is in the account currency, not the
	// preferred one — booking it as-is mislabels it (BR2-3). Report no price;
	// the engine falls back to its own converted reference price.
	switch {
	case needsConversion:
		res.FilledPx = 0
	case res.FilledPx > 0 && rate != 0:
		res.FilledPx /= rate
	}

	return res, nil
}

// alpacaFillWait / alpacaPollInterval bound how long confirmFill waits for an
// asynchronous fill before returning the best-known state.
const (
	alpacaFillWait     = 25 * time.Second
	alpacaPollInterval = time.Second
)

// alpacaLimitPrice formats a limit price at the precision Alpaca accepts — 4
// decimals below $1, 2 at or above — rounding toward the CONSERVATIVE side: a
// buy down (never pay more than asked), a sell up (never accept less). A flat
// 2 dp sent a 0.4567 buy limit as 0.46, up to ~0.7% over the intended price,
// and a sub-$1 sell limit could round past the market and never fill (BR2-7).
func alpacaLimitPrice(px float64, side Side) string {
	dp := 2
	if px < 1 {
		dp = 4
	}

	p := math.Pow(10, float64(dp))

	// Representation-safe, like roundDownQty: 12.34 must stay 12.34 either way.
	scaled := floorUnits(px * p)
	if side == Sell {
		scaled = ceilUnits(px * p)
	}

	return strconv.FormatFloat(scaled/p, 'f', dp, 64)
}

// alpacaWorkingStatus reports whether an order status means "still heading for
// a fill" (keep polling). Only Alpaca's FINAL statuses stop the poll.
//
// The list used to be the other way round — named working states, anything
// else terminal — so pending_cancel, pending_replace, held, stopped and
// suspended read as a confirmed fill of whatever filled_qty was last seen,
// usually 0. A "stopped" market order (a fill guaranteed but not yet printed)
// was booked as nothing and its shares arrived later as an untracked holding
// (BR2-6). A status this list does not know now keeps polling and, at the
// deadline, stays unconfirmed — unknown is not zero.
func alpacaWorkingStatus(s string) bool {
	switch strings.ToLower(s) {
	case "filled", "canceled", "expired", "rejected", "replaced", "done_for_day":
		return false
	}

	return true
}

// confirmFill polls GET /v2/orders/{id} until the order reaches a terminal
// state ("filled", "canceled", "rejected", …), so the returned OrderResult
// carries the real filled quantity and average price instead of the
// filled_qty:0 snapshot the submission response contains. On timeout, context
// cancellation or a request error the best-known state is returned unchanged
// with Terminal left false — Alpaca genuinely didn't confirm an outcome, so
// the caller must not book it.
func (a *Alpaca) confirmFill(ctx context.Context, res OrderResult) OrderResult {
	if res.ID == "" {
		return res
	}

	deadline := time.Now().Add(alpacaFillWait)
	cur := res

	for time.Now().Before(deadline) {
		var raw alpacaOrder

		if err := a.do(ctx, http.MethodGet, "/v2/orders/"+res.ID, nil, &raw); err != nil {
			return cur
		}

		cur = raw.toResult()
		if !alpacaWorkingStatus(raw.Status) {
			cur.Terminal = true // a real broker-confirmed terminal status

			return cur
		}

		select {
		case <-ctx.Done():
			return cur
		case <-time.After(alpacaPollInterval):
		}
	}

	return cur
}

// GetAccount returns the account's cash, equity and buying power, converted
// to preferCcy if it differs from the account's own currency.
func (a *Alpaca) GetAccount(ctx context.Context) (Account, error) {
	var raw struct {
		Cash        string `json:"cash"`
		Equity      string `json:"equity"`
		BuyingPower string `json:"buying_power"`
		Currency    string `json:"currency"`
	}

	if err := a.do(ctx, http.MethodGet, "/v2/account", nil, &raw); err != nil {
		return Account{}, err
	}

	a.mu.Lock()
	a.currency = raw.Currency
	a.mu.Unlock()

	cash, equity, bp := parseFloat(raw.Cash), parseFloat(raw.Equity), parseFloat(raw.BuyingPower)
	ccy := raw.Currency

	if a.preferCcy != "" && ccy != "" && ccy != a.preferCcy {
		if rate, err := a.fx.Rate(ctx, ccy, a.preferCcy); err != nil {
			recordFXFailure("alpaca", err, "balances shown at 1:1 and will be wrong until the rate resolves")
		} else {
			cash, equity, bp = cash*rate, equity*rate, bp*rate
			ccy = a.preferCcy
		}
	}

	return Account{Cash: cash, Equity: equity, BuyingPower: bp, Currency: ccy}, nil
}

// accountCurrency returns the account's own currency, fetched once and
// cached (GetPositions needs it but the positions endpoint doesn't report
// one itself).
func (a *Alpaca) accountCurrency(ctx context.Context) string {
	a.mu.Lock()
	c := a.currency
	a.mu.Unlock()

	if c != "" {
		return c
	}

	var raw struct {
		Currency string `json:"currency"`
	}

	if err := a.do(ctx, http.MethodGet, "/v2/account", nil, &raw); err != nil {
		return ""
	}

	a.mu.Lock()
	a.currency = raw.Currency
	a.mu.Unlock()

	return raw.Currency
}

// currencyAndRate returns the account's own currency and the multiplier to
// convert 1 unit of it into preferCcy (1 if they're the same, unknown, or no
// preferCcy is configured — logged on an actual fetch failure since a stuck
// rate should be visible in diagnostics).
//
// resolved is false only when a genuine fetch was attempted and failed —
// callers that report a position's FXRate (as opposed to ones just pricing
// an order, where 1:1 is an acceptable stopgap) must use it to leave FXRate
// at its zero value rather than reporting a fallback 1 as if it were a real
// rate: see GetPositions and CODE_REVIEW_FINDINGS.md's FX-fallback finding.
func (a *Alpaca) currencyAndRate(ctx context.Context) (ccy string, rate float64, resolved bool) {
	ccy = a.accountCurrency(ctx)

	if a.preferCcy == "" || ccy == "" || ccy == a.preferCcy {
		return ccy, 1, true
	}

	r, err := a.fx.Rate(ctx, ccy, a.preferCcy)
	if err != nil {
		recordFXFailure("alpaca", err, "rate unresolved: orders that need it are refused, positions stay in their native currency")
		return ccy, 1, false
	}

	return ccy, r, true
}

// InstrumentCurrency implements CurrencyResolver. Alpaca accounts/positions
// are always in a single currency, so symbol is unused.
//
// Unlike currencyAndRate (used for order pricing, where falling back to 1:1
// on a transient FX failure is an acceptable degraded mode — the order still
// goes out), this propagates a fetch failure as a real error rather than
// silently returning 1. The engine's FX rate cache (see
// Engine.resolveFXRates/refreshFXRates) relies on that distinction to skip
// caching on failure instead of overwriting a previously-correct rate with
// 1 — see CODE_REVIEW_FINDINGS.md's FX-fallback finding.
func (a *Alpaca) InstrumentCurrency(ctx context.Context, _ string) (string, float64, error) {
	ccy := a.accountCurrency(ctx)

	if a.preferCcy == "" || ccy == "" || ccy == a.preferCcy {
		return ccy, 1, nil
	}

	rate, err := a.fx.Rate(ctx, ccy, a.preferCcy)
	if err != nil {
		return ccy, 1, fmt.Errorf("fx rate %s->%s: %w", ccy, a.preferCcy, err)
	}

	return ccy, rate, nil
}

// GetPositions returns the currently held positions, converted to preferCcy
// if it differs from the account's own currency.
func (a *Alpaca) GetPositions(ctx context.Context) ([]Position, error) {
	var raw []struct {
		Symbol       string `json:"symbol"`
		Qty          string `json:"qty"`
		AvgEntry     string `json:"avg_entry_price"`
		CurrentPrice string `json:"current_price"`
		MarketValue  string `json:"market_value"`
		UnrealizedPL string `json:"unrealized_pl"`
	}

	if err := a.do(ctx, http.MethodGet, "/v2/positions", nil, &raw); err != nil {
		return nil, err
	}

	ccy, rate, resolved := a.currencyAndRate(ctx)

	// A failed fetch still uses rate (1, a stopgap) to convert THIS response's
	// prices — better than reporting native-currency numbers mislabeled as
	// preferCcy — but must not claim FXRate as a fact: the engine treats any
	// nonzero FXRate as authoritative and caches it (see
	// Engine.syncPortfolio), so a fallback value there would silently
	// overwrite a previously-correct cached rate. See
	// CODE_REVIEW_FINDINGS.md's FX-fallback finding.
	reportedRate := rate
	if !resolved {
		reportedRate = 0
	}

	out := make([]Position, len(raw))
	for i, p := range raw {
		out[i] = Position{
			Symbol:       p.Symbol,
			Qty:          parseFloat(p.Qty),
			AvgPrice:     parseFloat(p.AvgEntry) * rate,
			Current:      parseFloat(p.CurrentPrice) * rate,
			MarketValue:  parseFloat(p.MarketValue) * rate,
			UnrealizedPL: parseFloat(p.UnrealizedPL) * rate,
			Currency:     ccy,
			FXRate:       reportedRate,
		}
	}

	return out, nil
}

type alpacaOrder struct {
	ID          string `json:"id"`
	Symbol      string `json:"symbol"`
	Side        string `json:"side"`
	Qty         string `json:"qty"`
	FilledQty   string `json:"filled_qty"`
	FilledPx    string `json:"filled_avg_price"`
	Status      string `json:"status"`
	SubmittedAt string `json:"submitted_at"`
}

// toResult maps an Alpaca order payload onto the neutral OrderResult.
func (o alpacaOrder) toResult() OrderResult {
	t, _ := time.Parse(time.RFC3339, o.SubmittedAt)

	return OrderResult{
		ID:          o.ID,
		Symbol:      o.Symbol,
		Side:        Side(o.Side),
		Qty:         parseFloat(o.Qty),
		FilledQty:   parseFloat(o.FilledQty),
		FilledPx:    parseFloat(o.FilledPx),
		Status:      o.Status,
		SubmittedAt: t,
	}
}

// parseFloat parses Alpaca's stringly-typed numbers, returning 0 when empty.
func parseFloat(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}
