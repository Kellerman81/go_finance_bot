package backtest

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// flatSeries builds n bars all at price, so an exit test measures the exit rule
// rather than the price path.
func flatSeries(base time.Time, n int, price float64) []market.Candle {
	out := make([]market.Candle, n)
	for i := range out {
		out[i] = openBar(base.Add(time.Duration(i)*time.Minute), price, price, price, price)
	}

	return out
}

// buyOnce buys on the first evaluation and holds forever after, so exactly one
// position exists to test the exits against.
type buyOnce struct{ done map[string]bool }

// Name identifies the stub.
func (buyOnce) Name() string { return "buyonce" }

// WarmupBars needs no history.
func (buyOnce) WarmupBars() int { return 1 }

// Evaluate returns BUY the first time it sees a symbol, HOLD thereafter.
func (s *buyOnce) Evaluate(symbol string, candles []market.Candle) strategy.Signal {
	px := 0.0
	if len(candles) > 0 {
		px = candles[len(candles)-1].Close
	}

	action := strategy.Hold
	if !s.done[symbol] {
		s.done[symbol] = true
		action = strategy.Buy
	}

	return strategy.Signal{Symbol: symbol, Action: action, Strength: 1, Price: px}
}

// ----- item 2: the time barrier, in the backtest -----

// A position that neither wins nor loses enough to trip a price rule sits there
// forever without max_hold, and leaves at the barrier with it.
func TestMaxHoldClosesADeadPosition(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Flat at 100 apart from a small dip — never enough for a 20% stop or a 20%
	// target, so nothing but the clock can end this trade.
	data := map[string][]market.Candle{"TST": flatSeries(base, 40, 100)}

	exitsCfg := config.ExitsConfig{Enabled: true, StopLossPct: 0.20, TakeProfitPct: 0.20}

	without := &Backtester{
		Strategy: &buyOnce{done: map[string]bool{}}, Limits: testLimits(),
		Cash: 100000, OrderSize: 1000, Exits: exitsCfg,
	}
	if got := countSells(without.Run(data)); got != 0 {
		t.Fatalf("without max_hold the dead position was closed %d time(s)", got)
	}

	exitsCfg.MaxHold = 10 * time.Minute
	with := &Backtester{
		Strategy: &buyOnce{done: map[string]bool{}}, Limits: testLimits(),
		Cash: 100000, OrderSize: 1000, Exits: exitsCfg,
	}

	res := with.Run(data)
	if countSells(res) == 0 {
		t.Fatal("max_hold did not close the position")
	}

	if !hasReason(res, "max hold") {
		t.Errorf("no trade names the time barrier; reasons: %v", reasons(res))
	}
}

// A stop breached in the same bar the clock runs out must report the stop: that
// is the difference between filling at the stop price and at the close.
func TestPriceExitOutranksMaxHoldInTheBacktest(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := flatSeries(base, 12, 100)
	// The buy is decided on bar 0's close and fills at bar 1's open, so the
	// position is 5 minutes old on bar 6 — where it both dives through a 5% stop
	// and reaches max_hold. Both barriers break on the same bar.
	bars[6] = openBar(base.Add(6*time.Minute), 100, 100, 80, 90)

	bt := &Backtester{
		Strategy: &buyOnce{done: map[string]bool{}}, Limits: testLimits(),
		Cash: 100000, OrderSize: 1000,
		Exits: config.ExitsConfig{
			Enabled: true, StopLossPct: 0.05, MaxHold: 5 * time.Minute,
		},
	}

	res := bt.Run(data(bars))
	if !hasReason(res, "stop-loss") {
		t.Fatalf("expected a stop-loss exit; reasons: %v", reasons(res))
	}
}

// ----- item 1: the two-stage trail, in the backtest -----

// The tight trail exits a pullback that the loose one would have ridden out.
func TestTightTrailExitsSoonerAfterTheOffset(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := flatSeries(base, 14, 100)
	bars[4] = openBar(base.Add(4*time.Minute), 100, 110, 100, 110) // +10%, past a 5% offset
	bars[5] = openBar(base.Add(5*time.Minute), 110, 110, 106, 106) // -3.6% off the peak

	loose := config.ExitsConfig{Enabled: true, TrailingStopPct: 0.10}
	staged := config.ExitsConfig{
		Enabled: true, TrailingStopPct: 0.10,
		TrailingOffsetPct: 0.05, TrailingTightPct: 0.02,
	}

	looseRes := (&Backtester{
		Strategy: &buyOnce{done: map[string]bool{}}, Limits: testLimits(),
		Cash: 100000, OrderSize: 1000, Exits: loose,
	}).Run(data(bars))

	stagedRes := (&Backtester{
		Strategy: &buyOnce{done: map[string]bool{}}, Limits: testLimits(),
		Cash: 100000, OrderSize: 1000, Exits: staged,
	}).Run(data(bars))

	if countSells(looseRes) != 0 {
		t.Fatalf("a 10%% trail should have survived a 3.6%% pullback; reasons: %v",
			reasons(looseRes))
	}

	if countSells(stagedRes) == 0 {
		t.Fatalf("the 2%% tight trail did not fire; reasons: %v", reasons(stagedRes))
	}
}

// ----- item 3: the scale-out ladder, in the backtest -----

// A rung sells part of the position and leaves the rest running.
func TestLadderSellsATrancheAndKeepsTheRest(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := flatSeries(base, 16, 100)

	for i := 4; i < 16; i++ { // up 6%, clearing the first rung only
		bars[i] = openBar(base.Add(time.Duration(i)*time.Minute), 106, 106, 106, 106)
	}

	bt := &Backtester{
		Strategy: &buyOnce{done: map[string]bool{}}, Limits: testLimits(),
		Cash: 100000, OrderSize: 1000,
		Exits: config.ExitsConfig{Enabled: true, PartialTakeProfit: []config.PartialExit{
			{ProfitPct: 0.05, SellPct: 0.5},
			{ProfitPct: 0.20, SellPct: 0.5},
		}},
	}

	res := bt.Run(data(bars))
	if !hasReason(res, "partial take-profit") {
		t.Fatalf("no scale-out happened; reasons: %v", reasons(res))
	}

	// Exactly one rung is due, and it must fire once — not on every subsequent bar.
	if got := countSells(res); got != 1 {
		t.Errorf("scale-out fired %d times; each rung fires once per position", got)
	}

	// Half the position was sold, so the trade log's sell quantity is half the buy.
	var buyQty, sellQty float64

	for _, tr := range res.Trades {
		if strings.Contains(tr.Reason, "partial") {
			sellQty += tr.Qty
		} else if tr.Side == "buy" {
			buyQty += tr.Qty
		}
	}

	if buyQty <= 0 || sellQty <= 0 || sellQty > buyQty*0.51 || sellQty < buyQty*0.49 {
		t.Errorf("sold %v of %v; the 0.5 rung should sell half the original size",
			sellQty, buyQty)
	}
}

// TestScaleOutDoesNotRechargeTheEntryCommissionPerRung reproduces the
// "Round-trip P&L re-charges the entry commission on every scale-out rung"
// finding: each RoundTrip used to compute its cost as
// Commission(exit notional) + Commission(avgCost*rungQty), recomputing a
// synthetic per-rung "entry commission" from scratch instead of tracking
// what the sim broker actually charged once at buy time. With a flat/minimum
// commission component, that recomputation re-applies the flat fee once per
// rung — for a 2-rung ladder, that is one whole extra flat fee that was never
// really charged, a structural double-charge disagreeing with the real cash
// ledger (not float drift).
//
// The ladder's rungs fill at their own threshold price (see partialFill), so
// the gross (pre-commission) P&L per rung is fully determined by the
// configured thresholds — letting this test compute the exact expected
// Σ RoundTrip.PnL from first principles and distinguish the correct number
// from the old buggy one, rather than merely asserting "not equal to the old
// value".
func TestScaleOutDoesNotRechargeTheEntryCommissionPerRung(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := flatSeries(base, 20, 100)

	// Each bar's Open stays at the prior level and only its High spikes, so
	// the rung fills at its own resting price (a gap-up open would instead
	// fill at the (better) open price — see gapAdjusted — which is not what
	// this test wants to isolate).
	bars[4] = openBar(base.Add(4*time.Minute), 100, 106, 100, 106) // clears rung 1 (+5%) only

	for i := 5; i < 11; i++ {
		bars[i] = openBar(base.Add(time.Duration(i)*time.Minute), 106, 106, 106, 106)
	}

	bars[11] = openBar(base.Add(11*time.Minute), 106, 125, 106, 125) // clears rung 2 (+20%), fully closing

	for i := 12; i < 20; i++ {
		bars[i] = openBar(base.Add(time.Duration(i)*time.Minute), 125, 125, 125, 125)
	}

	const flatFee = 5.0

	bt := &Backtester{
		Strategy: &buyOnce{done: map[string]bool{}}, Limits: testLimits(),
		Cash: 100000, OrderSize: 1000, // buys 10 shares @ 100
		Costs: config.CostsConfig{CommissionFlat: flatFee},
		Exits: config.ExitsConfig{Enabled: true, PartialTakeProfit: []config.PartialExit{
			{ProfitPct: 0.05, SellPct: 0.5},
			{ProfitPct: 0.20, SellPct: 0.5},
		}},
	}

	res := bt.Run(data(bars))

	if len(res.RoundTrips) != 2 {
		t.Fatalf("expected exactly 2 round trips (one per rung), got %d: %+v",
			len(res.RoundTrips), res.RoundTrips)
	}

	// Rungs fill at entry*(1+threshold): 105 for rung 1, 120 for rung 2. Each
	// sells half of the 10-share position (5 shares), entered at 100.
	wantGross := (105-100)*5.0 + (120-100)*5.0 // = 25 + 100 = 125

	// Exactly what the sim broker really charges: one buy fill, two sell
	// fills, each a flat $5 — never a fourth "phantom" flat fee for a
	// synthetic per-rung entry recomputation.
	wantCost := 3 * flatFee // = 15
	wantPnL := wantGross - wantCost

	var gotPnL float64
	for _, tr := range res.RoundTrips {
		gotPnL += tr.PnL
	}

	if math.Abs(gotPnL-wantPnL) > 1e-6 {
		t.Errorf("sum(RoundTrip.PnL) = %.4f, want %.4f (the old code would give %.4f, "+
			"one flat fee too many)", gotPnL, wantPnL, wantGross-4*flatFee)
	}
}

// TestTrailingStopDoesNotRatchetOnItsOwnTestBar reproduces the "the trailing
// stop ratchets on the same bar's high and then fills at the ratcheted
// level" look-ahead finding. OHLC data does not say whether a bar's high or
// its low came first; using this bar's own high to raise the peak and then
// testing this SAME bar's low against the raised trail always assumes
// "high, then low" — crediting a ratchet the position may never actually
// have reached before the low that supposedly breached it. A bar that
// spikes well past the trail offset and then dips only moderately must not
// exit on that bar: the trail available to test against is whatever the
// PRIOR bars established, not this bar's own high.
func TestTrailingStopDoesNotRatchetOnItsOwnTestBar(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := flatSeries(base, 10, 100)

	// Entered at 100 (bars 0-3 flat), no rise before this bar, so the trail
	// available going in is 100*(1-0.10) = 90. This bar spikes to a high of
	// 130 — which, ratcheted into the peak same-bar, would raise the trail
	// to 130*0.9 = 117 — and dips to a low of 95: below the ratcheted 117,
	// but well above the pre-bar 90.
	bars[4] = openBar(base.Add(4*time.Minute), 100, 130, 95, 100)

	// Bars after: held at 120, deliberately ABOVE the 117 trail that bar 4's
	// high of 130 correctly ratchets in for bars from here on (130*0.9 =
	// 117) — so the run only tells us something about bar 4 itself. Flat at
	// (or below) 100, as an earlier version of this test used, would trip
	// that now-legitimate 117 trail on the very next bar regardless of
	// whether bar 4 itself looked ahead, proving nothing.
	for i := 5; i < 10; i++ {
		bars[i] = openBar(base.Add(time.Duration(i)*time.Minute), 120, 120, 120, 120)
	}

	bt := &Backtester{
		Strategy: &buyOnce{done: map[string]bool{}}, Limits: testLimits(),
		Cash: 100000, OrderSize: 1000,
		Exits: config.ExitsConfig{Enabled: true, TrailingStopPct: 0.10},
	}

	res := bt.Run(data(bars))

	if hasReason(res, "trailing-stop") {
		t.Fatalf("a trailing-stop exit fired using this bar's own high as its "+
			"peak (look-ahead); reasons: %v", reasons(res))
	}

	if countSells(res) != 0 {
		t.Errorf("expected the position still open (no exit condition legitimately "+
			"met using only the pre-bar peak), but got %d sell(s): %v",
			countSells(res), reasons(res))
	}
}

// The peak must still ratchet for LATER bars — only the same-bar look-ahead
// is removed, not the trail itself.
func TestTrailingStopPeakStillRatchetsForLaterBars(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := flatSeries(base, 10, 100)

	bars[4] = openBar(base.Add(4*time.Minute), 100, 130, 95, 100) // establishes a 130 peak
	// The trail off a 130 peak is 130*0.9 = 117: this bar's low breaches it.
	bars[5] = openBar(base.Add(5*time.Minute), 100, 100, 110, 110)

	bt := &Backtester{
		Strategy: &buyOnce{done: map[string]bool{}}, Limits: testLimits(),
		Cash: 100000, OrderSize: 1000,
		Exits: config.ExitsConfig{Enabled: true, TrailingStopPct: 0.10},
	}

	res := bt.Run(data(bars))

	if !hasReason(res, "trailing-stop") {
		t.Fatalf("the 130 peak established on bar 4 did not carry forward to bar 5; "+
			"reasons: %v", reasons(res))
	}
}

// TestScaleOutGapPastTwoRungsDoesNotResellTheLowerOne reproduces the
// "scale-out ladder advances one rung per fill in backtest, the highest rung
// hit live" finding: exits.NextPartial (shared by both) correctly combines
// every newly-satisfied rung into one Fraction when a bar's high gaps past
// more than one threshold at once — but the backtest used to advance its
// "taken" counter by only one (partials[sym]++) regardless of how many
// rungs the combined fill actually consumed, while the live engine sets
// PartialsTaken = rung + 1 (see engine/exits.go). Left under-advanced, the
// already-sold lower rung stayed "due": the next bar at the same gain level
// would report it due again and sell part of the position a second time —
// selling more of the position than the ladder configured, and doing so
// sooner than the live engine, which correctly holds the remainder until a
// genuinely new rung is reached.
func TestScaleOutGapPastTwoRungsDoesNotResellTheLowerOne(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := flatSeries(base, 10, 100)

	// Gaps straight past both rungs (5% and 10%) to 15% in one bar — a
	// single combined fill selling 30%+30% = 60% is the correct outcome.
	bars[4] = openBar(base.Add(4*time.Minute), 100, 115, 100, 115)

	// Gain stays at 15% — both rungs' thresholds remain satisfied. A well
	// behaved ladder does NOT sell again here: both configured rungs were
	// already consumed by bar 4's combined fill.
	for i := 5; i < 10; i++ {
		bars[i] = openBar(base.Add(time.Duration(i)*time.Minute), 115, 115, 115, 115)
	}

	bt := &Backtester{
		Strategy: &buyOnce{done: map[string]bool{}}, Limits: testLimits(),
		Cash: 100000, OrderSize: 1000,
		Exits: config.ExitsConfig{Enabled: true, PartialTakeProfit: []config.PartialExit{
			{ProfitPct: 0.05, SellPct: 0.3},
			{ProfitPct: 0.10, SellPct: 0.3},
		}},
	}

	res := bt.Run(data(bars))

	partials := 0

	var soldQty float64

	for _, tr := range res.Trades {
		if strings.Contains(tr.Reason, "partial") {
			partials++

			soldQty += tr.Qty
		}
	}

	if partials != 1 {
		t.Fatalf("the ladder fired %d partial-take-profit trade(s), want exactly 1 "+
			"(a single combined fill for both rungs): %v", partials, reasons(res))
	}

	// 60% of the 10-share position (30%+30%), not more.
	if soldQty < 5.9 || soldQty > 6.1 {
		t.Errorf("sold %.4f shares, want ~6.0 (60%% of 10) — not the full 30%%-then-30%%-again "+
			"double-sell the under-advanced taken counter used to cause", soldQty)
	}
}

// ----- item 5: per-symbol measured spread -----

// Charging each instrument its own measured spread must change the result
// relative to charging one number to everything.
func TestMeasuredSpreadChangesTheCostCharged(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := make([]market.Candle, 40)

	// A wide intrabar range, which is what the estimator reads a spread from.
	for i := range bars {
		bars[i] = openBar(base.Add(time.Duration(i)*time.Minute), 100, 103, 97, 100)
	}

	flat := &Backtester{
		Strategy: alwaysBuy{action: strategy.Buy}, Limits: testLimits(),
		Cash: 100000, OrderSize: 1000,
		Costs: config.CostsConfig{SpreadPct: 0.0001},
	}
	measured := &Backtester{
		Strategy: alwaysBuy{action: strategy.Buy}, Limits: testLimits(),
		Cash: 100000, OrderSize: 1000,
		Costs: config.CostsConfig{SpreadPct: 0.0001, MeasureSpread: true},
	}

	flatRes := flat.Run(data(bars))
	measuredRes := measured.Run(data(bars))

	if measuredRes.FinalEquity >= flatRes.FinalEquity {
		t.Errorf("measured spread (%.2f) was not costlier than the 1bp guess (%.2f); "+
			"a 6%% intrabar range should estimate well above 1bp",
			measuredRes.FinalEquity, flatRes.FinalEquity)
	}
}

// The configured spread stays a floor: an estimate below it must not make a
// symbol look cheaper to trade than it was budgeted for.
func TestConfiguredSpreadIsAFloor(t *testing.T) {
	t.Parallel()

	c := config.CostsConfig{SpreadPct: 0.01, MeasureSpread: true}
	if got := c.SpreadFor(0.0001); got != 0.01 {
		t.Errorf("SpreadFor(1bp) = %v, want the configured 1%% floor", got)
	}

	if got := c.SpreadFor(0.02); got != 0.02 {
		t.Errorf("SpreadFor(2%%) = %v, want the higher measured value", got)
	}

	off := config.CostsConfig{SpreadPct: 0.01}
	if got := off.SpreadFor(0.05); got != 0.01 {
		t.Errorf("with measure_spread off, SpreadFor = %v, want the configured 0.01", got)
	}
}

// ----- helpers -----

// data wraps one series as the single-symbol dataset Run takes.
func data(bars []market.Candle) map[string][]market.Candle {
	return map[string][]market.Candle{"TST": bars}
}

// countSells counts filled sell trades.
func countSells(r Result) int {
	n := 0

	for _, tr := range r.Trades {
		if tr.Side == "sell" {
			n++
		}
	}

	return n
}

// hasReason reports whether any trade's reason contains want.
func hasReason(r Result, want string) bool {
	for _, tr := range r.Trades {
		if strings.Contains(tr.Reason, want) {
			return true
		}
	}

	return false
}

// reasons lists every trade reason, for failure messages.
func reasons(r Result) []string {
	out := make([]string, 0, len(r.Trades))
	for _, tr := range r.Trades {
		out = append(out, string(tr.Side)+": "+tr.Reason)
	}

	return out
}
