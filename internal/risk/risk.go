// Package risk enforces the configured money limits. Every order — whether from
// the strategy engine or a manual API request — must pass Authorize before it
// reaches a broker. The manager clamps order size down to fit within limits and
// fails closed: if a limit cannot be satisfied, the order is rejected.
package risk

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
)

// Decision is the result of authorising an order.
type Decision struct {
	Approved bool        `json:"approved"`
	Symbol   string      `json:"symbol"`
	Side     broker.Side `json:"side"`
	OrigQty  float64     `json:"orig_qty"`
	FinalQty float64     `json:"final_qty"`
	EstValue float64     `json:"est_value"`
	Reason   string      `json:"reason"`
}

// Manager enforces money limits and tracks rolling daily spend.
type Manager struct {
	mu         sync.Mutex
	limits     config.Limits
	costs      config.CostsConfig
	now        func() time.Time
	day        string  // YYYY-MM-DD of current trading day
	spentToday float64 // cumulative buy cash outlay today (incl. commission)

	// reserved holds the estimated cash cost of buys that have been
	// authorised and sent toward a broker but whose outcome (filled, or
	// definitively not filled) is not yet known — see Reserve.
	reserved  map[uint64]float64
	nextResID uint64

	// guard supplies the one portfolio limit that sizes an order rather than
	// refusing it: min_positions_per_asset_class (see
	// PortfolioGuard.MaxPositionValue). The guard's other caps stay in
	// AllowEntry, which the engine calls separately — they are allow/deny
	// questions about concentration, not money ceilings. May be nil.
	guard *PortfolioGuard

	// minPositions is engine.min_positions: the global "spread the budget
	// across at least this many positions" setting. It is the coarse tier of
	// the same rule min_positions_per_asset_class refines per class — see
	// PortfolioGuard.MaxPositionValue, which resolves the two — so it lives
	// here rather than in the engine's sizing code, where it used to be a
	// separate max_total_invested/minPositions ceiling that Authorize never
	// saw.
	minPositions int
}

// New creates a risk manager from configured limits and a commission model.
func New(limits config.Limits, costs config.CostsConfig) *Manager {
	m := &Manager{limits: limits, costs: costs, now: time.Now}
	m.day = m.today()

	return m
}

// SetClock replaces the manager's time source. A backtest must drive it from
// the bar being simulated, not the wall clock: max_daily_spend is a rolling
// per-calendar-day budget, and without this it never rolls over during a
// multi-day (or multi-year) replay — the whole run shares the first
// simulated day's spend and silently stops authorising buys once it's
// exhausted. See internal/protect.Manager.SetClock, which has the same
// requirement and the same fix.
//
// Call it before use; it is not safe to change the clock concurrently with
// Authorize/RecordBuy.
func (m *Manager) SetClock(now func() time.Time) {
	if m == nil || now == nil {
		return
	}

	m.now = now
}

// SetPortfolioGuard installs (or, with nil, removes) the guard whose
// per-class position slots Authorize sizes against. The guard is stateless and
// rebuilt whenever the portfolio config changes, so this is called again with
// the new one — Engine.SetPortfolioGuard does both in one place so the manager
// and the engine can never be enforcing two different generations of it.
func (m *Manager) SetPortfolioGuard(g *PortfolioGuard) {
	if m == nil {
		return
	}

	m.mu.Lock()

	m.guard = g
	m.mu.Unlock()
}

// SetMinPositions updates the global min_positions tier. Like SetLimits this
// is a whole-value replace: 0 is a real value meaning "no global split".
func (m *Manager) SetMinPositions(n int) {
	if m == nil {
		return
	}

	m.mu.Lock()

	m.minPositions = n
	m.mu.Unlock()
}

// MinPositions returns the global min_positions tier.
func (m *Manager) MinPositions() int {
	if m == nil {
		return 0
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	return m.minPositions
}

// investableBase is what the book can actually put to work: equity less the
// cash the operator wants left alone, never more than max_total_invested
// allows to be invested at once. It is the denominator every position-slot
// share divides (see PortfolioGuard.MaxPositionValue).
//
// Equity alone overstates it. Dividing 1000 of equity into four 250 slots
// when cash_reserve holds 50 back and max_total_invested caps deployment at
// 950 promises a fourth position the account may never fund; dividing 950
// means the slots actually fit. Caller holds m.mu.
func (m *Manager) investableBase(snap portfolio.Snapshot) float64 {
	l := m.limits.Resolve(equityOf(snap))

	base := equityOf(snap) - l.CashReserve
	if l.MaxTotalInvested > 0 && l.MaxTotalInvested < base {
		base = l.MaxTotalInvested
	}

	return max(base, 0)
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

// Costs returns the active commission and friction model.
func (m *Manager) Costs() config.CostsConfig {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.costs
}

// SetCosts replaces the commission and friction model. It affects order sizing
// and the cash-headroom checks immediately; the rolling daily spend already
// recorded is not restated, since those trades really did cost what they cost.
func (m *Manager) SetCosts(c config.CostsConfig) {
	m.mu.Lock()

	m.costs = c
	m.mu.Unlock()
}

// Restore sets the rolling daily-spend accumulator for a given day, used to
// rehydrate state from persistence on startup. If the persisted day is not
// today, the spend is treated as stale and reset to zero.
func (m *Manager) Restore(day string, amount float64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if day == m.today() {
		m.day = day
		m.spentToday = amount
	} else {
		m.day = m.today()
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

// Reserve records a pending buy's estimated cash cost so that a subsequent
// Authorize call — made before this buy is known to have filled or
// definitively failed — sees reduced cash_reserve/max_daily_spend headroom
// instead of the same stale figure a still-pending order already
// committed. Without this, two orders submitted close together (a resting
// limit order can stay outstanding for minutes: RecordBuy is not called
// until a fill is actually confirmed) could each be authorised against the
// same cash, together spending more than either cap allows.
//
// amount is the estimated full cash cost (see estimatedCost) of the pending
// buy. A non-positive amount reserves nothing and returns id 0, which
// Release always treats as a safe no-op — callers do not need to guard the
// non-buy/non-positive case themselves.
//
// Release the returned id once the order's fate is known: on any outcome
// that is NOT a confirmed fill (submit error, a confirmed cancel/reject, or
// definitively giving up on a resting order), or once RecordBuy has booked
// the real spend for a fill. An outcome the broker did not confirm either
// way should be left reserved rather than released — an unreleased
// reservation only makes a future buy more conservative, never less safe,
// matching the fails-closed design the rest of this package already uses.
func (m *Manager) Reserve(amount float64) uint64 {
	if amount <= 0 {
		return 0
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.nextResID++
	id := m.nextResID

	if m.reserved == nil {
		m.reserved = make(map[uint64]float64)
	}

	m.reserved[id] = amount

	return id
}

// ReserveBuy is Reserve for a qty at a venue price, estimating the cash cost
// the same way Authorize's cash_reserve/max_daily_spend headroom does (see
// estimatedCost), so a reservation and the headroom it eats into are always
// computed on a consistent basis.
func (m *Manager) ReserveBuy(qty, price float64) uint64 {
	if qty <= 0 || price <= 0 {
		return 0
	}

	m.mu.Lock()
	amount := m.estimatedCost(qty, price)
	m.mu.Unlock()

	return m.Reserve(amount)
}

// Release clears a reservation made by Reserve/ReserveBuy. id 0 (what
// Reserve returns for a non-positive amount) and an already-released id are
// both safe no-ops, so callers never need to track whether a release
// already happened.
func (m *Manager) Release(id uint64) {
	if id == 0 {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.reserved, id)
}

// reservedTotal sums all outstanding reservations. Caller holds m.mu.
func (m *Manager) reservedTotal() float64 {
	var sum float64
	for _, v := range m.reserved {
		sum += v
	}

	return sum
}

// estimatedCost is the same commission/adverse-price-adjusted per-unit basis
// Authorize uses to convert cash_reserve/max_daily_spend headroom into a
// quantity ceiling, applied in reverse: the estimated full cash cost of
// buying qty at price. Caller holds m.mu.
func (m *Manager) estimatedCost(qty, price float64) float64 {
	adverse := m.costs.SpreadPct/2 + m.costs.SlippagePct
	effPrice := price * (1 + m.costs.CommissionPct + adverse)
	fixedFee := max(m.costs.CommissionFlat, m.costs.CommissionMin)

	return qty*effPrice + fixedFee
}

// Authorize validates a proposed order against all money limits given the
// current portfolio snapshot and an estimated execution price. It returns a
// Decision whose FinalQty may be smaller than the requested Qty (clamped to
// fit). Sells are always allowed up to the held quantity. The returned order
// (when Approved) carries FinalQty.
//
//nolint:cyclop,funlen // sequential limit gates; splitting would obscure the check order
func (m *Manager) Authorize(
	o broker.Order,
	estPrice float64,
	snap portfolio.Snapshot,
) (broker.Order, Decision) {
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
		// Normalized lookup, matching how portfolio keys its holdings and how
		// PortfolioGuard.AllowEntry already looked them up — a case mismatch
		// here rejects an exit outright ("no position to sell"), which is the
		// one refusal this package must never make by accident.
		// Snapshot.Holding, not a raw map index: a case/whitespace mismatch
		// would report "not held" and reject the exit outright — the one
		// refusal this package must never make by accident.
		heldPos, _ := snap.Holding(o.Symbol)
		held := heldPos.Qty
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
	//
	// Resolved against current equity first, so a limit written as a
	// percentage binds exactly like the fixed amount it stands in for. See
	// config.Limits.Resolve.
	l := m.limits.Resolve(equityOf(snap))
	qty := o.Qty

	caps := m.buyCapRules(o, estPrice, snap)

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
		// A zero-quantity holdings row (a broker reporting a just-closed
		// position, or a rounding remainder) is not an existing position to
		// exempt this buy from the cap — it would otherwise let a fresh
		// entry into that symbol skip the check entirely. See
		// CODE_REVIEW_FINDINGS.md's "max_open_positions bypassed by a
		// zero-quantity holdings row".
		cur, _ := snap.Holding(o.Symbol)
		alreadyHeld := cur.Qty > 0
		if !alreadyHeld && snap.OpenPositions >= l.MaxOpenPositions {
			d.Reason = fmt.Sprintf("blocked by max_open_positions (%d held)", snap.OpenPositions)
			return o, d
		}
	}

	// Round down to 4 decimals (fractional shares) and require a meaningful size.
	qty = float64(int(qty*1e4)) / 1e4
	if qty*estPrice < 1 { // less than $1 notional is not worth trading
		// Name the cap that squeezed it down. Without this the operator sees
		// only "clamped below tradable minimum" and cannot tell whether they
		// are out of cash, at max_total_invested, or up against the daily
		// spend budget — three problems with three different answers, and the
		// binding rule is already known right here.
		if binding != "" {
			d.Reason = fmt.Sprintf(
				"clamped below tradable minimum after limits: %s left room for only %.4f @ %.2f (%.2f)",
				binding, qty, estPrice, qty*estPrice)
		} else {
			d.Reason = "clamped below tradable minimum after limits"
		}

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

// capRule is one money-limit dimension considered when sizing/authorising a
// BUY: how much dollar headroom remains under it, and what price converts
// that headroom into a share quantity. Shared between Authorize (which
// clamps qty to the smallest binding cap) and BuyHeadroom (which reports the
// smallest cap's dollar headroom directly, for advisory sizing) — a single
// source of truth for what "how much room is left" means, so an engine-side
// sizing estimate can never drift from what Authorize will actually approve.
type capRule struct {
	name     string
	maxValue float64 // remaining dollar headroom for this dimension
	price    float64 // per-share divisor (notional or commission-adjusted)
	enabled  bool
}

// buyCapRules computes the money-limit headroom for a candidate BUY of
// o.Symbol at estPrice, given the current portfolio snap.
func (m *Manager) buyCapRules(o broker.Order, estPrice float64, snap portfolio.Snapshot) []capRule {
	l := m.limits.Resolve(equityOf(snap))

	// Cash-affecting caps (spend / reserve) must account for commission AND
	// the price the fill actually happens at, so we divide their headroom by
	// an effective price that includes the percentage fee plus half the
	// spread and slippage — the same adverse-price markup
	// config.CostsConfig.FillPrice applies to every real market buy — and
	// reserve the fixed commission component up front: commission is
	// max(min, flat + pct·notional) <= max(flat, min) + pct·notional, so
	// deducting max(flat, min) from the headroom bounds the true cash cost.
	// Without the spread/slippage term, an order sized to exactly the
	// remaining cash_reserve/max_daily_spend headroom at the bare reference
	// price fills for MORE than that once real spread/slippage is applied,
	// landing below the configured cash floor. Position-value caps use the
	// bare notional price (spread/slippage there is a genuine mark-to-market
	// wobble, not a caller-controllable headroom to protect).
	adverse := m.costs.SpreadPct/2 + m.costs.SlippagePct
	effPrice := estPrice * (1 + m.costs.CommissionPct + adverse)
	fixedFee := max(m.costs.CommissionFlat, m.costs.CommissionMin)

	// reserved is the estimated cash cost of buys already authorised and
	// sent toward a broker whose outcome (filled, or definitively not) is
	// not yet known — see Reserve. Without subtracting it here, a second
	// buy evaluated before the first one's resting limit order resolves
	// would see the same unspent cash/budget the first one already
	// committed, and could authorise spending it twice over.
	reserved := m.reservedTotal()

	// The position slot this symbol is entitled to, from min_positions and/or
	// min_positions_per_asset_class. Like max_per_position it is a ceiling on
	// the POSITION, so what is already held comes off it — otherwise a
	// scale-in ladder would walk one name up to the whole class budget an add
	// at a time, which is the outcome the setting exists to prevent.
	//
	// Nil-safe: with no guard at all the global min_positions tier still
	// applies, since it works whether or not portfolio limits are enabled.
	slotValue, slotRule, slotted := m.guard.MaxPositionValue(
		o.Symbol, m.investableBase(snap), m.minPositions)

	return []capRule{
		{"max_order_value", l.MaxOrderValue, estPrice, l.MaxOrderValue > 0},
		{
			"max_per_position",
			l.MaxPerPosition - heldValue(snap, o.Symbol),
			estPrice,
			l.MaxPerPosition > 0,
		},
		{
			slotRule,
			slotValue - heldValue(snap, o.Symbol),
			estPrice,
			slotted,
		},
		{
			"max_total_invested",
			l.MaxTotalInvested - snap.Invested,
			estPrice,
			l.MaxTotalInvested > 0,
		},
		{
			"max_daily_spend",
			l.MaxDailySpend - m.spentToday - reserved - fixedFee,
			effPrice,
			l.MaxDailySpend > 0,
		},
		{"cash_reserve", snap.Cash - reserved - l.CashReserve - fixedFee, effPrice, true},
	}
}

// heldValue is the marked value already held in symbol, matched the same
// normalized way portfolio keys its holdings.
func heldValue(snap portfolio.Snapshot, symbol string) float64 {
	h, _ := snap.Holding(symbol)

	return h.MarketValue
}

// BuyHeadroom reports the most notional a new BUY of o.Symbol could reach
// right now under every enabled money limit — the smallest remaining
// headroom among max_order_value, max_per_position, max_total_invested,
// max_daily_spend and cash_reserve (cash_reserve is always enabled, so this
// is never unbounded). It is the same figure Authorize's own cap loop would
// clamp qty to, computed by the identical logic (buyCapRules) — advisory
// input for sizing that wants to aim at a real ceiling, not a substitute for
// Authorize, which remains the sole authority and re-checks everything
// regardless of what sizing aimed for. Read-only: does not touch
// spentToday, reserved, or roll the trading day.
//
// The minimum is taken over each rule's QUANTITY, then converted back to
// notional — not over the raw headroom figures, which are not in the same
// units. A capRule's maxValue is denominated in whatever its own price
// divides: the position caps hold notional at estPrice, but max_daily_spend
// and cash_reserve hold CASH COST at effPrice (commission plus half the
// spread plus slippage — see buyCapRules). Comparing those directly, as this
// used to, reported a cash cap's full cash headroom as though it were
// notional, so with costs configured the advisory ceiling came out above
// what Authorize would actually approve, and sizing aimed at a number it
// could only ever be clamped down from.
func (m *Manager) BuyHeadroom(o broker.Order, estPrice float64, snap portfolio.Snapshot) float64 {
	if estPrice <= 0 {
		return 0
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	maxQty := math.MaxFloat64

	for _, c := range m.buyCapRules(o, estPrice, snap) {
		if !c.enabled {
			continue
		}

		if c.price <= 0 {
			// No sane divisor for this rule, so no honest answer for it.
			// Skipping would report the OTHER caps' headroom as if this one
			// did not exist — the fails-open reading, in a package that fails
			// closed on money. Only reachable through a nonsensical cost model
			// (commission_pct <= -1), which config has no range check for.
			return 0
		}

		if q := c.maxValue / c.price; q < maxQty {
			maxQty = q
		}
	}

	if maxQty <= 0 || maxQty == math.MaxFloat64 {
		return 0
	}

	return maxQty * estPrice
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
	if d := m.today(); d != m.day {
		m.day = d
		m.spentToday = 0
	}
}

// today returns the current UTC day (YYYY-MM-DD) for the rolling daily
// spend, from m.now — the wall clock by default, or the simulated clock a
// backtest installs via SetClock.
func (m *Manager) today() string { return m.now().UTC().Format("2006-01-02") }
