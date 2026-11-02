package backtest

import (
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// buyAtTimes buys on every bar whose time is listed — buyAt allows only one.
type buyAtTimes struct{ when []time.Time }

func (buyAtTimes) Name() string    { return "buyattimes" }
func (buyAtTimes) WarmupBars() int { return 1 }

func (b buyAtTimes) Evaluate(symbol string, candles []market.Candle) strategy.Signal {
	last := candles[len(candles)-1]

	action := strategy.Hold

	for _, w := range b.when {
		if last.Time.Equal(w) {
			action = strategy.Buy
		}
	}

	return strategy.Signal{Symbol: symbol, Action: action, Strength: 1, Price: last.Close}
}

// PR2-6, backtest side: a trade that sells half at +5% and stops the rest out
// at -2..-3% is a net winner. Judged on its last tranche alone it started a
// loss cooldown and refused the re-entry; judged as a round trip it must not.
// The live engine's closeTranche follows the same rule — this keeps the two
// from drifting.
func TestBacktestCooldownJudgesTheRoundTrip(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := flatSeries(base, 30, 100)

	for i := 4; i < 8; i++ { // +6%: the first rung sells half
		bars[i] = openBar(base.Add(time.Duration(i)*time.Minute), 106, 106, 106, 106)
	}

	for i := 8; i < 30; i++ { // -3%: the stop takes the rest
		bars[i] = openBar(base.Add(time.Duration(i)*time.Minute), 97, 97, 97, 97)
	}

	bt := &Backtester{
		Strategy: buyAtTimes{when: []time.Time{base, base.Add(20 * time.Minute)}},
		Limits:   testLimits(), Cash: 100000, OrderSize: 1000,
		Exits: config.ExitsConfig{Enabled: true, StopLossPct: 0.02, PartialTakeProfit: []config.PartialExit{
			{ProfitPct: 0.05, SellPct: 0.5},
		}},
		Protections: config.ProtectionsConfig{Enabled: true, CooldownAfterLoss: 24 * time.Hour},
	}

	res := bt.Run(data(bars))

	var net float64
	for _, rt := range res.RoundTrips {
		net += rt.PnL
	}

	if len(res.RoundTrips) < 2 || net <= 0 {
		t.Fatalf("fixture did not produce a winning two-tranche trade: %d tranches, net %.2f",
			len(res.RoundTrips), net)
	}

	buys := 0

	for _, tr := range res.Trades {
		if tr.Side == "buy" {
			buys++
		}
	}

	if buys != 2 {
		t.Errorf("%d buys, want 2: the winning round trip's losing last tranche started a cooldown", buys)
	}
}
