package broker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/errlog"
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
//

func buildBracketBody(
	accountKey string,
	inst instrument,
	b BracketOrder,
	amount float64,
) map[string]any {
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
//
//nolint:cyclop // validation + leg mapping form one order flow
func (s *Saxo) PlaceBracket(ctx context.Context, b BracketOrder) (BracketResult, error) {
	if b.Side != Buy && b.Side != Sell {
		return BracketResult{}, fmt.Errorf("saxo: invalid side %q", b.Side)
	}

	if b.Qty <= 0 || math.IsNaN(b.Qty) || math.IsInf(b.Qty, 0) {
		return BracketResult{}, fmt.Errorf("saxo: invalid quantity %v", b.Qty)
	}

	if b.EntryType == Limit && b.EntryPrice <= 0 {
		return BracketResult{}, errors.New("saxo: limit entry requires a positive entry price")
	}

	// Validate protective legs are on a sane side of the entry.
	if b.Side == Buy {
		if b.StopLoss > 0 && b.TakeProfit > 0 && b.StopLoss >= b.TakeProfit {
			return BracketResult{}, errors.New("saxo: long bracket needs stop_loss < take_profit")
		}
	} else {
		if b.StopLoss > 0 && b.TakeProfit > 0 && b.StopLoss <= b.TakeProfit {
			return BracketResult{}, errors.New("saxo: short bracket needs stop_loss > take_profit")
		}
	}

	inst, err := s.resolveInstrument(ctx, b.Symbol)
	if err != nil {
		return BracketResult{}, err
	}

	amount := floorUnits(b.Qty) // BR3-6, see SubmitOrder
	if amount < 1 {
		return BracketResult{}, fmt.Errorf(
			"saxo: bracket for %s floors to 0 whole shares (qty %.4f)",
			b.Symbol,
			b.Qty,
		)
	}

	if err := s.pace(ctx); err != nil {
		return BracketResult{}, err
	}

	// b's prices arrive in preferCcy (like everything else this adapter
	// reports); Saxo expects OrderPrice in the instrument's own currency.
	//
	// The old "rate != 0 && rate != 1" guard treated a FAILED fetch (which
	// falls back to rate=1) identically to a genuine same-currency 1:1 —
	// silently skipping conversion and sending entry/stop/take-profit prices
	// unconverted. For a bracket this is the worst version of the bug: the
	// stop-loss and take-profit prices are wrong too, and the long/short
	// sanity checks above run before this conversion so they can't catch it.
	// Refuse the whole bracket instead. See CODE_REVIEW_FINDINGS.md's
	// Alpaca/Saxo order-pricing FX finding.
	rate, resolved := s.fxRateForCurrencyResolved(ctx, inst.Currency)
	if !resolved {
		return BracketResult{}, fmt.Errorf(
			"saxo: cannot price bracket for %s: fx rate %s->%s unavailable",
			b.Symbol, inst.Currency, s.preferCcy)
	}

	if rate != 1 {
		if b.EntryType == Limit {
			b.EntryPrice /= rate
		}

		if b.StopLoss > 0 {
			b.StopLoss /= rate
		}

		if b.TakeProfit > 0 {
			b.TakeProfit /= rate
		}
	}

	b = s.bracketOnTickGrid(ctx, inst, b)

	body := buildBracketBody(s.accountKey, inst, b, amount)

	var resp saxoPlaceResponse

	if err := s.do(ctx, http.MethodPost, "/trade/v2/orders", body, &resp); err != nil {
		return BracketResult{}, err
	}

	res := BracketResult{EntryID: resp.OrderID}

	res.Orders = append(res.Orders, OrderResult{
		ID: resp.OrderID, Symbol: b.Symbol, Side: b.Side,
		Qty: amount, Status: "submitted", SubmittedAt: time.Now(),
	})
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
		res.Orders = append(res.Orders, OrderResult{
			ID: o.OrderID, Symbol: b.Symbol,
			Side: opposite(b.Side), Qty: amount, Status: "submitted", SubmittedAt: time.Now(),
		})
	}

	return res, nil
}

// buildModifyBody constructs the PATCH /trade/v2/orders payload. Pure for tests.
//
// workingType is the order's current Saxo OpenOrderType. A type this package
// cannot express (a stop leg, a stop-limit, a trailing stop) is kept as it is:
// OrderModification only knows Market and Limit, and re-sending a bracket's
// stop as "Limit" turned a sell stop below the market into a sell limit below
// it, which fills at once (BR2-2).
func buildModifyBody(
	accountKey string,
	inst instrument,
	m OrderModification,
	amount float64,
	workingType, workingDuration string,
) map[string]any {
	orderType := "Limit"
	switch m.Type {
	case Market:
		orderType = "Market"
	case Limit:
		orderType = "Limit"
	}

	if workingType != "" && workingType != "Limit" && workingType != "Market" {
		orderType = workingType
	}

	body := map[string]any{
		"OrderId":       m.OrderID,
		"AccountKey":    accountKey,
		"Uic":           inst.Uic,
		"AssetType":     inst.AssetType,
		"Amount":        amount,
		"OrderType":     orderType,
		"OrderDuration": map[string]any{"DurationType": modifyDuration(workingDuration)},
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
		return OrderResult{}, errors.New("saxo: modify requires order id")
	}

	inst, err := s.resolveInstrument(ctx, m.Symbol)
	if err != nil {
		return OrderResult{}, err
	}

	amount := floorUnits(m.Qty) // BR3-6, see SubmitOrder
	if amount < 1 {
		return OrderResult{}, fmt.Errorf("saxo: modify amount floors to 0 shares (qty %.4f)", m.Qty)
	}

	// The new price is in the preferred currency, like every other price the
	// engine hands this adapter; SubmitOrder and PlaceBracket convert it to
	// the instrument's own currency or refuse. ModifyOrder sent it as-is, so
	// a €150 limit went out as 150 USD (BR2-2).
	if m.Price > 0 {
		rate, resolved := s.fxRateForCurrencyResolved(ctx, inst.Currency)
		if !resolved {
			return OrderResult{}, fmt.Errorf(
				"saxo: cannot price the modification of %s: fx rate %s->%s unavailable",
				m.OrderID,
				inst.Currency,
				s.preferCcy,
			)
		}

		m.Price = s.roundToTick(ctx, inst, m.Price/rate)
	}

	working, err := s.workingOrder(ctx, m.OrderID)
	if err != nil {
		return OrderResult{}, err
	}

	if err := s.pace(ctx); err != nil {
		return OrderResult{}, err
	}

	body := buildModifyBody(s.accountKey, inst, m, amount, working.Type, working.Duration)

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

	return OrderResult{
		ID: id, Symbol: m.Symbol, Side: m.Side, Qty: amount,
		Status: "replaced", SubmittedAt: time.Now(),
	}, nil
}

// CancelOrders cancels one or more working orders by id.
func (s *Saxo) CancelOrders(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return errors.New("saxo: no order ids to cancel")
	}

	q := url.Values{}
	q.Set("AccountKey", s.accountKey)

	path := "/trade/v2/orders/" + strings.Join(ids, ",") + "?" + q.Encode()

	return s.do(ctx, http.MethodDelete, path, nil, nil)
}

// saxoOrderRef is an order id reference in an order-placement response.
type saxoOrderRef struct {
	OrderID string `json:"OrderId"`
}

// saxoPlaceResponse is the /trade/v2/orders response: the entry order id plus
// the ids of the related (bracket) orders.
type saxoPlaceResponse struct {
	OrderID string         `json:"OrderId"`
	Orders  []saxoOrderRef `json:"Orders"`
}

// saxoDuration wraps an order's duration type.
type saxoDuration struct {
	DurationType string `json:"DurationType"`
}

// saxoDisplayFormat carries display metadata (exchange-qualified symbol and
// the instrument's own trading currency).
type saxoDisplayFormat struct {
	Symbol   string `json:"Symbol"`
	Currency string `json:"Currency"`
}

// saxoWorkingOrder is one entry of the working-orders list.
type saxoWorkingOrder struct {
	OrderID          string            `json:"OrderId"`
	Uic              int               `json:"Uic"`
	BuySell          string            `json:"BuySell"`
	Amount           float64           `json:"Amount"`
	Price            float64           `json:"Price"`
	OpenOrderType    string            `json:"OpenOrderType"`
	Status           string            `json:"Status"`
	Duration         saxoDuration      `json:"Duration"`
	DisplayAndFormat saxoDisplayFormat `json:"DisplayAndFormat"`
}

// modifyDuration is the duration a modification re-sends: the working
// order's own. It always sent GoodTillCancel, so a modify turned the bot's
// DayOrder limits into GTC orders that outlive the session — and, once their
// watcher was gone, could fill days later as an untracked position (BR3-7).
// An order whose duration the listing does not report keeps the old GTC.
func modifyDuration(working string) string {
	if working != "" {
		return working
	}

	return "GoodTillCancel"
}

// workingOrder returns a working order as OpenOrders lists it (its Saxo
// OpenOrderType and duration). An order it cannot find, or a listing that
// fails, refuses the modification: guessing "Limit" for what may be a stop
// leg is the failure buildModifyBody avoids.
func (s *Saxo) workingOrder(ctx context.Context, id string) (OpenOrder, error) {
	orders, err := s.OpenOrders(ctx)
	if err != nil {
		return OpenOrder{}, fmt.Errorf(
			"saxo: cannot confirm the type of order %s before modifying it: %w",
			id,
			err,
		)
	}

	for _, o := range orders {
		if o.OrderID == id {
			return o, nil
		}
	}

	return OpenOrder{}, fmt.Errorf("saxo: order %s is not working; nothing to modify", id)
}

// OpenOrders lists the account's working (unfilled) orders.
func (s *Saxo) OpenOrders(ctx context.Context) ([]OpenOrder, error) {
	q := url.Values{}
	q.Set("ClientKey", s.clientKey)
	q.Set("AccountKey", s.accountKey)
	q.Set("FieldGroups", "DisplayAndFormat")

	var resp struct {
		Data []saxoWorkingOrder `json:"Data"`
	}

	if err := s.do(ctx, http.MethodGet, "/port/v1/orders/me?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}

	out := make([]OpenOrder, 0, len(resp.Data))
	for _, d := range resp.Data {
		price, ccy := s.reportingPrice(ctx, d.Price, d.DisplayAndFormat.Currency)

		out = append(out, OpenOrder{
			OrderID:  d.OrderID,
			Symbol:   s.symbolForUic(d.Uic, strings.Split(d.DisplayAndFormat.Symbol, ":")[0]),
			Side:     Side(strings.ToLower(d.BuySell)),
			Type:     d.OpenOrderType,
			Qty:      d.Amount,
			Price:    price,
			Currency: ccy,
			Status:   d.Status,
			Duration: d.Duration.DurationType,
		})
	}

	return out, nil
}

// reportingPrice converts a working order's price from the instrument's
// currency into preferCcy, the currency every other price this adapter
// reports — and ModifyOrder expects — is in. Shown raw, a USD order read
// 163.00 in a EUR book; the operator typed 160 meaning USD, ModifyOrder took
// it as EUR and sent 160 / 0.92 = 173.9 USD, a buy limit 7% above the
// intended level (BR3-8). ccy is set only when the price could NOT be
// converted, so the panel can say which currency it is in.
func (s *Saxo) reportingPrice(ctx context.Context, price float64, native string) (float64, string) {
	native = strings.ToUpper(strings.TrimSpace(native))
	if price == 0 || native == "" || s.preferCcy == "" || native == s.preferCcy {
		return price, ""
	}

	rate, resolved := s.fxRateForCurrencyResolved(ctx, native)
	if !resolved {
		return price, native
	}

	return price * rate, ""
}

// saxoTickScheme is an instrument's price grid: one tick size, or a ladder of
// (price ceiling, tick) steps.
type saxoTickScheme struct {
	TickSize       float64 `json:"TickSize"`
	TickSizeScheme struct {
		DefaultTickSize float64 `json:"DefaultTickSize"`
		Elements        []struct {
			HighPrice float64 `json:"HighPrice"`
			TickSize  float64 `json:"TickSize"`
		} `json:"Elements"`
	} `json:"TickSizeScheme"`
}

// tickAt is the tick size that applies at price, 0 when none is known.
func (t saxoTickScheme) tickAt(price float64) float64 {
	for _, e := range t.TickSizeScheme.Elements {
		if price <= e.HighPrice && e.TickSize > 0 {
			return e.TickSize
		}
	}

	if t.TickSizeScheme.DefaultTickSize > 0 {
		return t.TickSizeScheme.DefaultTickSize
	}

	return t.TickSize
}

// roundToTick puts price on the instrument's tick grid, to the nearest tick.
// Saxo refuses an order price off the grid (PriceNotInTickSizeIncrements),
// and every price the engine sends is off it: a passive limit is
// price × (1 ± offset), then divided by an FX rate — 163.04347826 for a €150
// limit on a USD line. Limit execution, which only Saxo actually rests, had
// every clip refused, each evaluation (BR3-5). When the instrument details
// cannot be read the price goes as it is, and that is said once.
func (s *Saxo) roundToTick(ctx context.Context, inst instrument, price float64) float64 {
	tick := s.tickScheme(ctx, inst).tickAt(price)
	if tick <= 0 || price <= 0 {
		return price
	}

	return roundToStep(price, tick)
}

// roundToStep rounds v to the nearest multiple of step, with the result
// rendered at step's own decimals so no float tail (163.04000000000002)
// leaves it off the grid again.
func roundToStep(v, step float64) float64 {
	n := math.Round(v / step)
	decimals := 0

	if _, frac, ok := strings.Cut(strconv.FormatFloat(step, 'f', -1, 64), "."); ok {
		decimals = len(frac)
	}

	out, err := strconv.ParseFloat(strconv.FormatFloat(n*step, 'f', decimals, 64), 64)
	if err != nil {
		return n * step
	}

	return out
}

// tickScheme returns inst's tick scheme, fetched once and cached.
func (s *Saxo) tickScheme(ctx context.Context, inst instrument) saxoTickScheme {
	s.mu.Lock()
	t, ok := s.ticks[inst.Uic]
	s.mu.Unlock()

	if ok {
		return t
	}

	path := fmt.Sprintf("/ref/v1/instruments/details/%d/%s", inst.Uic, url.PathEscape(inst.AssetType))
	if err := s.do(ctx, http.MethodGet, path, nil, &t); err != nil {
		errlog.RecordOncef("saxo", "instrument %d: tick size unavailable (%v); order prices are sent "+
			"unrounded and Saxo may refuse them", inst.Uic, err)

		return saxoTickScheme{}
	}

	s.mu.Lock()
	if s.ticks == nil {
		s.ticks = make(map[int]saxoTickScheme)
	}

	s.ticks[inst.Uic] = t
	s.mu.Unlock()

	return t
}

// bracketOnTickGrid puts a bracket's entry, stop and take-profit prices on the
// instrument's tick grid, after conversion — see roundToTick (BR3-5).
func (s *Saxo) bracketOnTickGrid(ctx context.Context, inst instrument, b BracketOrder) BracketOrder {
	if b.EntryType == Limit {
		b.EntryPrice = s.roundToTick(ctx, inst, b.EntryPrice)
	}

	if b.StopLoss > 0 {
		b.StopLoss = s.roundToTick(ctx, inst, b.StopLoss)
	}

	if b.TakeProfit > 0 {
		b.TakeProfit = s.roundToTick(ctx, inst, b.TakeProfit)
	}

	return b
}
