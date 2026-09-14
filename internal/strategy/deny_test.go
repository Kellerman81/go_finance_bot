package strategy

import (
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// risingCandlesFlatVolume builds n candles with a strictly rising
// open==high==low==close (a clean breakout series with no wicks) and a flat
// volume on every bar — so the final bar's relative volume reads ~1.0, no
// surge.
func risingCandlesFlatVolume(n int, start, step, volume float64) []market.Candle {
	out := make([]market.Candle, n)
	for i := range out {
		p := start + step*float64(i)
		out[i] = market.Candle{Open: p, High: p, Low: p, Close: p, Volume: volume}
	}

	return out
}

func TestNewBuildsDenyGate(t *testing.T) {
	cfg := config.StrategyConfig{
		Detectors: []string{"ema_cross"}, Combine: "majority", MinStrength: 0.1,
		MinStrengthSell: -1, FastMA: 12, SlowMA: 26,
		DenyGate: true, DenyBreakoutPeriod: 20, DenyBreakoutRVOLThreshold: 1.2,
		DenyReversalStochPeriod: 5, DenyReversalOverbought: 85,
	}

	cmb, ok := New(cfg).(*Combined)
	if !ok {
		t.Fatalf("New returned %T, want *Combined", cmb)
	}

	if !cmb.denyGate {
		t.Error("denyGate not wired from config")
	}

	if cmb.denyBreakoutPeriod != 20 || cmb.denyBreakoutRVOLThresh != 1.2 {
		t.Errorf("breakout params = {%d %v}, want {20 1.2}",
			cmb.denyBreakoutPeriod, cmb.denyBreakoutRVOLThresh)
	}

	if cmb.denyReversalStochPeriod != 5 || cmb.denyReversalOverbought != 85 {
		t.Errorf("reversal params = {%d %v}, want {5 85}",
			cmb.denyReversalStochPeriod, cmb.denyReversalOverbought)
	}
}

// TestDenyGateBlocksFalseBreakoutWithoutVolume: a fresh N-bar high on flat
// (unconfirmed) volume is exactly the false-breakout setup the chapter warns
// about — the buy must be denied even though the underlying detector(s) say
// BUY.
func TestDenyGateBlocksFalseBreakoutWithoutVolume(t *testing.T) {
	candles := risingCandlesFlatVolume(30, 100, 1, 100) // steady rise, flat volume

	c := &Combined{
		detectors: []Detector{fakeDetector{"a", Buy, 0.9}},
		mode:      Majority, sellMode: Majority, warmup: 1,
		denyGate: true, denyBreakoutPeriod: 20, denyBreakoutRVOLThresh: 1.2,
	}

	sig := c.Evaluate("X", candles)
	if sig.Action != Hold {
		t.Fatalf("false breakout (no volume) => %s, want HOLD (denied)", sig.Action)
	}

	if !strings.Contains(sig.Reason, "denied") || !strings.Contains(sig.Reason, "breakout without volume") {
		t.Errorf("reason should explain the breakout denial, got %q", sig.Reason)
	}
}

// TestDenyGateAllowsBreakoutWithVolume is the mirror case: the same fresh
// high, but with a real volume surge on the breakout bar, must NOT be denied
// — the check is specifically "unconfirmed", not "any breakout".
func TestDenyGateAllowsBreakoutWithVolume(t *testing.T) {
	candles := risingCandlesFlatVolume(30, 100, 1, 100)
	candles[len(candles)-1].Volume = 1000 // 10x surge on the breakout bar

	c := &Combined{
		detectors: []Detector{fakeDetector{"a", Buy, 0.9}},
		mode:      Majority, sellMode: Majority, warmup: 1,
		denyGate: true, denyBreakoutPeriod: 20, denyBreakoutRVOLThresh: 1.2,
	}

	if got := c.Evaluate("X", candles).Action; got != Buy {
		t.Errorf("breakout WITH a volume surge => %s, want BUY (not denied)", got)
	}
}

// TestDenyGateBlocksOverextendedReversal: a short lookback %K pinned at the
// top of its own range (a strictly rising series always sits at the top of a
// short trailing window) is the overextension check — deny even though the
// underlying detector(s) say BUY, isolated from the breakout check (disabled
// via denyBreakoutPeriod: 0).
func TestDenyGateBlocksOverextendedReversal(t *testing.T) {
	candles := risingCandlesFlatVolume(30, 100, 1, 100)

	c := &Combined{
		detectors: []Detector{fakeDetector{"a", Buy, 0.9}},
		mode:      Majority, sellMode: Majority, warmup: 1,
		denyGate: true, denyReversalStochPeriod: 5, denyReversalOverbought: 85,
	}

	sig := c.Evaluate("X", candles)
	if sig.Action != Hold {
		t.Fatalf("overextended move => %s, want HOLD (denied)", sig.Action)
	}

	if !strings.Contains(sig.Reason, "overextended") {
		t.Errorf("reason should explain the overextension denial, got %q", sig.Reason)
	}
}

// TestDenyGateNeverBlocksSell: exits must never be blocked, same rule as
// TrendGate and risk.Manager.Authorize — even under conditions that would
// deny a BUY outright.
func TestDenyGateNeverBlocksSell(t *testing.T) {
	candles := risingCandlesFlatVolume(30, 100, 1, 100)

	c := &Combined{
		detectors: []Detector{fakeDetector{"a", Sell, 0.9}},
		mode:      Majority, sellMode: Majority, warmup: 1,
		denyGate: true, denyBreakoutPeriod: 20, denyBreakoutRVOLThresh: 1.2,
		denyReversalStochPeriod: 5, denyReversalOverbought: 85,
	}

	if got := c.Evaluate("X", candles).Action; got != Sell {
		t.Errorf("SELL under deny conditions => %s, want SELL (never denied)", got)
	}
}

// TestDenyGateDisabledLeavesBuyAlone confirms the default (off) behaviour is
// unchanged: the same conditions that trigger both checks above must not
// deny anything when denyGate is false.
func TestDenyGateDisabledLeavesBuyAlone(t *testing.T) {
	candles := risingCandlesFlatVolume(30, 100, 1, 100)

	c := &Combined{
		detectors: []Detector{fakeDetector{"a", Buy, 0.9}},
		mode:      Majority, sellMode: Majority, warmup: 1,
		denyGate: false, denyBreakoutPeriod: 20, denyBreakoutRVOLThresh: 1.2,
		denyReversalStochPeriod: 5, denyReversalOverbought: 85,
	}

	if got := c.Evaluate("X", candles).Action; got != Buy {
		t.Errorf("denyGate disabled => %s, want BUY (gate must be a no-op when off)", got)
	}
}
