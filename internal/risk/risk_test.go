package risk

import (
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
)

// TestMaxDailySpendRollsOverOnSimulatedClock reproduces the backtest bug
// where max_daily_spend never reset: without SetClock, rollDay compares
// against the wall-clock day, so a multi-day replay accumulates spend from
// bar 0 forever and silently blocks every buy after the first simulated day
// exhausts the budget. With SetClock driving the manager from simulated
// time, the budget must roll over exactly like it would live.
func TestMaxDailySpendRollsOverOnSimulatedClock(t *testing.T) {
	m := New(config.Limits{
		MaxOrderValue: 1e6, MaxPerPosition: 1e6, MaxTotalInvested: 1e6,
		MaxDailySpend: 1000, MaxOpenPositions: 10,
	}, config.CostsConfig{})

	day1 := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)

	clock := day1
	m.SetClock(func() time.Time { return clock })

	snap := portfolio.Snapshot{Cash: 1e6, Holdings: map[string]portfolio.Holding{}}
	order := broker.Order{Symbol: "AAPL", Side: broker.Buy, Qty: 5, Type: broker.Market}

	// Spend the whole day-1 budget.
	_, d := m.Authorize(order, 200, snap) // 1000 notional
	if !d.Approved {
		t.Fatalf("day 1 first buy blocked: %s", d.Reason)
	}

	m.RecordBuy(d.EstValue)

	if _, d := m.Authorize(order, 200, snap); d.Approved {
		t.Fatalf("day 1 second buy should be blocked by max_daily_spend, got approved: %+v", d)
	}

	// Advance the simulated clock into day 2 — the budget must reset, exactly
	// as it would for a live bot waking up the next morning. Before SetClock
	// existed, rollDay used time.Now() and this assertion would fail (or
	// rather, the whole backtest would already have been broken from the
	// first simulated day onward, independent of what today's real date is).
	clock = day1.AddDate(0, 0, 1)

	if _, d := m.Authorize(order, 200, snap); !d.Approved {
		t.Fatalf("day 2 buy should be approved after the simulated day rolled over: %s", d.Reason)
	}
}

// TestAuthorizeReservesHeadroomForSpreadAndSlippage reproduces the
// spread/slippage finding: sizing a buy against cash_reserve using the bare
// reference price (ignoring spread/slippage) lets an order through that,
// once actually filled at the real (worse) price every live/sim fill uses,
// leaves cash below the configured reserve.
func TestAuthorizeReservesHeadroomForSpreadAndSlippage(t *testing.T) {
	costs := config.CostsConfig{SpreadPct: 0.004, SlippagePct: 0.002} // 40bps spread + 20bps slippage
	m := New(config.Limits{
		MaxOrderValue: 1e6, MaxPerPosition: 1e6, MaxTotalInvested: 1e6,
		MaxDailySpend: 1e6, MaxOpenPositions: 10, CashReserve: 100,
	}, costs)

	price := 100.0
	snap := portfolio.Snapshot{Cash: 1100, Holdings: map[string]portfolio.Holding{}}      // exactly 1000 above the reserve
	order := broker.Order{Symbol: "AAPL", Side: broker.Buy, Qty: 10, Type: broker.Market} // 1000 notional at the reference price

	authOrder, d := m.Authorize(order, price, snap)
	if !d.Approved {
		t.Fatalf("order blocked: %s", d.Reason)
	}

	// The actual fill price every live/sim path uses — computed the same way
	// config.CostsConfig.FillPrice does, inlined here to keep the assertion
	// independent of that function's own correctness.
	adverse := costs.SpreadPct/2 + costs.SlippagePct
	fillPrice := price * (1 + adverse)
	actualCost := authOrder.Qty * fillPrice

	remainingCash := snap.Cash - actualCost
	if remainingCash < 100 {
		t.Errorf("authorized qty %v leaves cash at %.4f after the real fill price, "+
			"below the configured cash_reserve of 100 — Authorize sized against the bare "+
			"reference price and ignored spread/slippage", authOrder.Qty, remainingCash)
	}
}

// TestMaxOpenPositionsIgnoresAZeroQuantityRow reproduces the
// "max_open_positions bypassed by a zero-quantity holdings row" finding: a
// broker can report a zero-quantity row for a symbol right after a position
// closes (eventual consistency, or a rounding remainder), and
// portfolio.SyncPositions carries that row straight through. Authorize used
// to treat map presence alone as "already held", exempting a fresh buy into
// that symbol from the new-position cap it should have counted against.
func TestMaxOpenPositionsIgnoresAZeroQuantityRow(t *testing.T) {
	m := New(config.Limits{
		MaxOrderValue: 1e6, MaxPerPosition: 1e6, MaxTotalInvested: 1e6,
		MaxDailySpend: 1e6, MaxOpenPositions: 1,
	}, config.CostsConfig{})

	// Built through the real Portfolio type (not a hand-rolled Snapshot
	// literal) so the test exercises portfolio.Snapshot's own OpenPositions
	// count together with Authorize's alreadyHeld check — the two halves of
	// this finding's fix have to agree with each other, not just each look
	// correct in isolation.
	pf := portfolio.New()
	pf.SetCash(1e6)
	pf.SyncPositions([]portfolio.Holding{
		// A closed position the broker still reports at qty 0 — not a real
		// open position.
		{Symbol: "OLD", Qty: 0},
	})

	snap := pf.Snapshot()

	order := broker.Order{Symbol: "NEW", Side: broker.Buy, Qty: 1, Type: broker.Market}

	if _, d := m.Authorize(order, 100, snap); !d.Approved {
		t.Fatalf(
			"a fresh entry into a different symbol was blocked by a stale zero-qty row: %s",
			d.Reason,
		)
	}
}

// A genuinely held (non-zero) row must still count toward the cap and exempt
// itself, exactly as before.
func TestMaxOpenPositionsStillAppliesToRealHoldings(t *testing.T) {
	m := New(config.Limits{
		MaxOrderValue: 1e6, MaxPerPosition: 1e6, MaxTotalInvested: 1e6,
		MaxDailySpend: 1e6, MaxOpenPositions: 1,
	}, config.CostsConfig{})

	pf := portfolio.New()
	pf.SetCash(1e6)
	pf.SyncPositions([]portfolio.Holding{{Symbol: "OLD", Qty: 10, MarketValue: 1000}})

	held := pf.Snapshot()

	// A new symbol is blocked: the cap of 1 is already met by a real holding.
	if _, d := m.Authorize(
		broker.Order{Symbol: "NEW", Side: broker.Buy, Qty: 1, Type: broker.Market}, 100, held,
	); d.Approved {
		t.Fatal("a new symbol was approved despite max_open_positions already being met")
	}

	// Adding to the SAME already-held symbol is still exempt.
	if _, d := m.Authorize(
		broker.Order{Symbol: "OLD", Side: broker.Buy, Qty: 1, Type: broker.Market}, 100, held,
	); !d.Approved {
		t.Fatalf("adding to an already-held symbol was blocked: %s", d.Reason)
	}
}

// TestSpentTodayResetsAcrossManyRolledDays exercises SpentToday directly
// (the read path GET /api/status uses) across several simulated day
// boundaries, including one where RecordBuy is the first call of the new
// day rather than Authorize.
func TestSpentTodayResetsAcrossManyRolledDays(t *testing.T) {
	m := New(config.Limits{MaxDailySpend: 100}, config.CostsConfig{})

	clock := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	m.SetClock(func() time.Time { return clock })

	m.RecordBuy(60)

	if got := m.SpentToday(); got != 60 {
		t.Fatalf("SpentToday = %v, want 60", got)
	}

	clock = clock.AddDate(0, 0, 1)

	if got := m.SpentToday(); got != 0 {
		t.Fatalf("SpentToday after a simulated day roll = %v, want 0 (reset)", got)
	}

	m.RecordBuy(40)

	if got := m.SpentToday(); got != 40 {
		t.Fatalf("SpentToday on day 2 = %v, want 40", got)
	}
}

// TestReserveHoldsHeadroomAgainstCashReserve reproduces the "no cash
// reservation between Authorize and RecordBuy" finding: a buy that has been
// authorised and sent toward a broker, but whose fill is not yet confirmed
// (a resting limit order can stay outstanding for minutes), left no trace
// anywhere Authorize could see — a second buy authorised in that window saw
// the exact same unspent snap.Cash the first one is about to commit, and
// could pass the cash_reserve check using cash that's already spoken for.
func TestReserveHoldsHeadroomAgainstCashReserve(t *testing.T) {
	m := New(config.Limits{
		MaxOrderValue: 1e6, MaxPerPosition: 1e6, MaxTotalInvested: 1e6,
		MaxDailySpend: 1e6, MaxOpenPositions: 10, CashReserve: 0,
	}, config.CostsConfig{})

	snap := portfolio.Snapshot{Cash: 1000, Holdings: map[string]portfolio.Holding{}}
	order := broker.Order{Symbol: "AAPL", Side: broker.Buy, Qty: 5, Type: broker.Market}

	// First buy authorised for the whole 1000 of cash; not yet recorded as
	// spent (its fill has not been confirmed), but reserved.
	_, d1 := m.Authorize(order, 200, snap) // 1000 notional
	if !d1.Approved {
		t.Fatalf("first buy blocked: %s", d1.Reason)
	}

	m.Reserve(d1.EstValue)

	// Second buy authorised against the SAME stale snapshot (as a caller
	// re-fetching a not-yet-synced portfolio snapshot would see) must be
	// blocked: the first buy's estimated cost already claims all the cash.
	if _, d2 := m.Authorize(order, 200, snap); d2.Approved {
		t.Fatalf("second buy against the same unspent cash should be blocked while the "+
			"first is still reserved, got approved: %+v", d2)
	}
}

// TestReleaseRestoresHeadroom confirms Release is the counterpart to
// Reserve: once a reservation is released (the order is known to have
// failed, or its real cost has been recorded via RecordBuy), the headroom
// it held is available again.
func TestReleaseRestoresHeadroom(t *testing.T) {
	m := New(config.Limits{
		MaxOrderValue: 1e6, MaxPerPosition: 1e6, MaxTotalInvested: 1e6,
		MaxDailySpend: 1e6, MaxOpenPositions: 10, CashReserve: 0,
	}, config.CostsConfig{})

	snap := portfolio.Snapshot{Cash: 1000, Holdings: map[string]portfolio.Holding{}}
	order := broker.Order{Symbol: "AAPL", Side: broker.Buy, Qty: 5, Type: broker.Market}

	id := m.Reserve(1000)

	if _, d := m.Authorize(order, 200, snap); d.Approved {
		t.Fatalf("buy should be blocked while the reservation stands, got approved: %+v", d)
	}

	m.Release(id)

	if _, d := m.Authorize(order, 200, snap); !d.Approved {
		t.Fatalf("buy should be approved once the reservation is released: %s", d.Reason)
	}
}

// TestReleaseIsSafeToCallTwiceOrWithZero guards the two "no-op" contracts
// Release documents: id 0 (what Reserve/ReserveBuy return for a
// non-positive amount) and an already-released id must both be safe, so
// every caller along the many buy paths can release unconditionally without
// tracking whether it already did.
func TestReleaseIsSafeToCallTwiceOrWithZero(t *testing.T) {
	m := New(config.Limits{MaxDailySpend: 1e6}, config.CostsConfig{})

	m.Release(0) // must not panic

	id := m.Reserve(500)
	m.Release(id)
	m.Release(id) // double release must not panic or under/overflow the total

	if got := m.reservedTotal(); got != 0 {
		t.Fatalf("reservedTotal after a double release = %v, want 0", got)
	}
}

// TestReserveBuyMatchesAuthorizesOwnCostBasis confirms ReserveBuy estimates a
// buy's cash cost the same way Authorize's own cash_reserve/max_daily_spend
// headroom does — commission and the adverse spread/slippage markup
// included — so a reservation and the check it's meant to protect can never
// silently disagree about how expensive an order really is.
func TestReserveBuyMatchesAuthorizesOwnCostBasis(t *testing.T) {
	costs := config.CostsConfig{SpreadPct: 0.004, SlippagePct: 0.002, CommissionPct: 0.001, CommissionMin: 1}
	m := New(config.Limits{
		MaxOrderValue: 1e6, MaxPerPosition: 1e6, MaxTotalInvested: 1e6,
		MaxDailySpend: 1e6, MaxOpenPositions: 10, CashReserve: 0,
	}, costs)

	price := 100.0
	snap := portfolio.Snapshot{Cash: 1015, Holdings: map[string]portfolio.Holding{}}
	order := broker.Order{Symbol: "AAPL", Side: broker.Buy, Qty: 10, Type: broker.Market} // 1000 notional

	_, d := m.Authorize(order, price, snap)
	if !d.Approved {
		t.Fatalf("order blocked: %s", d.Reason)
	}

	// ReserveBuy for the exact same qty/price should reserve close enough to
	// d.EstValue that reserving it against an identical snapshot blocks a
	// second identical buy — proving the two bases agree in practice, not
	// just that ReserveBuy returns *some* positive number.
	m2 := New(config.Limits{
		MaxOrderValue: 1e6, MaxPerPosition: 1e6, MaxTotalInvested: 1e6,
		MaxDailySpend: 1e6, MaxOpenPositions: 10, CashReserve: 0,
	}, costs)

	m2.ReserveBuy(order.Qty, price)

	_, d2 := m2.Authorize(order, price, snap)
	if d2.Approved && d2.FinalQty >= order.Qty {
		t.Fatalf("second identical buy should be heavily clamped by the first's "+
			"reservation (only ~15 of cash's 1015 remains unreserved), got the full "+
			"qty approved: %+v", d2)
	}
}

// The tests below fill out Authorize's remaining, previously-untested
// paths: the three position-value caps (max_order_value, max_per_position,
// max_total_invested — every existing test above sets these deliberately
// non-binding to isolate whatever else it's testing), the basic sell path,
// and the two input-validation guards. See CODE_REVIEW_FINDINGS.md's
// "Manager.Authorize... has zero unit tests" finding — largely closed by
// the coverage already added alongside other fixes in this pass, but these
// specific paths were still gaps.

// TestAuthorizeClampsToMaxOrderValue checks a buy requesting more than
// max_order_value is approved at a reduced quantity, not rejected outright
// — the documented "clamps qty down" contract, not "fails closed" (that's
// reserved for when even the clamped qty would be unmeaningful).
func TestAuthorizeClampsToMaxOrderValue(t *testing.T) {
	m := New(config.Limits{
		MaxOrderValue: 500, MaxPerPosition: 1e6, MaxTotalInvested: 1e6,
		MaxDailySpend: 1e6, MaxOpenPositions: 10,
	}, config.CostsConfig{})

	snap := portfolio.Snapshot{Cash: 1e6, Holdings: map[string]portfolio.Holding{}}
	order := broker.Order{Symbol: "AAPL", Side: broker.Buy, Qty: 10, Type: broker.Market} // 1000 notional @ 100

	authOrder, d := m.Authorize(order, 100, snap)
	if !d.Approved {
		t.Fatalf("order blocked outright, want clamped: %s", d.Reason)
	}

	if want := 5.0; d.FinalQty != want || authOrder.Qty != want {
		t.Errorf("FinalQty = %v, want %v (500 max_order_value / 100 price)", d.FinalQty, want)
	}

	if !strings.Contains(d.Reason, "max_order_value") {
		t.Errorf("Reason = %q, want it to name the binding cap", d.Reason)
	}
}

// TestAuthorizeClampsToMaxPerPosition checks the cap is measured against
// the position's EXISTING market value, not just the new order in
// isolation — headroom is what's left, not the whole cap.
func TestAuthorizeClampsToMaxPerPosition(t *testing.T) {
	m := New(config.Limits{
		MaxOrderValue: 1e6, MaxPerPosition: 1000, MaxTotalInvested: 1e6,
		MaxDailySpend: 1e6, MaxOpenPositions: 10,
	}, config.CostsConfig{})

	snap := portfolio.Snapshot{
		Cash: 1e6,
		Holdings: map[string]portfolio.Holding{
			"AAPL": {Symbol: "AAPL", Qty: 5, MarketValue: 700}, // 300 of headroom left
		},
	}
	order := broker.Order{Symbol: "AAPL", Side: broker.Buy, Qty: 10, Type: broker.Market} // 1000 notional @ 100

	authOrder, d := m.Authorize(order, 100, snap)
	if !d.Approved {
		t.Fatalf("order blocked outright, want clamped: %s", d.Reason)
	}

	if want := 3.0; d.FinalQty != want || authOrder.Qty != want {
		t.Errorf("FinalQty = %v, want %v (300 remaining headroom / 100 price)", d.FinalQty, want)
	}

	if !strings.Contains(d.Reason, "max_per_position") {
		t.Errorf("Reason = %q, want it to name the binding cap", d.Reason)
	}
}

// TestAuthorizeClampsToMaxTotalInvested checks the portfolio-wide cap is
// measured against total Invested across every symbol, not just this one.
func TestAuthorizeClampsToMaxTotalInvested(t *testing.T) {
	m := New(config.Limits{
		MaxOrderValue: 1e6, MaxPerPosition: 1e6, MaxTotalInvested: 5000,
		MaxDailySpend: 1e6, MaxOpenPositions: 10,
	}, config.CostsConfig{})

	snap := portfolio.Snapshot{
		Cash:     1e6,
		Invested: 4800, // 200 of headroom left across the whole portfolio
		Holdings: map[string]portfolio.Holding{},
	}
	order := broker.Order{Symbol: "AAPL", Side: broker.Buy, Qty: 10, Type: broker.Market} // 1000 notional @ 100

	authOrder, d := m.Authorize(order, 100, snap)
	if !d.Approved {
		t.Fatalf("order blocked outright, want clamped: %s", d.Reason)
	}

	if want := 2.0; d.FinalQty != want || authOrder.Qty != want {
		t.Errorf("FinalQty = %v, want %v (200 remaining headroom / 100 price)", d.FinalQty, want)
	}

	if !strings.Contains(d.Reason, "max_total_invested") {
		t.Errorf("Reason = %q, want it to name the binding cap", d.Reason)
	}
}

// TestAuthorizeSellPath covers the basic sell contract: approved and
// clamped to the held quantity when it exceeds what's actually held, and
// rejected when nothing is held at all. Sells are documented to bypass
// every money limit — this only exercises the qty-clamping logic itself.
func TestAuthorizeSellPath(t *testing.T) {
	m := New(config.Limits{MaxOrderValue: 1}, config.CostsConfig{}) // deliberately tiny; must not apply to sells

	snap := portfolio.Snapshot{
		Cash:     0,
		Holdings: map[string]portfolio.Holding{"AAPL": {Symbol: "AAPL", Qty: 5}},
	}

	// Requesting more than held clamps to the held quantity.
	authOrder, d := m.Authorize(
		broker.Order{Symbol: "AAPL", Side: broker.Sell, Qty: 10, Type: broker.Market}, 100, snap,
	)
	if !d.Approved || d.FinalQty != 5 || authOrder.Qty != 5 {
		t.Errorf("sell of 10 against 5 held = %+v, want approved and clamped to 5", d)
	}

	if !strings.Contains(d.Reason, "sell authorised") {
		t.Errorf("Reason = %q, want it to say the sell was authorised", d.Reason)
	}

	// No holding at all is rejected outright.
	_, d = m.Authorize(
		broker.Order{Symbol: "MSFT", Side: broker.Sell, Qty: 1, Type: broker.Market}, 100, snap,
	)
	if d.Approved {
		t.Errorf("sell of an unheld symbol was approved: %+v", d)
	}

	if !strings.Contains(d.Reason, "no position to sell") {
		t.Errorf("Reason = %q, want it to say there's no position", d.Reason)
	}
}

// TestAuthorizeRejectsInvalidInputs covers the two guards before any limit
// check runs: a non-positive price and a non-positive quantity.
func TestAuthorizeRejectsInvalidInputs(t *testing.T) {
	m := New(config.Limits{MaxOrderValue: 1e6}, config.CostsConfig{})
	snap := portfolio.Snapshot{Cash: 1e6, Holdings: map[string]portfolio.Holding{}}

	if _, d := m.Authorize(
		broker.Order{Symbol: "AAPL", Side: broker.Buy, Qty: 1, Type: broker.Market}, 0, snap,
	); d.Approved || !strings.Contains(d.Reason, "no valid price") {
		t.Errorf("zero price: %+v, want rejected with 'no valid price'", d)
	}

	if _, d := m.Authorize(
		broker.Order{Symbol: "AAPL", Side: broker.Buy, Qty: 0, Type: broker.Market}, 100, snap,
	); d.Approved || !strings.Contains(d.Reason, "non-positive quantity") {
		t.Errorf("zero quantity: %+v, want rejected with 'non-positive quantity'", d)
	}
}

// TestSellIsAuthorizedRegardlessOfSymbolCase is the risk-side half of the EN-6
// finding: Authorize indexed snap.Holdings with the raw order symbol while
// PortfolioGuard.AllowEntry upper-cased first. A broker reporting a
// differently-cased ticker made every exit fail with "no position to sell" —
// the one refusal this package must never make by accident, since sells are
// meant to always go through up to the held quantity.
func TestSellIsAuthorizedRegardlessOfSymbolCase(t *testing.T) {
	m := New(config.Limits{MaxOpenPositions: 10}, config.CostsConfig{})

	pf := portfolio.New()
	pf.SetCash(1000)
	pf.SyncPositions([]portfolio.Holding{
		{Symbol: "sap.de", Qty: 10, AvgPrice: 100, MarketValue: 1000},
	})

	order := broker.Order{Symbol: "SAP.DE", Side: broker.Sell, Qty: 10, Type: broker.Market}

	out, d := m.Authorize(order, 100, pf.Snapshot())
	if !d.Approved {
		t.Fatalf("sell refused: %q — a case difference must not block an exit", d.Reason)
	}

	if out.Qty != 10 {
		t.Errorf("authorised qty = %v, want the full 10 held", out.Qty)
	}
}
