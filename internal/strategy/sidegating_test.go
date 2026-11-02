package strategy

import (
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// fixedVote is a detector that always casts the same vote.
type fixedVote struct {
	name     string
	action   Action
	strength float64
}

func (f fixedVote) Name() string                            { return f.name }
func (fixedVote) WarmupBars() int                           { return 1 }
func (f fixedVote) Detect(series) (Action, float64, string) { return f.action, f.strength, f.name }

// TestAVetoedSellLeavesAnAllowedBuy is the regression for ST2-3. With
// separate buy and sell detector sets, SELL was chosen over BUY (it was
// stronger) BEFORE the trend gate ran; the gate then vetoed the SELL and the
// result was HOLD — though the same gate would have passed the BUY. In an
// up-trend, BUY 0.5 against SELL 0.6 came out "HOLD: SELL blocked by trend".
func TestAVetoedSellLeavesAnAllowedBuy(t *testing.T) {
	t.Parallel()

	closes := make([]float64, 120)
	for i := range closes {
		closes[i] = 100 + float64(i)*0.5 // a clean up-trend
	}

	candles := make([]market.Candle, len(closes))
	for i, c := range closes {
		candles[i] = market.Candle{Open: c, High: c * 1.001, Low: c * 0.999, Close: c, Volume: 100}
	}

	c := &Combined{
		detectors:     []Detector{fixedVote{"buyer", Buy, 0.5}},
		sellDetectors: []Detector{fixedVote{"seller", Sell, 0.6}},
		mode:          Majority, sellMode: Majority, warmup: 1,
		minStrength: 0.1, minStrengthSell: 0.1,
		trendGate: true, trendFilter: newTrendDetectorParams(100, 0.01),
	}

	if sig := c.Evaluate("X", candles); sig.Action != Buy {
		t.Errorf("up-trend, BUY 0.5 vs vetoed SELL 0.6: got %s (%s), want BUY", sig.Action, sig.Reason)
	}
}

// ST3-2: ST2-3 moved the directional gates into the per-candidate loop, but
// the conviction and deny gates still ran on the side chosen by raw strength.
// A BUY of 0.60 (against min_strength 0.70) beat a SELL of 0.50 (against the
// deliberately lower min_strength_sell 0.30), failed its own bar, and the
// result was HOLD — the strategy exit swallowed.
func TestASubThresholdBuyDoesNotSuppressAQualifyingSell(t *testing.T) {
	t.Parallel()

	candles := make([]market.Candle, 30)
	for i := range candles {
		candles[i] = market.Candle{Open: 100, High: 100, Low: 100, Close: 100, Volume: 1}
	}

	c := &Combined{
		detectors:     []Detector{fixedVote{"buyer", Buy, 0.6}},
		sellDetectors: []Detector{fixedVote{"seller", Sell, 0.5}},
		mode:          Majority, sellMode: Majority, warmup: 1,
		minStrength: 0.7, minStrengthSell: 0.3,
	}

	if sig := c.Evaluate("X", candles); sig.Action != Sell {
		t.Errorf("got %s (%s), want SELL: it clears its own bar", sig.Action, sig.Reason)
	}
}
