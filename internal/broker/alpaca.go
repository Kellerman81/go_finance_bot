package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
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

func (a *Alpaca) Name() string { return "alpaca" }

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
		return fmt.Errorf("alpaca %s %s: status %d: %s", method, path, resp.StatusCode, string(data))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("alpaca decode %s: %w", path, err)
		}
	}
	return nil
}

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
	return raw.toResult(), nil
}

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

func parseFloat(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}
