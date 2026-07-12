package backtest

import (
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// TestBacktestWindowCoversTrendPeriod proves the backtester sizes its per-bar
// window to the strategy's full lookback (not the old ~100-bar warmup cap), so a
// long-lookback trend detector actually engages and its settings affect results.
//
// The data rises for 300 bars then goes flat for 100. The last 100 bars alone
// look like no trend, so a window capped at ~100 would never fire the trend; a
// window that spans the detector's 300-bar period sees the rise and votes BUY.
func TestBacktestWindowCoversTrendPeriod(t *testing.T) {
	const n = 400
	closes := make([]market.Candle, n)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		px := 100.0 + 0.1*float64(i)
		if i >= 300 {
			px = 100.0 + 0.1*300 // flat tail
		}
		closes[i] = market.Candle{
			Symbol: "TST", Open: px, High: px, Low: px, Close: px, Volume: 1,
			Time: base.Add(time.Duration(i) * time.Minute),
		}
	}
	data := map[string][]market.Candle{"TST": closes}

	cfg := config.StrategyConfig{
		Detectors: []string{"trend"}, Combine: "majority", MinStrength: 0.01,
		MinStrengthSell: -1, FastMA: 12, SlowMA: 26,
		TrendPeriod: 300, TrendThreshold: 0.01, TrendGate: false,
	}
	bt := &Backtester{
		Strategy: strategy.New(cfg),
		Limits:   config.Limits{MaxOrderValue: 1000, MaxPerPosition: 1000, MaxTotalInvested: 10000, MaxDailySpend: 10000, MaxOpenPositions: 10},
		Exits:    config.ExitsConfig{},
		Cash:     100000,
		OrderSize: 500,
	}
	res := bt.Run(data)
	if res.NumTrades == 0 {
		t.Fatal("trend detector never fired: backtest window did not cover its lookback period")
	}
}
