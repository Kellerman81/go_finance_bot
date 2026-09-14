package backtest

import (
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// risingCandles builds a steadily rising series the trend detector votes BUY on.
func risingCandles(symbol string, n int) []market.Candle {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	out := make([]market.Candle, n)

	for i := range out {
		px := 100.0 + 0.1*float64(i)
		out[i] = market.Candle{
			Symbol: symbol, Open: px, High: px, Low: px, Close: px, Volume: 1,
			Time: base.Add(time.Duration(i) * time.Minute),
		}
	}

	return out
}

// assetClassBacktester is a trend-following backtest over two ETF symbols,
// parameterised only by the portfolio caps under test.
func assetClassBacktester(port config.PortfolioConfig) *Backtester {
	cfg := config.StrategyConfig{
		Detectors: []string{"trend"}, Combine: "majority", MinStrength: 0.01,
		MinStrengthSell: -1, FastMA: 12, SlowMA: 26,
		TrendPeriod: 100, TrendThreshold: 0.01,
	}

	return &Backtester{
		Strategy: strategy.New(cfg),
		Limits: config.Limits{
			MaxOrderValue:    1000,
			MaxPerPosition:   1000,
			MaxTotalInvested: 100000,
			MaxDailySpend:    100000,
			MaxOpenPositions: 10,
		},
		Cash:      100000,
		OrderSize: 500,
		Portfolio: port,
		AssetClasses: map[string]string{
			"IWDA": "etf", "VWCE": "etf",
		},
	}
}

// The backtester runs its own hand-rolled decision loop rather than calling
// into the live engine (see CLAUDE.md's drift warning), so "the guard is
// shared code" is not by itself proof that a backtest models the allocation
// cap. This checks the wiring end to end: the same config that stops the live
// bot buying must stop the backtest buying, or the backtest reports returns
// from a book the live bot would have refused to build.
func TestBacktestHonoursAssetClassCap(t *testing.T) {
	t.Parallel()

	data := map[string][]market.Candle{
		"IWDA": risingCandles("IWDA", 400),
		"VWCE": risingCandles("VWCE", 400),
	}

	uncapped := assetClassBacktester(config.PortfolioConfig{}).Run(data)
	if uncapped.NumTrades == 0 {
		t.Fatal("the control run made no trades; the cap test would prove nothing")
	}

	// 0.1% of a 100000 book is 100 — less than one 500 order, so no ETF buy
	// can ever fit inside the budget.
	capped := assetClassBacktester(config.PortfolioConfig{
		Enabled:             true,
		MaxPctPerAssetClass: map[string]float64{"etf": 0.001},
	}).Run(data)

	if capped.NumTrades != 0 {
		t.Errorf("the backtester made %d trades past an asset-class cap that fits none",
			capped.NumTrades)
	}
}

// min_positions_per_asset_class sizes a buy DOWN rather than refusing it, and
// it is enforced in risk.Manager rather than in the guard the backtester calls
// directly — so it only reaches a backtest through the manager the backtester
// builds itself. Same reasoning as the cap test above: shared code is not proof
// of shared wiring. Without it, a backtest would report the returns of
// full-size positions the live bot would have quartered.
func TestBacktestHonoursAssetClassSlots(t *testing.T) {
	t.Parallel()

	data := map[string][]market.Candle{
		"IWDA": risingCandles("IWDA", 400),
		"VWCE": risingCandles("VWCE", 400),
	}

	// A 1% ETF budget on a 100000 book is 1000 — room for the 500 order the
	// control run places, so the class cap itself never refuses a buy.
	capOnly := config.PortfolioConfig{
		Enabled:             true,
		MaxPctPerAssetClass: map[string]float64{"etf": 0.01},
	}

	control := assetClassBacktester(capOnly).Run(data)
	if control.NumTrades == 0 {
		t.Fatal("the control run made no trades; the slot test would prove nothing")
	}

	// Not exactly 500: qty is rounded down to four decimals before the fill.
	if got := largestBuy(control); got < 499 {
		t.Fatalf("control's largest buy was %.2f, want the full 500 order", got)
	}

	// The same budget split five ways: 200 per position, so every buy must be
	// clamped to 200 even though the order size and every money limit allow more.
	slotted := capOnly
	slotted.MinPositionsPerAssetClass = map[string]int{"etf": 5}

	split := assetClassBacktester(slotted).Run(data)
	if split.NumTrades == 0 {
		t.Fatal("the split run made no trades at all; the slot refused rather than sized")
	}

	if got := largestBuy(split); got > 200.01 {
		t.Errorf("largest buy was %.2f, want no more than the 200 slot", got)
	}
}

// largestBuy is the biggest buy notional a run actually placed.
func largestBuy(r Result) float64 {
	var most float64

	for _, tr := range r.Trades {
		if tr.Side != broker.Buy || tr.Status != "filled" {
			continue
		}

		if v := tr.Qty * tr.Price; v > most {
			most = v
		}
	}

	return most
}

// A class with no cap, and an unclassified symbol, must trade exactly as they
// would with the caps switched off — an allocation budget constrains only what
// it names.
func TestBacktestAssetClassCapLeavesOtherClassesAlone(t *testing.T) {
	t.Parallel()

	data := map[string][]market.Candle{
		"IWDA": risingCandles("IWDA", 400),
		"VWCE": risingCandles("VWCE", 400),
	}

	uncapped := assetClassBacktester(config.PortfolioConfig{}).Run(data)

	// The cap names a class neither symbol belongs to.
	other := assetClassBacktester(config.PortfolioConfig{
		Enabled:             true,
		MaxPctPerAssetClass: map[string]float64{"bond": 0.001},
	}).Run(data)

	if other.NumTrades != uncapped.NumTrades {
		t.Errorf("a cap on an unrelated class changed the run: %d trades vs %d",
			other.NumTrades, uncapped.NumTrades)
	}
}
