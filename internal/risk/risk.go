// Package risk enforces the configured money limits. Every order — whether from
// the strategy engine or a manual API request — must pass Authorize before it
// reaches a broker. The manager clamps order size down to fit within limits and
// fails closed: if a limit cannot be satisfied, the order is rejected.
package risk

import (
	"fmt"
	"sync"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
)

// Decision is the result of authorising an order.
type Decision struct {
	Approved bool    `json:"approved"`
	Symbol   string  `json:"symbol"`
	Side     broker.Side `json:"side"`
	OrigQty  float64 `json:"orig_qty"`
	FinalQty float64 `json:"final_qty"`
	EstValue float64 `json:"est_value"`
	Reason   string  `json:"reason"`
}

// Manager enforces money limits and tracks rolling daily spend.
type Manager struct {
	mu         sync.Mutex
	limits     config.Limits
	costs      config.CostsConfig
	day        string  // YYYY-MM-DD of current trading day
	spentToday float64 // cumulative buy cash outlay today (incl. commission)
}

// New creates a risk manager from configured limits and a commission model.
func New(limits config.Limits, costs config.CostsConfig) *Manager {
	return &Manager{limits: limits, costs: costs, day: today()}
}

// Limits returns the current limits.
func (m *Manager) Limits() config.Limits {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.limits
}

// SetLimits replaces the active limits (used by the API).
func (m *Manager) SetLimits(l config.Limits) {
	m.mu.Lock()
	m.limits = l
	m.mu.Unlock()
}

// Restore sets the rolling daily-spend accumulator for a given day, used to
// rehydrate state from persistence on startup. If the persisted day is not
// today, the spend is treated as stale and reset to zero.
func (m *Manager) Restore(day string, amount float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if day == today() {
		m.day = day
		m.spentToday = amount
	} else {
		m.day = today()
		m.spentToday = 0
	}
}

// SpentToday returns the cumulative buy notional for the current day.
func (m *Manager) SpentToday() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rollDay()
	return m.spentToday
}

// Authorize validates a proposed order against all money limits given the
// current portfolio snapshot and an estimated execution price. It returns a
// Decision whose FinalQty may be smaller than the requested Qty (clamped to
// fit). Sells are always allowed up to the held quantity. The returned order
// (when Approved) carries FinalQty.
func (m *Manager) Authorize(o broker.Order, estPrice float64, snap portfolio.Snapshot) (broker.Order, Decision) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rollDay()

	d := Decision{Symbol: o.Symbol, Side: o.Side, OrigQty: o.Qty}

	if estPrice <= 0 {
		d.Reason = "no valid price"
		return o, d
	}
	if o.Qty <= 0 {
		d.Reason = "non-positive quantity"
		return o, d
	}

	if o.Side == broker.Sell {
		held := snap.Holdings[o.Symbol].Qty
		if held <= 0 {
			d.Reason = "no position to sell"
			return o, d
		}
		qty := min(o.Qty, held)
		o.Qty = qty
		d.Approved = true
		d.FinalQty = qty
		d.EstValue = qty * estPrice
		d.Reason = "sell authorised"
		return o, d
	}

	// ----- BUY path: apply every money limit as a qty ceiling -----
	l := m.limits
	qty := o.Qty

	// Cash-affecting caps (spend / reserve) must account for commission, so we
	// divide their headroom by an effective price that includes the percentage
	// fee and reserve the fixed component up front: commission is
	// max(min, flat + pct·notional) <= max(flat, min) + pct·notional, so
	// deducting max(flat, min) from the headroom bounds the true cash cost.
	// Position-value caps use the bare notional price.
	effPrice := estPrice * (1 + m.costs.CommissionPct)
	fixedFee := max(m.costs.CommissionFlat, m.costs.CommissionMin)

	type cap struct {
		name     string
		maxValue float64 // remaining dollar headroom for this dimension
		price    float64 // per-share divisor (notional or commission-adjusted)
		enabled  bool
	}
	caps := []cap{
		{"max_order_value", l.MaxOrderValue, estPrice, l.MaxOrderValue > 0},
		{"max_per_position", l.MaxPerPosition - snap.Holdings[o.Symbol].MarketValue, estPrice, l.MaxPerPosition > 0},
		{"max_total_invested", l.MaxTotalInvested - snap.Invested, estPrice, l.MaxTotalInvested > 0},
		{"max_daily_spend", l.MaxDailySpend - m.spentToday - fixedFee, effPrice, l.MaxDailySpend > 0},
		{"cash_reserve", snap.Cash - l.CashReserve - fixedFee, effPrice, true},
	}

	binding := ""
	for _, c := range caps {
		if !c.enabled {
			continue
		}
		if c.maxValue <= 0 {
			d.Reason = fmt.Sprintf("blocked by %s (no headroom)", c.name)
			return o, d
		}
		maxQty := c.maxValue / c.price
		if maxQty < qty {
			qty = maxQty
			binding = c.name
		}
	}

	// New-symbol position-count limit.
	if l.MaxOpenPositions > 0 {
		_, alreadyHeld := snap.Holdings[o.Symbol]
		if !alreadyHeld && snap.OpenPositions >= l.MaxOpenPositions {
			d.Reason = fmt.Sprintf("blocked by max_open_positions (%d held)", snap.OpenPositions)
			return o, d
		}
	}

	// Round down to 4 decimals (fractional shares) and require a meaningful size.
	qty = float64(int(qty*1e4)) / 1e4
	if qty*estPrice < 1 { // less than $1 notional is not worth trading
		d.Reason = "clamped below tradable minimum after limits"
		return o, d
	}

	o.Qty = qty
	notional := qty * estPrice
	d.Approved = true
	d.FinalQty = qty
	// EstValue is the full cash cost of the buy, including commission.
	d.EstValue = notional + m.costs.Commission(notional)
	if binding != "" {
		d.Reason = fmt.Sprintf("buy authorised, clamped by %s", binding)
	} else {
		d.Reason = "buy authorised"
	}
	return o, d
}

// Commission returns the modelled commission for a trade of the given notional.
func (m *Manager) Commission(notional float64) float64 { return m.costs.Commission(notional) }

// RecordBuy adds a filled buy's cash outlay (notional + commission) to the
// rolling daily spend. Call after a buy order is confirmed filled.
func (m *Manager) RecordBuy(notional float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rollDay()
	m.spentToday += notional + m.costs.Commission(notional)
}

// rollDay resets daily spend when the trading day changes. Caller holds m.mu.
func (m *Manager) rollDay() {
	if d := today(); d != m.day {
		m.day = d
		m.spentToday = 0
	}
}

func today() string { return time.Now().UTC().Format("2006-01-02") }
