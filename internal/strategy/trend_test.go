package strategy

import (
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// closesToCandles builds candles whose Close follows the given series (OHLC all
// set to the close), enough for the trend detector's warmup.
func closesToCandles(closes []float64) []market.Candle {
	out := make([]market.Candle, len(closes))
	for i, c := range closes {
		out[i] = market.Candle{Open: c, High: c, Low: c, Close: c, Volume: 1}
	}
	return out
}

func risingCloses(n int, start, step float64) []float64 {
	v := make([]float64, n)
	for i := range v {
		v[i] = start + step*float64(i)
	}
	return v
}

func TestTrendDetectorDirection(t *testing.T) {
	cfg := config.StrategyConfig{TrendPeriod: 100, TrendThreshold: 0.01}
	d := newTrendDetector(cfg)

	// A clear ~10% rise over 30 bars => BUY.
	up := toSeries(closesToCandles(risingCloses(30, 100, 0.34)))
	if a, str, _ := d.Detect(up); a != Buy || str <= 0 {
		t.Errorf("uptrend => %s str %.2f, want BUY with positive strength", a, str)
	}

	// Mirror image => SELL.
	down := toSeries(closesToCandles(risingCloses(30, 110, -0.34)))
	if a, _, _ := d.Detect(down); a != Sell {
		t.Errorf("downtrend => %s, want SELL", a)
	}

	// Flat below the threshold => HOLD.
	flat := toSeries(closesToCandles(risingCloses(30, 100, 0.0001)))
	if a, _, _ := d.Detect(flat); a != Hold {
		t.Errorf("flat trend => %s, want HOLD", a)
	}
}

func TestTrendDetectorWarmup(t *testing.T) {
	d := newTrendDetector(config.StrategyConfig{TrendPeriod: 100, TrendThreshold: 0.01})
	short := toSeries(closesToCandles(risingCloses(trendMinBars-1, 100, 1)))
	if a, _, reason := d.Detect(short); a != Hold || !strings.Contains(reason, "warming up") {
		t.Errorf("below trendMinBars => %s/%q, want HOLD warming up", a, reason)
	}
}

func TestTrendGateBlocksCounterTrend(t *testing.T) {
	upSeries := closesToCandles(risingCloses(40, 100, 0.5)) // strong uptrend
	filter := newTrendDetector(config.StrategyConfig{TrendPeriod: 100, TrendThreshold: 0.01})

	// A SELL signal against an up-trend is vetoed.
	c := &Combined{
		detectors: []Detector{fakeDetector{"a", Sell, 0.9}},
		mode:      Majority, sellMode: Majority, warmup: 1,
		trendGate: true, trendFilter: filter,
	}
	sig := c.Evaluate("X", upSeries)
	if sig.Action != Hold {
		t.Fatalf("SELL into an up-trend => %s, want HOLD", sig.Action)
	}
	if !strings.Contains(sig.Reason, "blocked by") {
		t.Errorf("reason should explain the veto, got %q", sig.Reason)
	}

	// A BUY with the up-trend is allowed.
	cb := &Combined{
		detectors: []Detector{fakeDetector{"a", Buy, 0.9}},
		mode:      Majority, sellMode: Majority, warmup: 1,
		trendGate: true, trendFilter: filter,
	}
	if got := cb.Evaluate("X", upSeries).Action; got != Buy {
		t.Errorf("BUY with the up-trend => %s, want BUY", got)
	}
}

func TestTrendGateFlatDoesNotBlock(t *testing.T) {
	flat := closesToCandles(risingCloses(40, 100, 0.0001)) // below threshold
	filter := newTrendDetector(config.StrategyConfig{TrendPeriod: 100, TrendThreshold: 0.01})
	c := &Combined{
		detectors: []Detector{fakeDetector{"a", Sell, 0.9}},
		mode:      Majority, sellMode: Majority, warmup: 1,
		trendGate: true, trendFilter: filter,
	}
	if got := c.Evaluate("X", flat).Action; got != Sell {
		t.Errorf("SELL with a flat trend => %s, want SELL (no gating)", got)
	}
}

func TestCombinedMaxLookback(t *testing.T) {
	cfg := config.StrategyConfig{
		Detectors: []string{"rsi", "trend"}, Combine: "majority", MinStrength: 0.1,
		MinStrengthSell: -1, FastMA: 12, SlowMA: 26, RSIPeriod: 14,
		TrendPeriod: 1200, TrendThreshold: 0.01,
	}
	cmb := New(cfg).(*Combined)
	lb, ok := interface{}(cmb).(interface{ MaxLookback() int })
	if !ok {
		t.Fatal("Combined should expose MaxLookback")
	}
	if got := lb.MaxLookback(); got < 1200 {
		t.Errorf(
			"MaxLookback = %d, want >= trend period 1200 so backtests size the window correctly",
			got,
		)
	}
}

func TestNewBuildsTrendGate(t *testing.T) {
	cfg := config.StrategyConfig{
		Detectors: []string{"trend"}, Combine: "majority", MinStrength: 0.1,
		MinStrengthSell: -1, FastMA: 12, SlowMA: 26,
		TrendPeriod: 100, TrendThreshold: 0.01, TrendGate: true,
	}
	s := New(cfg)
	cmb, ok := s.(*Combined)
	if !ok {
		t.Fatalf("New returned %T, want *Combined", s)
	}
	if !cmb.trendGate || cmb.trendFilter == nil {
		t.Error("trend gate not wired from config")
	}
}
