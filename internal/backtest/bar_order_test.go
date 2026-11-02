package backtest

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/engine"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// BT2-6: max_hold is judged from the age at the bar's start, so a barrier due
// on this bar was due at its open — where live exits on its first tick. The
// backtest held through the bar and filled at the close, or, as here, rode
// the position into an intrabar dive live never saw and booked a stop-loss.
func TestADueTimeBarrierFillsAtTheOpen(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := flatSeries(base, 12, 100)
	// Filled at bar 1's open; 5 minutes old at bar 6's open — due there.
	bars[6] = openBar(base.Add(6*time.Minute), 100, 100, 80, 90)

	bt := &Backtester{
		Strategy: &buyOnce{done: map[string]bool{}}, Limits: testLimits(),
		Cash: 100000, OrderSize: 1000,
		Exits: config.ExitsConfig{Enabled: true, StopLossPct: 0.05, MaxHold: 5 * time.Minute},
	}

	res := bt.Run(data(bars))

	for _, tr := range res.Trades {
		if tr.Side != "sell" {
			continue
		}

		if tr.Price != 100 || !hasReason(res, "max hold") {
			t.Errorf("exit at %.2f (%s), want the time barrier at the open, 100", tr.Price, tr.Reason)
		}
	}

	if countSells(res) != 1 {
		t.Fatalf("%d sells, want one", countSells(res))
	}
}

// BT2-7: on a falling bar the scale-in threshold (-3%) is crossed before the
// stop (-5%). Live adds there, and judges the stop against the re-based
// average (≈98.5 → stop ≈93.6), which a low of 94 does not reach. The
// backtest ran the exits first, stopped out on the old average at 95, and
// never added.
func TestAnAddAboveTheStopComesFirstOnAFallingBar(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Minute) }

	bars := []market.Candle{
		openBar(at(0), 100, 100, 100, 100), // decide: buy
		openBar(at(1), 100, 100, 100, 100), // filled at 100
		openBar(at(2), 100, 100, 94, 94.5), // through -3%, not through the re-based stop
		openBar(at(3), 94.5, 95, 94.2, 94.8),
	}

	bt := &Backtester{
		Strategy: &buyOnce{done: map[string]bool{}}, Limits: testLimits(),
		Cash: 100000, OrderSize: 1000,
		Exits:   config.ExitsConfig{Enabled: true, StopLossPct: 0.05},
		ScaleIn: config.ScaleInConfig{Enabled: true, MaxAdds: 1, StepPct: 0.03, SizePct: 1},
	}

	res := bt.Run(data(bars))

	buys := 0

	for _, tr := range res.Trades {
		if tr.Side == "buy" {
			buys++
		}
	}

	if buys != 2 || countSells(res) != 0 {
		t.Errorf("%d buys, %d sells; want the entry plus the add and no stop-out: %v",
			buys, countSells(res), reasons(res))
	}
}

// The other half of BT2-7's rule: when the open has already gapped through
// the stop, live's first tick runs checkExits before checkScaleIn and the
// position is gone. Adding first would re-base the stop below the bar and
// keep a doubled position the live bot never holds.
func TestAGapThroughTheStopExitsWithoutAdding(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Minute) }

	bars := []market.Candle{
		openBar(at(0), 100, 100, 100, 100),
		openBar(at(1), 100, 100, 100, 100),
		openBar(at(2), 94, 94.5, 93.5, 94), // opens below both -3% and the -5% stop
		openBar(at(3), 94, 94.5, 93.8, 94),
	}

	bt := &Backtester{
		Strategy: &buyOnce{done: map[string]bool{}}, Limits: testLimits(),
		Cash: 100000, OrderSize: 1000,
		Exits:   config.ExitsConfig{Enabled: true, StopLossPct: 0.05},
		ScaleIn: config.ScaleInConfig{Enabled: true, MaxAdds: 1, StepPct: 0.03, SizePct: 1},
	}

	res := bt.Run(data(bars))

	if !hasReason(res, "stop-loss") {
		t.Errorf("no stop-out on a gap through the stop: %v", reasons(res))
	}

	for _, tr := range res.Trades {
		if tr.Side == "buy" && tr.Time.After(at(1)) {
			t.Errorf("added into a gap through the stop: %s", tr.Reason)
		}
	}
}

// Found by the verify-change pass on BT2-7: with max_hold due at the bar's
// open, the add was made at the threshold INSIDE the bar and the whole
// position then sold at the bar's earlier open — a gain that cannot happen.
// Live's first tick runs checkExits before checkScaleIn: no add.
func TestADueTimeBarrierExitsAtTheOpenWithoutAddingFirst(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Minute) }

	bars := []market.Candle{
		openBar(at(0), 100, 100, 100, 100),
		openBar(at(1), 100, 100, 100, 100), // filled at 100
		openBar(at(2), 100, 100, 100, 100),
		openBar(at(3), 100, 100, 100, 100),
		openBar(at(4), 100, 100, 96, 97), // 3m old at the open; dips through -3%
		openBar(at(5), 97, 97, 97, 97),
	}

	bt := &Backtester{
		Strategy: &buyOnce{done: map[string]bool{}}, Limits: testLimits(),
		Cash: 100000, OrderSize: 1000,
		Exits:   config.ExitsConfig{Enabled: true, MaxHold: 3 * time.Minute},
		ScaleIn: config.ScaleInConfig{Enabled: true, MaxAdds: 1, StepPct: 0.03, SizePct: 1},
	}

	res := bt.Run(data(bars))

	for _, tr := range res.Trades {
		if tr.Side == "buy" && tr.Time.After(at(1)) {
			t.Errorf("added on the bar whose open closed the position: %s", tr.Reason)
		}
	}

	if !hasReason(res, "max hold") {
		t.Errorf("no time exit: %v", reasons(res))
	}
}

// EX3-2: exitFill tested the stop before the trail, so a bar through both
// filled at the DEEPER stop: stop 95, trail 98.94, low 94 booked 95 as a
// "stop-loss" where live sold at the trail on the way down — a 4% gap that
// biased every backtest, and autotune, against tight trails.
func TestABarThroughTrailAndStopFillsAtTheTrail(t *testing.T) {
	t.Parallel()

	cfg := config.ExitsConfig{Enabled: true, StopLossPct: 0.05, TrailingStopPct: 0.03}
	bt := &Backtester{Exits: cfg}

	fill, reason := bt.exitFill(market.Candle{Open: 101, High: 101, Low: 94, Close: 94}, 100, 102, 0, 0, 0)

	if want := 102 * 0.97; math.Abs(fill-want) > 1e-9 || !strings.HasPrefix(reason, "trailing-stop") {
		t.Errorf("filled at %.2f (%s), want the trail at %.2f", fill, reason, want)
	}
}

// BT3-1: after a scale-in the levels are rebuilt from the NEW, lower average,
// and the exit pass tested them against the bar's open and high — which came
// before the add. Bought at 100 with a 2% target, the bar 101/101/96/97 added
// at 97 (average 98.48, target 100.45) and then "sold" everything at the 101
// open: a profit on a price already gone. Live still holds.
func TestAnAddsLoweredTargetIsNotHitByThePreAddOpen(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Minute) }

	bars := []market.Candle{
		openBar(at(0), 100, 100, 100, 100),
		openBar(at(1), 100, 100, 100, 100), // filled at 100
		openBar(at(2), 101, 101, 96, 97),   // under the old target, then through -3%
		openBar(at(3), 97, 97.2, 96.8, 97),
	}

	bt := &Backtester{
		Strategy: &buyOnce{done: map[string]bool{}}, Limits: testLimits(),
		Cash: 100000, OrderSize: 1000,
		Exits:   config.ExitsConfig{Enabled: true, TakeProfitPct: 0.02},
		ScaleIn: config.ScaleInConfig{Enabled: true, MaxAdds: 1, StepPct: 0.03, SizePct: 1},
	}

	if res := bt.Run(data(bars)); countSells(res) > 0 {
		t.Errorf("sold on the add bar at a price that preceded the add: %v", reasons(res))
	}
}

// BT3-1, the ladder: the pre-add high of 101 cleared a rung re-based to 100.45.
func TestAnAddsLoweredRungIsNotHitByThePreAddHigh(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Minute) }

	bars := []market.Candle{
		openBar(at(0), 100, 100, 100, 100),
		openBar(at(1), 100, 100, 100, 100),
		openBar(at(2), 100, 101, 96, 97), // the rung at +2% of 100 is not reached; then -3%
		openBar(at(3), 97, 97.2, 96.8, 97),
	}

	bt := &Backtester{
		Strategy: &buyOnce{done: map[string]bool{}}, Limits: testLimits(),
		Cash: 100000, OrderSize: 1000,
		Exits: config.ExitsConfig{Enabled: true, PartialTakeProfit: []config.PartialExit{
			{ProfitPct: 0.02, SellPct: 0.5},
		}},
		ScaleIn: config.ScaleInConfig{Enabled: true, MaxAdds: 1, StepPct: 0.03, SizePct: 1},
	}

	if res := bt.Run(data(bars)); countSells(res) > 0 {
		t.Errorf("a scale-out rung fired on the add bar from the pre-add high: %v", reasons(res))
	}
}

// BT3-2: wins and losses were counted per sell fill, so a position with a
// two-rung ladder closed as THREE trades and three wins. Every min_trades gate
// read ClosedTrades — up to (rungs+1)x weaker than documented — and the
// win-rate objectives favoured ladders.
func TestAScaledOutPositionIsOneClosedTrade(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := make([]market.Candle, 12)

	for i := range bars {
		bars[i] = openBar(base.Add(time.Duration(i)*time.Minute), 100, 100.5, 99.5, 100)
	}

	bars[4] = openBar(base.Add(4*time.Minute), 100, 106, 100, 105)
	bars[6] = openBar(base.Add(6*time.Minute), 105, 121, 105, 120)
	bars[8] = openBar(base.Add(8*time.Minute), 120, 120, 100, 101)

	bt := &Backtester{
		Strategy: &buyOnce{done: map[string]bool{}}, Limits: testLimits(), Cash: 100000, OrderSize: 1000,
		Exits: config.ExitsConfig{
			Enabled: true, StopLossPct: 0.05, TrailingStopPct: 0.1,
			PartialTakeProfit: []config.PartialExit{{ProfitPct: 0.05, SellPct: 0.3}, {ProfitPct: 0.20, SellPct: 0.3}},
		},
	}

	res := bt.Run(data(bars))

	if len(res.RoundTrips) < 2 {
		t.Fatalf("the fixture did not scale out (%d sell fills)", len(res.RoundTrips))
	}

	if res.ClosedTrades != 1 || len(res.Trips) != 1 {
		t.Errorf("one position closed as ClosedTrades=%d, Trips=%d (wins %d), want 1", res.ClosedTrades, len(res.Trips), res.Wins)
	}
}

// BT3-3: live counts every add — a pyramid buy as much as a scale-in step —
// against scale_in.max_adds; the backtest counted only scale-in steps. With
// both on and max_adds 1, a pyramid buy used the one add live, while the
// backtest still added on the drawdown that followed.
func TestAPyramidBuyUsesTheScaleInAddsBudget(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Minute) }

	bars := flatSeries(base, 8, 100)
	for i := 4; i < 8; i++ {
		bars[i] = openBar(at(i), 94, 94, 94, 94) // -6%: the scale-in rung is due
	}

	bt := &Backtester{
		Strategy: buyAtTimes{when: []time.Time{at(0), at(1)}}, // entry, then a pyramid buy
		Limits:   testLimits(), Cash: 100000, OrderSize: 1000, AllowPyramiding: true,
		ScaleIn: config.ScaleInConfig{Enabled: true, MaxAdds: 1, StepPct: 0.03, SizePct: 1},
	}

	buys := 0

	for _, tr := range bt.Run(data(bars)).Trades {
		if tr.Side == "buy" {
			buys++
		}
	}

	if buys != 2 {
		t.Errorf("%d buys, want 2: the pyramid buy already took the one allowed add", buys)
	}
}

// longLookback is a strategy that asks for far more history than live keeps
// and records the most it was handed.
type longLookback struct{ most int }

func (*longLookback) Name() string     { return "long" }
func (*longLookback) WarmupBars() int  { return 1 }
func (*longLookback) MaxLookback() int { return 30000 }
func (l *longLookback) Evaluate(symbol string, candles []market.Candle) strategy.Signal {
	l.most = max(l.most, len(candles))

	return strategy.Signal{Symbol: symbol, Action: strategy.Hold}
}

// ST3-4: the backtest sized its per-bar window from the strategy's lookback —
// (trend_period+2) × htf_multiple, 29,280 bars at a multiple of 15 — while live
// keeps 3,000. The higher-timeframe trend regressed over ~1,950 aggregated
// bars in the backtest and ~200 live, and vetoed on a different trend.
func TestTheBacktestWindowNeverExceedsLiveHistory(t *testing.T) {
	t.Parallel()

	s := &longLookback{}
	bt := &Backtester{Strategy: s, Limits: testLimits(), Cash: 100000}

	bt.Run(data(flatSeries(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 4000, 100)))

	if s.most > engine.MaxHistoryBars {
		t.Errorf("the strategy was handed %d bars; live never holds more than %d", s.most, engine.MaxHistoryBars)
	}
}
