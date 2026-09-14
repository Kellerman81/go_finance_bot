package backtest

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// alwaysBuy is a strategy that buys on every bar once warm, so the fill
// mechanics can be tested without depending on any indicator's behaviour.
type alwaysBuy struct{ action strategy.Action }

// Name identifies the stub.
func (alwaysBuy) Name() string { return "always" }

// WarmupBars needs no history.
func (alwaysBuy) WarmupBars() int { return 1 }

// Evaluate returns the fixed action at full strength.
func (s alwaysBuy) Evaluate(symbol string, candles []market.Candle) strategy.Signal {
	px := 0.0
	if len(candles) > 0 {
		px = candles[len(candles)-1].Close
	}

	return strategy.Signal{
		Symbol: symbol, Action: s.action, Strength: 1, Price: px, Reason: "stub",
	}
}

// openBar returns a candle with an explicit OHLC.
func openBar(at time.Time, o, h, l, c float64) market.Candle {
	return market.Candle{Symbol: "TST", Time: at, Open: o, High: h, Low: l, Close: c, Volume: 100}
}

// testLimits are permissive money limits, so the limit clamp never masks what a
// fill test is measuring.
func testLimits() config.Limits {
	return config.Limits{
		MaxOrderValue: 1e9, MaxPerPosition: 1e9, MaxTotalInvested: 1e9,
		MaxDailySpend: 1e9, MaxOpenPositions: 10,
	}
}

// TestFillsAtNextBarOpen proves the look-ahead is gone: a decision taken on a
// bar's close is filled at the *next* bar's open, never at the close it was
// computed from.
func TestFillsAtNextBarOpen(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Bar 0 closes at 100; bar 1 opens at 130. Filling at the signal bar's close
	// would buy at 100 — the price that had already gone when the signal existed.
	data := map[string][]market.Candle{"TST": {
		openBar(base, 100, 100, 100, 100),
		openBar(base.Add(time.Minute), 130, 130, 130, 130),
		openBar(base.Add(2*time.Minute), 130, 130, 130, 130),
	}}

	bt := &Backtester{
		Strategy: alwaysBuy{action: strategy.Buy}, Limits: testLimits(),
		Cash: 100000, OrderSize: 1000,
	}

	res := bt.Run(data)
	if res.NumTrades == 0 {
		t.Fatal("no trades")
	}

	if got := res.Trades[0].Price; got != 130 {
		t.Errorf("first fill price = %v, want 130 (the next bar's open, not the signal close)", got)
	}

	if res.Trades[0].Time != base.Add(time.Minute) {
		t.Errorf("fill time = %v, want the next bar's timestamp", res.Trades[0].Time)
	}
}

// TestIntrabarStopFires proves a stop pierced during a bar is hit even when the
// bar closes back above it. Testing exits against the close alone silently
// deletes losing trades from the record.
func TestIntrabarStopFires(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Buy at bar 1's open (100). Bar 2 dips to 88 — through the 10% stop at 90 —
	// but closes back at 99, so a close-only check would miss it entirely.
	data := map[string][]market.Candle{"TST": {
		openBar(base, 100, 100, 100, 100),
		openBar(base.Add(time.Minute), 100, 101, 99, 100),
		openBar(base.Add(2*time.Minute), 100, 101, 88, 99),
		openBar(base.Add(3*time.Minute), 99, 100, 98, 99),
	}}

	bt := &Backtester{
		Strategy: alwaysBuy{action: strategy.Buy}, Limits: testLimits(),
		Exits: config.ExitsConfig{Enabled: true, StopLossPct: 0.10},
		Cash:  100000, OrderSize: 1000,
	}

	res := bt.Run(data)

	var stopped *struct{ price float64 }

	for _, tr := range res.Trades {
		if tr.Side == "sell" {
			stopped = &struct{ price float64 }{tr.Price}
			break
		}
	}

	if stopped == nil {
		t.Fatal("no stop-loss sell: the intrabar low never triggered the exit")
	}

	if math.Abs(stopped.price-90) > 0.01 {
		t.Errorf("stop fill price = %v, want ~90 (the stop level, reached intrabar)", stopped.price)
	}
}

// TestStopGapsThroughLevel proves a market that opens below the stop fills at
// that open, not at the stop price — you cannot get out at a level the market
// jumped over.
func TestStopGapsThroughLevel(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Entry at 100 on bar 1's open, stop at 90; bar 2 opens at 80 (gap down).
	data := map[string][]market.Candle{"TST": {
		openBar(base, 100, 100, 100, 100),
		openBar(base.Add(time.Minute), 100, 101, 99, 100),
		openBar(base.Add(2*time.Minute), 80, 82, 78, 81),
		openBar(base.Add(3*time.Minute), 81, 82, 80, 81),
	}}

	bt := &Backtester{
		Strategy: alwaysBuy{action: strategy.Buy}, Limits: testLimits(),
		Exits: config.ExitsConfig{Enabled: true, StopLossPct: 0.10},
		Cash:  100000, OrderSize: 1000,
	}

	res := bt.Run(data)

	for _, tr := range res.Trades {
		if tr.Side == "sell" {
			if math.Abs(tr.Price-80) > 0.01 {
				t.Errorf("gap-down stop filled at %v, want 80 (the open it gapped to)", tr.Price)
			}

			return
		}
	}

	t.Fatal("no stop-loss sell on the gap-down bar")
}

// TestSlippageWorsensBothSides proves spread and slippage are paid on entry and
// exit: the same round trip nets less than a frictionless one.
func TestSlippageWorsensBothSides(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := make([]market.Candle, 6)

	for i := range bars {
		px := 100.0
		bars[i] = openBar(base.Add(time.Duration(i)*time.Minute), px, px, px, px)
	}

	data := map[string][]market.Candle{"TST": bars}

	run := func(costs config.CostsConfig) Result {
		bt := &Backtester{
			Strategy: alwaysBuy{action: strategy.Buy}, Limits: testLimits(),
			Costs: costs, Cash: 100000, OrderSize: 1000,
		}

		return bt.Run(data)
	}

	free := run(config.CostsConfig{})
	// 20 bps spread + 10 bps slippage = 20 bps adverse per fill.
	costly := run(config.CostsConfig{SpreadPct: 0.002, SlippagePct: 0.001})

	if free.NumTrades == 0 || costly.NumTrades == 0 {
		t.Fatal("no trades in one of the runs")
	}

	if costly.Trades[0].Price <= free.Trades[0].Price {
		t.Errorf("buy filled at %v with costs vs %v without; a buy must pay up",
			costly.Trades[0].Price, free.Trades[0].Price)
	}

	if costly.FinalEquity >= free.FinalEquity {
		t.Errorf("equity with costs %.2f >= frictionless %.2f; trading friction must cost money",
			costly.FinalEquity, free.FinalEquity)
	}
}

// TestFillPriceDirections checks the cost model moves each side the right way.
func TestFillPriceDirections(t *testing.T) {
	t.Parallel()

	c := config.CostsConfig{SpreadPct: 0.001, SlippagePct: 0.0005} // 10 bps adverse

	if got := c.FillPrice(true, 100); math.Abs(got-100.10) > 1e-9 {
		t.Errorf("buy fill = %v, want 100.10", got)
	}

	if got := c.FillPrice(false, 100); math.Abs(got-99.90) > 1e-9 {
		t.Errorf("sell fill = %v, want 99.90", got)
	}

	if got := (config.CostsConfig{}).FillPrice(true, 100); got != 100 {
		t.Errorf("frictionless fill = %v, want 100 unchanged", got)
	}
}

// noopRiskManager returns a risk.Manager with headroom nobody in these tests
// should hit (base is always > 0 in them, so BuyHeadroom is never actually
// consulted) and an empty snapshot, so call sites that don't care about the
// headroom fallback can pass something valid without constructing it by hand
// every time.
func noopRiskManager() (*risk.Manager, portfolio.Snapshot) {
	rm := risk.New(config.Limits{
		MaxOrderValue: 1e9, MaxPerPosition: 1e9, MaxTotalInvested: 1e9, CashReserve: 0,
	}, config.CostsConfig{})

	return rm, portfolio.Snapshot{Cash: 1e9}
}

// TestROILadderExitsInBacktest proves the ladder is wired end to end: a position
// that never reaches the fixed take-profit still exits once the ladder decays to
// a threshold it has cleared.
func TestROILadderExitsInBacktest(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Enters at 100 on bar 1's open, then drifts up ~3% and sits there. A 20%
	// take-profit is never reached; the 2%-after-2h rung is.
	bars := []market.Candle{
		openBar(base, 100, 100, 100, 100),
		openBar(base.Add(time.Minute), 100, 100, 100, 100),
	}

	for i := 2; i < 200; i++ {
		px := 103.0
		bars = append(bars, openBar(base.Add(time.Duration(i)*time.Minute), px, px, px, px))
	}

	data := map[string][]market.Candle{"TST": bars}

	withLadder := &Backtester{
		Strategy: alwaysBuy{action: strategy.Buy}, Limits: testLimits(),
		Exits: config.ExitsConfig{
			Enabled: true, TakeProfitPct: 0.20,
			ROI: map[string]float64{"0s": 0.20, "2h": 0.02},
		},
		Cash: 100000, OrderSize: 1000,
	}

	res := withLadder.Run(data)

	var exited bool

	for _, tr := range res.Trades {
		if tr.Side == "sell" && strings.Contains(tr.Reason, "roi") {
			exited = true
			break
		}
	}

	if !exited {
		t.Error("the ROI ladder never took profit on a position held past its 2h rung")
	}
}

// TestMetricsTradeQuality checks profit factor, expectancy and exposure are
// computed from the closed round trips rather than left at zero.
func TestMetricsTradeQuality(t *testing.T) {
	t.Parallel()

	if pf := profitFactor([]float64{100, -50, 25}); math.Abs(pf-2.5) > 1e-9 {
		t.Errorf("profit factor = %v, want 2.5", pf)
	}

	if pf := profitFactor([]float64{100, 25}); pf != 0 {
		t.Errorf("profit factor with no losses = %v, want 0 (not infinity)", pf)
	}

	exp, win, loss := tradeQuality([]float64{10, -4, 6})
	if math.Abs(exp-4) > 1e-9 {
		t.Errorf("expectancy = %v, want 4", exp)
	}

	if math.Abs(win-8) > 1e-9 || math.Abs(loss+4) > 1e-9 {
		t.Errorf("avg win/loss = %v/%v, want 8/-4", win, loss)
	}
}

// TestAnnualisedRatios checks the ratios scale with the observed bar spacing, so
// a minute strategy and a daily one are comparable.
func TestAnnualisedRatios(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	daily := make([]EquityPoint, 260)
	equity := 100000.0

	for i := range daily {
		// Alternating gain/loss with a positive drift.
		if i%2 == 0 {
			equity *= 1.004
		} else {
			equity *= 0.998
		}

		daily[i] = EquityPoint{Time: base.AddDate(0, 0, i), Equity: equity}
	}

	if pk := periodsPerYear(daily); pk < 200 || pk > 400 {
		t.Errorf("periods per year = %v, want ~365 for daily bars", pk)
	}

	rets := barReturns(daily)

	sharpe := annualisedRatio(rets, periodsPerYear(daily), false)
	sortino := annualisedRatio(rets, periodsPerYear(daily), true)

	if sharpe <= 0 || sortino <= 0 {
		t.Fatalf("ratios = sharpe %v sortino %v, want both positive for a rising curve",
			sharpe, sortino)
	}

	if sortino <= sharpe {
		t.Errorf("sortino %v <= sharpe %v; ignoring upside deviation should raise it",
			sortino, sharpe)
	}

	if annualisedRatio(nil, 252, false) != 0 {
		t.Error("empty return series must score 0, not NaN")
	}
}
