package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/errlog"
)

// Alpaca is a Broker backed by the Alpaca REST trading API. Point BaseURL at
// https://paper-api.alpaca.markets for paper trading (recommended) or the live
// endpoint once a strategy is proven.
type Alpaca struct {
	baseURL string
	keyID   string
	secret  string
	http    *http.Client
}

// NewAlpaca constructs an Alpaca broker client.
func NewAlpaca(baseURL, keyID, secret string) *Alpaca {
	return &Alpaca{
		baseURL: baseURL,
		keyID:   keyID,
		secret:  secret,
		http:    &http.Client{Timeout: 15 * time.Second},
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

// SubmitOrder places a day order and, for market orders, waits for the
// asynchronous fill so the result carries real fill data.
func (a *Alpaca) SubmitOrder(ctx context.Context, o Order) (OrderResult, error) {
	reqBody := map[string]any{
		"symbol":        o.Symbol,
		"side":          string(o.Side),
		"type":          string(o.Type),
		"time_in_force": "day",
	}
	if o.Qty > 0 {
		reqBody["qty"] = strconv.FormatFloat(o.Qty, 'f', -1, 64)
	} else if o.Notional > 0 {
		reqBody["notional"] = strconv.FormatFloat(o.Notional, 'f', 2, 64)
	}

	if o.Type == Limit {
		reqBody["limit_price"] = strconv.FormatFloat(o.LimitPx, 'f', 2, 64)
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

	return res, nil
}

// alpacaFillWait / alpacaPollInterval bound how long confirmFill waits for an
// asynchronous fill before returning the best-known state.
const (
	alpacaFillWait     = 25 * time.Second
	alpacaPollInterval = time.Second
)

// alpacaWorkingStatus reports whether an order status means "still heading for
// a fill" (keep polling). Anything unrecognised is treated as terminal.
func alpacaWorkingStatus(s string) bool {
	switch strings.ToLower(s) {
	case "",
		"new",
		"accepted",
		"pending_new",
		"accepted_for_bidding",
		"partially_filled",
		"calculated":
		return true
	}

	return false
}

// confirmFill polls GET /v2/orders/{id} until the order reaches a terminal
// state ("filled", "canceled", "rejected", …), so the returned OrderResult
// carries the real filled quantity and average price instead of the
// filled_qty:0 snapshot the submission response contains. On timeout, context
// cancellation or a request error the best-known state is returned unchanged.
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

// GetAccount returns the account's cash, equity and buying power.
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

	return Account{
		Cash:        parseFloat(raw.Cash),
		Equity:      parseFloat(raw.Equity),
		BuyingPower: parseFloat(raw.BuyingPower),
		Currency:    raw.Currency,
	}, nil
}

// GetPositions returns the currently held positions.
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

	out := make([]Position, len(raw))
	for i, p := range raw {
		out[i] = Position{
			Symbol:       p.Symbol,
			Qty:          parseFloat(p.Qty),
			AvgPrice:     parseFloat(p.AvgEntry),
			Current:      parseFloat(p.CurrentPrice),
			MarketValue:  parseFloat(p.MarketValue),
			UnrealizedPL: parseFloat(p.UnrealizedPL),
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
