package strategy

import (
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// fakeDetector returns a fixed action/strength, for testing the combiner.
type fakeDetector struct {
	name     string
	action   Action
	strength float64
}

func (f fakeDetector) Name() string    { return f.name }
func (f fakeDetector) WarmupBars() int { return 1 }

func (f fakeDetector) Detect(
	series,
) (Action, float64, string) {
	return f.action, f.strength, f.name
}

func combinedWith(mode CombineMode, ds ...Detector) *Combined {
	return &Combined{detectors: ds, mode: mode, sellMode: mode, warmup: 1}
}

var twoBars = []market.Candle{{Close: 10}, {Close: 11}}

func TestAsymmetricCombine(t *testing.T) {
	// Strict to buy (unanimous), lenient to sell (majority): a single dissenting
	// BUY blocks a unanimous buy, but a sell still goes through on a majority.
	c := &Combined{
		detectors: []Detector{
			fakeDetector{"a", Sell, 0.6},
			fakeDetector{"b", Sell, 0.5},
			fakeDetector{"c", Buy, 0.4},
		},
		mode: Unanimous, sellMode: Majority, warmup: 1,
	}
	if got := c.Evaluate("X", twoBars).Action; got != Sell {
		t.Fatalf("asymmetric (unanimous buy / majority sell) => %s, want SELL", got)
	}

	// Same votes but symmetric unanimous => HOLD (the lone BUY blocks the sell).
	sym := &Combined{
		detectors: []Detector{
			fakeDetector{"a", Sell, 0.6},
			fakeDetector{"b", Sell, 0.5},
			fakeDetector{"c", Buy, 0.4},
		},
		mode: Unanimous, sellMode: Unanimous, warmup: 1,
	}
	if got := sym.Evaluate("X", twoBars).Action; got != Hold {
		t.Errorf("symmetric unanimous with a dissent => %s, want HOLD", got)
	}
}

func TestSeparateSellDetectors(t *testing.T) {
	// BUY set is quiet, SELL set fires => SELL (driven by the sell-only detector).
	c := &Combined{
		detectors:     []Detector{fakeDetector{"buy1", Hold, 0}},
		sellDetectors: []Detector{fakeDetector{"sell1", Sell, 0.8}},
		mode:          Majority, sellMode: Majority, warmup: 1,
	}
	sig := c.Evaluate("X", twoBars)
	if sig.Action != Sell {
		t.Fatalf("separate sell detectors => %s, want SELL", sig.Action)
	}
	if !strings.Contains(sig.Reason, "sell1") {
		t.Errorf("reason should cite the sell detector, got %q", sig.Reason)
	}
	// BUY set fires, SELL set is quiet => BUY (the sell detectors don't veto).
	c2 := &Combined{
		detectors:     []Detector{fakeDetector{"buy1", Buy, 0.8}},
		sellDetectors: []Detector{fakeDetector{"sell1", Hold, 0}},
		mode:          Majority, sellMode: Majority, warmup: 1,
	}
	if got := c2.Evaluate("X", twoBars).Action; got != Buy {
		t.Errorf("buy set fires, sell set quiet => %s, want BUY", got)
	}
}

func TestSellGateLowerThanBuy(t *testing.T) {
	// A weak (0.2) signal: passes the low sell bar (0.1) but fails the buy bar (0.5).
	sellSide := &Combined{
		detectors: []Detector{fakeDetector{"a", Sell, 0.2}},
		mode:      Majority, sellMode: Majority, warmup: 1, minStrength: 0.5, minStrengthSell: 0.1,
	}
	if got := sellSide.Evaluate("X", twoBars).Action; got != Sell {
		t.Fatalf("weak sell with low sell-gate => %s, want SELL", got)
	}
	buySide := &Combined{
		detectors: []Detector{fakeDetector{"a", Buy, 0.2}},
		mode:      Majority, sellMode: Majority, warmup: 1, minStrength: 0.5, minStrengthSell: 0.1,
	}
	if got := buySide.Evaluate("X", twoBars).Action; got != Hold {
		t.Errorf("weak buy below buy-gate => %s, want HOLD", got)
	}
}

func TestMajorityVote(t *testing.T) {
	c := combinedWith(Majority,
		fakeDetector{"a", Buy, 0.6},
		fakeDetector{"b", Buy, 0.4},
		fakeDetector{"c", Sell, 0.9},
	)
	sig := c.Evaluate("X", twoBars)
	if sig.Action != Buy {
		t.Fatalf("majority => %s, want BUY", sig.Action)
	}
	if !strings.Contains(sig.Reason, "=>") || !strings.Contains(sig.Reason, "a:BUY") {
		t.Errorf("reason should compare detectors, got %q", sig.Reason)
	}
}

func TestUnanimousBlocksOnConflict(t *testing.T) {
	conflict := combinedWith(Unanimous,
		fakeDetector{"a", Buy, 0.6},
		fakeDetector{"b", Sell, 0.6},
	)
	if got := conflict.Evaluate("X", twoBars).Action; got != Hold {
		t.Errorf("conflicting unanimous => %s, want HOLD", got)
	}

	agree := combinedWith(Unanimous,
		fakeDetector{"a", Buy, 0.6},
		fakeDetector{"b", Hold, 0},
		fakeDetector{"c", Buy, 0.8},
	)
	if got := agree.Evaluate("X", twoBars).Action; got != Buy {
		t.Errorf("agreeing unanimous => %s, want BUY", got)
	}
}

func TestWeightedNetStrength(t *testing.T) {
	c := combinedWith(Weighted,
		fakeDetector{"a", Buy, 0.8},
		fakeDetector{"b", Sell, 0.3},
	)
	sig := c.Evaluate("X", twoBars)
	if sig.Action != Buy {
		t.Fatalf("weighted net positive => %s, want BUY", sig.Action)
	}
}

func TestWarmupGate(t *testing.T) {
	c := &Combined{detectors: []Detector{fakeDetector{"a", Buy, 1}}, mode: Majority, warmup: 50}
	sig := c.Evaluate("X", twoBars)
	if sig.Action != Hold || !strings.Contains(sig.Reason, "warming up") {
		t.Errorf("expected warmup hold, got %s / %q", sig.Action, sig.Reason)
	}
}
