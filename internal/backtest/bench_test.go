package backtest

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// benchData builds a deterministic random walk: the shape of the work, without
// the shape of any particular market.
func benchData(symbols, bars int) map[string][]market.Candle {
	rng := rand.New(rand.NewPCG(7, 11)) //nolint:gosec // deterministic benchmark data
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	out := make(map[string][]market.Candle, symbols)

	for s := range symbols {
		sym := string(rune('A' + s))
		series := make([]market.Candle, bars)
		price := 100.0

		for i := range series {
			price *= 1 + (rng.Float64()-0.5)*0.01

			half := price * 0.004
			series[i] = market.Candle{
				Symbol: sym,
				Time:   base.Add(time.Duration(i) * time.Minute),
				Open:   price,
				High:   price + half,
				Low:    price - half,
				Close:  price,
				Volume: 10000 + rng.Float64()*5000,
			}
		}

		out[sym] = series
	}

	return out
}

// benchStrategy is the detector set a real config runs, so the benchmark
// measures the work the optimizer actually pays for rather than a toy.
func benchStrategy() strategy.Strategy {
	return strategy.New(config.StrategyConfig{
		Detectors: []string{
			"ema_cross", "rsi", "macd", "bollinger", "adx",
			"stochastic", "supertrend", "donchian", "trend",
		},
		Combine:     "weighted",
		MinStrength: 0.2,
		FastMA:      12, SlowMA: 26,
		RSIPeriod: 14, RSIOverbought: 70, RSIOversold: 30,
		MACDFast: 12, MACDSlow: 26, MACDSignal: 9,
		BBPeriod: 20, BBStdDev: 2,
		ADXPeriod: 14, ADXThreshold: 25,
		StochKPeriod: 14, StochDPeriod: 3, StochOverbought: 80, StochOversold: 20,
		SupertrendPeriod: 10, SupertrendMult: 3,
		DonchianPeriod: 20,
		TrendPeriod:    400, TrendThreshold: 0.01,
		ATRPeriod: 14,
	})
}

// BenchmarkEvaluate measures one strategy evaluation over a full lookback
// window — the innermost call of both the live loop and the backtester.
func BenchmarkEvaluate(b *testing.B) {
	data := benchData(1, 1000)["A"]
	strat := benchStrategy()

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		strat.Evaluate("A", data)
	}
}

// BenchmarkRun measures a whole backtest. The optimizer runs thousands of these,
// so allocation here is multiplied by the size of the search.
func BenchmarkRun(b *testing.B) {
	data := benchData(4, 1200)

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		bt := &Backtester{
			Strategy:  benchStrategy(),
			Limits:    testLimits(),
			Cash:      100000,
			OrderSize: 1000,
			Exits: config.ExitsConfig{
				Enabled: true, StopLossPct: 0.05, TrailingStopPct: 0.08,
				TakeProfitPct: 0.2, ATRPeriod: 14, ATRStopMult: 2,
			},
			Costs: config.CostsConfig{CommissionPct: 0.001, SpreadPct: 0.0005},
		}

		bt.Run(data)
	}
}
