package broker

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// opposite returns the protective-leg side for a given entry side.
func opposite(s Side) Side {
	if s == Buy {
		return Sell
	}
	return Buy
}

// saxoSide maps our lowercase Side to Saxo's capitalised "Buy"/"Sell".
func saxoSide(s Side) string {
	if s == Sell {
		return "Sell"
	}
	return "Buy"
}

// buildBracketBody constructs the /trade/v2/orders payload for an entry order
// with nested related stop-loss and take-profit legs. It is pure (no I/O) so
// the payload shape can be unit-tested. amount is the whole-share entry size.
func buildBracketBody(accountKey string, inst instrument, b BracketOrder, amount float64) map[string]any {
	duration := b.Duration
	if duration == "" {
		duration = "GoodTillCancel"
	}
	entryType := "Market"
	if b.EntryType == Limit {
		entryType = "Limit"
	}

	body := map[string]any{
		"AccountKey":    accountKey,
		"Uic":           inst.Uic,
		"AssetType":     inst.AssetType,
		"Amount":        amount,
		"BuySell":       saxoSide(b.Side),
		"OrderType":     entryType,
		"ManualOrder":   true,
		"OrderDuration": map[string]any{"DurationType": duration},
	}
	if b.EntryType == Limit {
		body["OrderPrice"] = b.EntryPrice
	}

	legSide := saxoSide(opposite(b.Side))
	var legs []map[string]any
	if b.StopLoss > 0 {
		legs = append(legs, map[string]any{
			"AccountKey":    accountKey,
			"Uic":           inst.Uic,
			"AssetType":     inst.AssetType,
			"Amount":        amount,
			"BuySell":       legSide,
			"OrderType":     "StopIfTraded",
			"OrderPrice":    b.StopLoss,
			"ManualOrder":   true,
			"OrderDuration": map[string]any{"DurationType": "GoodTillCancel"},
		})
	}
	if b.TakeProfit > 0 {
		legs = append(legs, map[string]any{
			"AccountKey":    accountKey,
			"Uic":           inst.Uic,
			"AssetType":     inst.AssetType,
			"Amount":        amount,
			"BuySell":       legSide,
			"OrderType":     "Limit",
			"OrderPrice":    b.TakeProfit,
			"ManualOrder":   true,
			"OrderDuration": map[string]any{"DurationType": "GoodTillCancel"},
		})
	}
	if len(legs) > 0 {
		body["Orders"] = legs
	}
	return body
}

// PlaceBracket submits an entry order with attached protective legs.
func (s *Saxo) PlaceBracket(ctx context.Context, b BracketOrder) (BracketResult, error) {
	if b.Side != Buy && b.Side != Sell {
		return BracketResult{}, fmt.Errorf("saxo: invalid side %q", b.Side)
	}
	if b.Qty <= 0 || math.IsNaN(b.Qty) || math.IsInf(b.Qty, 0) {
		return BracketResult{}, fmt.Errorf("saxo: invalid quantity %v", b.Qty)
	}
	if b.EntryType == Limit && b.EntryPrice <= 0 {
		return BracketResult{}, fmt.Errorf("saxo: limit entry requires a positive entry price")
	}
	// Validate protective legs are on a sane side of the entry.
	if b.Side == Buy {
		if b.StopLoss > 0 && b.TakeProfit > 0 && b.StopLoss >= b.TakeProfit {
			return BracketResult{}, fmt.Errorf("saxo: long bracket needs stop_loss < take_profit")
		}
	} else {
		if b.StopLoss > 0 && b.TakeProfit > 0 && b.StopLoss <= b.TakeProfit {
			return BracketResult{}, fmt.Errorf("saxo: short bracket needs stop_loss > take_profit")
		}
	}

	inst, err := s.resolveInstrument(ctx, b.Symbol)
	if err != nil {
		return BracketResult{}, err
	}
	amount := math.Floor(b.Qty)
	if amount < 1 {
		return BracketResult{}, fmt.Errorf("saxo: bracket for %s floors to 0 whole shares (qty %.4f)", b.Symbol, b.Qty)
	}

	if err := s.pace(ctx); err != nil {
		return BracketResult{}, err
	}
	body := buildBracketBody(s.accountKey, inst, b, amount)

	var resp struct {
		OrderID string `json:"OrderId"`
		Orders  []struct {
			OrderID string `json:"OrderId"`
		} `json:"Orders"`
	}
	if err := s.do(ctx, http.MethodPost, "/trade/v2/orders", body, &resp); err != nil {
		return BracketResult{}, err
	}

	res := BracketResult{EntryID: resp.OrderID}
	res.Orders = append(res.Orders, OrderResult{ID: resp.OrderID, Symbol: b.Symbol, Side: b.Side,
		Qty: amount, Status: "submitted", SubmittedAt: time.Now()})
	// Map related-order ids by declared order (stop first if present, then TP).
	idx := 0
	if b.StopLoss > 0 && idx < len(resp.Orders) {
		res.StopID = resp.Orders[idx].OrderID
		idx++
	}
	if b.TakeProfit > 0 && idx < len(resp.Orders) {
		res.TakeProfitID = resp.Orders[idx].OrderID
	}
	for _, o := range resp.Orders {
		res.Orders = append(res.Orders, OrderResult{ID: o.OrderID, Symbol: b.Symbol,
			Side: opposite(b.Side), Qty: amount, Status: "submitted", SubmittedAt: time.Now()})
	}
	return res, nil
}

// buildModifyBody constructs the PATCH /trade/v2/orders payload. Pure for tests.
func buildModifyBody(accountKey string, inst instrument, m OrderModification, amount float64) map[string]any {
	orderType := "Limit"
	switch m.Type {
	case Market:
		orderType = "Market"
	case Limit:
		orderType = "Limit"
	}
	body := map[string]any{
		"OrderId":       m.OrderID,
		"AccountKey":    accountKey,
		"Uic":           inst.Uic,
		"AssetType":     inst.AssetType,
		"Amount":        amount,
		"OrderType":     orderType,
		"OrderDuration": map[string]any{"DurationType": "GoodTillCancel"},
	}
	if m.Side == Buy || m.Side == Sell {
		body["BuySell"] = saxoSide(m.Side)
	}
	if m.Price > 0 {
		body["OrderPrice"] = m.Price
	}
	return body
}

// ModifyOrder changes a working order's price and/or amount.
func (s *Saxo) ModifyOrder(ctx context.Context, m OrderModification) (OrderResult, error) {
	if m.OrderID == "" {
		return OrderResult{}, fmt.Errorf("saxo: modify requires order id")
	}
	inst, err := s.resolveInstrument(ctx, m.Symbol)
	if err != nil {
		return OrderResult{}, err
	}
	amount := math.Floor(m.Qty)
	if amount < 1 {
		return OrderResult{}, fmt.Errorf("saxo: modify amount floors to 0 shares (qty %.4f)", m.Qty)
	}
	if err := s.pace(ctx); err != nil {
		return OrderResult{}, err
	}
	body := buildModifyBody(s.accountKey, inst, m, amount)

	var resp struct {
		OrderID string `json:"OrderId"`
	}
	if err := s.do(ctx, http.MethodPatch, "/trade/v2/orders", body, &resp); err != nil {
		return OrderResult{}, err
	}
	id := resp.OrderID
	if id == "" {
		id = m.OrderID
	}
	return OrderResult{ID: id, Symbol: m.Symbol, Side: m.Side, Qty: amount,
		Status: "replaced", SubmittedAt: time.Now()}, nil
}

// CancelOrders cancels one or more working orders by id.
func (s *Saxo) CancelOrders(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return fmt.Errorf("saxo: no order ids to cancel")
	}
	q := url.Values{}
	q.Set("AccountKey", s.accountKey)
	path := "/trade/v2/orders/" + strings.Join(ids, ",") + "?" + q.Encode()
	return s.do(ctx, http.MethodDelete, path, nil, nil)
}

// OpenOrders lists the account's working (unfilled) orders.
func (s *Saxo) OpenOrders(ctx context.Context) ([]OpenOrder, error) {
	q := url.Values{}
	q.Set("ClientKey", s.clientKey)
	q.Set("AccountKey", s.accountKey)
	q.Set("FieldGroups", "DisplayAndFormat")
	var resp struct {
		Data []struct {
			OrderID       string  `json:"OrderId"`
			Uic           int     `json:"Uic"`
			BuySell       string  `json:"BuySell"`
			Amount        float64 `json:"Amount"`
			Price         float64 `json:"Price"`
			OpenOrderType string  `json:"OpenOrderType"`
			Status        string  `json:"Status"`
			Duration      struct {
				DurationType string `json:"DurationType"`
			} `json:"Duration"`
			DisplayAndFormat struct {
				Symbol string `json:"Symbol"`
			} `json:"DisplayAndFormat"`
		} `json:"Data"`
	}
	if err := s.do(ctx, http.MethodGet, "/port/v1/orders/me?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	out := make([]OpenOrder, 0, len(resp.Data))
	for _, d := range resp.Data {
		out = append(out, OpenOrder{
			OrderID:  d.OrderID,
			Symbol:   strings.Split(d.DisplayAndFormat.Symbol, ":")[0],
			Side:     Side(strings.ToLower(d.BuySell)),
			Type:     d.OpenOrderType,
			Qty:      d.Amount,
			Price:    d.Price,
			Status:   d.Status,
			Duration: d.Duration.DurationType,
		})
	}
	return out, nil
}
