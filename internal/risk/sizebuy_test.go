package risk

import (
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
)

// Buy sizing used to live in two hand-mirrored copies, Engine.riskSizedNotional
// and Backtester.riskSize, each with its own tests. Both now call SizeBuy, so
// the contract is pinned once, here.
//
// ATR arrives as a plain parameter rather than being derived from bar history,
// which is why these are so much shorter than the engine-side tests they
// replace: the stop arithmetic is exercised directly instead of through a
// seeded candle series.

// sizeBuyManager is a manager whose only binding limit is max_order_value, so
// BuyHeadroom is a known figure and the risk scaling is what moves the answer.
func sizeBuyManager(headroom float64) *Manager {
	return New(config.Limits{
		MaxOrderValue:    headroom,
		MaxPerPosition:   1e9,
		MaxTotalInvested: 1e9,
	}, config.CostsConfig{})
}

// sizeBuySnapshot is a book with enough cash that cash_reserve never binds.
func sizeBuySnapshot() portfolio.Snapshot {
	return portfolio.Snapshot{Cash: 1e9, Equity: 1e9, Holdings: map[string]portfolio.Holding{}}
}

// pctExits is a percentage-only stop: no ATR multiple, so every trade's actual
// stop distance equals the baseline exactly.
func pctExits(stopLossPct float64) config.ExitsConfig {
	return config.ExitsConfig{Enabled: true, StopLossPct: stopLossPct}
}

// With no ATR stop configured there is only one stop candidate, so every
// trade's distance equals stop_loss_pct and there is no risk variation to
// scale on. The full headroom must come back: "if the risk is unremarkable,
// I expect the whole amount".
func TestSizeBuyUsesTheFullHeadroomAtTheBaselineStop(t *testing.T) {
	t.Parallel()

	got := sizeBuyManager(1000).SizeBuy(
		"X", 100, sizeBuySnapshot(), SizingConfig{Exits: pctExits(0.05)}, 0)

	if !got.OK || got.Notional != 1000 {
		t.Errorf("SizeBuy = %+v, want the full 1000 headroom", got)
	}
}

// The other half of the contract: an ATR-derived stop WIDER than the baseline
// is a genuinely riskier entry right now, and the size shrinks in proportion.
// price 100, atr 10, mult 1 puts the ATR stop at 90 against a percentage stop
// at 95 — exits.Compute takes the wider one, so the distance is 10% against a
// 5% baseline and the order halves.
func TestSizeBuyScalesDownOnAWiderATRStop(t *testing.T) {
	t.Parallel()

	cfg := SizingConfig{Exits: pctExits(0.05)}
	cfg.Exits.ATRStopMult = 1

	got := sizeBuyManager(1000).SizeBuy("X", 100, sizeBuySnapshot(), cfg, 10)

	if !got.OK || got.Notional != 500 {
		t.Errorf("SizeBuy = %+v, want 500 (half, for twice the baseline stop)", got)
	}

	if !strings.Contains(got.Note, "risk-sized") {
		t.Errorf("note %q does not describe the reduction", got.Note)
	}
}

// A narrower ATR stop must NOT enlarge the order: exits.Compute always takes
// the wider of the two, so the ratio is capped at 1 by construction and the
// headroom is the ceiling, never a starting point to grow from.
func TestSizeBuyNeverGrowsPastTheHeadroom(t *testing.T) {
	t.Parallel()

	cfg := SizingConfig{Exits: pctExits(0.05)}
	cfg.Exits.ATRStopMult = 1

	// atr 1 at price 100 => an ATR stop only 1% away, narrower than the 5%
	// baseline, so the baseline stays in force.
	got := sizeBuyManager(1000).SizeBuy("X", 100, sizeBuySnapshot(), cfg, 1)

	if !got.OK || got.Notional != 1000 {
		t.Errorf("SizeBuy = %+v, want the headroom unchanged at 1000", got)
	}
}

// max_risk_size_usd is an absolute ceiling that binds independently of the
// proportional figure.
func TestSizeBuyRespectsMaxRiskSizeUSD(t *testing.T) {
	t.Parallel()

	got := sizeBuyManager(1000).SizeBuy("X", 100, sizeBuySnapshot(),
		SizingConfig{Exits: pctExits(0.05), MaxRiskSizeUSD: 250}, 0)

	if !got.OK || got.Notional != 250 {
		t.Errorf("SizeBuy = %+v, want the 250 cap", got)
	}
}

// Risk scaling needs a stop to measure against. With exits off, or no
// stop_loss_pct baseline, there is nothing to compare — the headroom stands
// unscaled rather than the order being refused.
func TestSizeBuyWithoutAStopUsesThePlainHeadroom(t *testing.T) {
	t.Parallel()

	m := sizeBuyManager(1000)
	snap := sizeBuySnapshot()

	cases := map[string]config.ExitsConfig{
		"exits disabled":   {Enabled: false, StopLossPct: 0.05},
		"no baseline stop": {Enabled: true, StopLossPct: 0},
	}

	for name, xc := range cases {
		got := m.SizeBuy("X", 100, snap, SizingConfig{Exits: xc}, 0)
		if !got.OK || got.Notional != 1000 {
			t.Errorf("%s: SizeBuy = %+v, want the plain 1000 headroom", name, got)
		}

		if got.Note != "" {
			t.Errorf("%s: note %q claims a reduction that did not happen", name, got.Note)
		}
	}
}

// min_order_size_usd REFUSES an order the risk scaling put below it — it is
// not a floor that raises one back up.
//
// The floor is what this used to do, and it had the sizing backwards: the
// orders it enlarged were exactly the ones the scaling had just judged too
// risky to hold at full size. An order below the size worth paying commission
// on is an order not worth placing.
func TestSizeBuyRefusesAnOrderBelowTheMinimum(t *testing.T) {
	t.Parallel()

	cfg := SizingConfig{Exits: pctExits(0.05), MinOrderSizeUSD: 600}
	cfg.Exits.ATRStopMult = 1 // halves the order, to 500

	got := sizeBuyManager(1000).SizeBuy("X", 100, sizeBuySnapshot(), cfg, 10)

	if got.OK {
		t.Fatalf("SizeBuy = %+v, want a refusal: 500 is below the 600 minimum", got)
	}

	if !strings.Contains(got.Reason, "min_order_size_usd") {
		t.Errorf("reason %q does not name the setting responsible", got.Reason)
	}
}

// The threshold is a no-op at 0 (the default) and when the order clears it.
func TestSizeBuyMinimumIsInertWhenSatisfied(t *testing.T) {
	t.Parallel()

	m := sizeBuyManager(1000)
	snap := sizeBuySnapshot()

	if got := m.SizeBuy("X", 100, snap, SizingConfig{Exits: pctExits(0.05)}, 0); !got.OK {
		t.Errorf("SizeBuy refused with no minimum configured: %+v", got)
	}

	got := m.SizeBuy("X", 100, snap,
		SizingConfig{Exits: pctExits(0.05), MinOrderSizeUSD: 1000}, 0)
	if !got.OK || got.Notional != 1000 {
		t.Errorf("SizeBuy = %+v, want 1000 approved (exactly at the minimum)", got)
	}
}

// No headroom is a refusal, not a zero-size order, and it says so — an
// operator seeing "no headroom" knows to look at the money limits rather than
// at the strategy.
func TestSizeBuyRefusesWithNoHeadroom(t *testing.T) {
	t.Parallel()

	m := New(config.Limits{MaxOrderValue: 1000, CashReserve: 1e9}, config.CostsConfig{})

	got := m.SizeBuy("X", 100, sizeBuySnapshot(), SizingConfig{Exits: pctExits(0.05)}, 0)
	if got.OK {
		t.Fatalf("SizeBuy = %+v, want a refusal when cash_reserve leaves nothing", got)
	}

	if !strings.Contains(got.Reason, "headroom") {
		t.Errorf("reason %q does not explain that the limits left no room", got.Reason)
	}
}

// A non-positive price cannot size anything and must not divide by zero.
func TestSizeBuyRefusesANonPositivePrice(t *testing.T) {
	t.Parallel()

	for _, price := range []float64{0, -5} {
		got := sizeBuyManager(1000).SizeBuy(
			"X", price, sizeBuySnapshot(), SizingConfig{Exits: pctExits(0.05)}, 0)
		if got.OK {
			t.Errorf("price %v was sized: %+v", price, got)
		}
	}
}

// The refusal must name the cause, because the two ways to reach it need
// different fixes. Risk scaling below the minimum means "this trade is too
// volatile to hold at size right now"; headroom below the minimum means "the
// account has no room for a worthwhile position at all", which is a money
// limit or a shrinking book, not the strategy. A single message blaming risk
// would send the operator to the wrong setting — and on a 1-minute bar feed,
// where the ATR stop can never exceed a percentage baseline and the ratio is
// always 1, the headroom case is the ONLY one that can occur.
func TestSizeBuyRefusalNamesTheCause(t *testing.T) {
	t.Parallel()

	snap := sizeBuySnapshot()

	// Risk did it: full headroom is 1000, the wider ATR stop halves it to 500.
	risky := SizingConfig{Exits: pctExits(0.05), MinOrderSizeUSD: 600}
	risky.Exits.ATRStopMult = 1

	got := sizeBuyManager(1000).SizeBuy("X", 100, snap, risky, 10)
	if got.OK || !strings.Contains(got.Reason, "risk-sized") {
		t.Errorf("a risk-scaled refusal reads %q, want it to blame risk", got.Reason)
	}

	// The limits did it: headroom is 50, no scaling involved at all.
	got = sizeBuyManager(50).SizeBuy("X", 100, snap,
		SizingConfig{Exits: pctExits(0.05), MinOrderSizeUSD: 600}, 0)
	if got.OK {
		t.Fatalf("SizeBuy = %+v, want a refusal", got)
	}

	if strings.Contains(got.Reason, "risk-sized") {
		t.Errorf("a headroom refusal blames risk: %q", got.Reason)
	}

	// And it must name WHICH limit. "The money limits leave 99.02" is true and
	// useless; the operator needs the line of config to go and look at.
	if !strings.Contains(got.Reason, "max_order_value") {
		t.Errorf("reason %q does not name the binding limit", got.Reason)
	}
}

// The refusal reason feeds engine.noteUnsizeable's announce-once dedupe, which
// keys on the reason string — so an identical situation must produce a
// byte-identical reason every time. It did not: the branch picking between
// "risk did this" and "the limits did this" compared notional against base,
// and with a percentage-only stop the ratio is stop_loss_pct/stop_loss_pct,
// which floating point renders as 0.9999999999999991 about as often as 1.
// The message alternated, the dedupe saw new information each evaluation, and
// production recorded a blocked trade — and sent a notification — roughly
// once a minute per symbol.
func TestSizeBuyRefusalIsStableAcrossPrices(t *testing.T) {
	t.Parallel()

	// Headroom below the minimum, percentage-only stop: no risk scaling can
	// apply, so every one of these must produce the same reason.
	m := sizeBuyManager(50)
	snap := sizeBuySnapshot()
	cfg := SizingConfig{Exits: pctExits(0.05), MinOrderSizeUSD: 600}

	seen := map[string]bool{}

	for _, price := range []float64{
		100, 99.02, 287.60, 159.074, 33.33, 1478.40, 17.39, 0.07,
	} {
		got := m.SizeBuy("X", price, snap, cfg, 0)
		if got.OK {
			t.Fatalf("price %v was sized: %+v", price, got)
		}

		seen[got.Reason] = true

		if strings.Contains(got.Reason, "risk-sized") {
			t.Errorf("price %v blamed risk with no ATR stop configured: %q", price, got.Reason)
		}
	}

	if len(seen) != 1 {
		t.Errorf("the same refusal produced %d different reasons, which defeats the "+
			"announce-once dedupe: %v", len(seen), seen)
	}
}

// A money limit written as a percentage must bind through Authorize exactly
// like the fixed amount it stands in for, and must track equity without
// anyone editing the file. This is the whole point of the percentage halves:
// position SIZING already scaled (the class caps and slots are fractions),
// and it was the absolute limits that pinned every order at a number chosen
// for an account size the operator has since grown out of.
func TestAuthorizeScalesPercentageLimitsWithEquity(t *testing.T) {
	t.Parallel()

	// 20% of equity per order, no fixed ceiling at all.
	m := New(config.Limits{
		MaxOrderValuePct:    0.20,
		MaxTotalInvestedPct: 0.95,
		CashReservePct:      0.05,
	}, config.CostsConfig{})

	for _, tc := range []struct{ equity, want float64 }{
		{1000, 200}, {5000, 1000}, {20000, 4000},
	} {
		snap := portfolio.Snapshot{Cash: tc.equity, Equity: tc.equity,
			Holdings: map[string]portfolio.Holding{}}

		got := m.BuyHeadroom(broker.Order{Symbol: "X", Side: broker.Buy}, 100, snap)
		if got != tc.want {
			t.Errorf("equity %v: headroom %v, want %v (20%%)", tc.equity, got, tc.want)
		}
	}
}

// With both halves set the tighter binds, so an absolute is a hard backstop a
// percentage cannot grow past — "20% of equity, but never more than 500".
func TestAuthorizeAbsoluteCapsThePercentage(t *testing.T) {
	t.Parallel()

	m := New(config.Limits{
		MaxOrderValue: 500, MaxOrderValuePct: 0.20,
		MaxTotalInvested: 1e9,
	}, config.CostsConfig{})

	snap := portfolio.Snapshot{Cash: 1e6, Equity: 1e6,
		Holdings: map[string]portfolio.Holding{}}

	if got := m.BuyHeadroom(broker.Order{Symbol: "X", Side: broker.Buy}, 100, snap); got != 500 {
		t.Errorf("headroom %v on a huge book, want the 500 absolute backstop", got)
	}
}

// cash_reserve_pct is a FLOOR, so it must grow the reserve rather than shrink
// it. Resolving it the way the ceilings resolve would take the smaller of the
// two and quietly leave a large account with a token reserve.
func TestCashReservePercentageGrowsTheFloor(t *testing.T) {
	t.Parallel()

	m := New(config.Limits{
		MaxOrderValue: 1e9, MaxTotalInvested: 1e9,
		CashReserve: 50, CashReservePct: 0.50,
	}, config.CostsConfig{})

	snap := portfolio.Snapshot{Cash: 1000, Equity: 1000,
		Holdings: map[string]portfolio.Holding{}}

	// 50% of 1000 is 500 reserved, leaving 500 spendable — not the 950 that
	// the fixed 50 alone would leave.
	if got := m.BuyHeadroom(broker.Order{Symbol: "X", Side: broker.Buy}, 100, snap); got != 500 {
		t.Errorf("headroom %v, want 500 — the larger (percentage) reserve must win", got)
	}
}
