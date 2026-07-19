package strategy

import (
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// TestRegistryCoversNewDetectors verifies every canonical name (including the new
// detectors) builds, and that AvailableDetectors stays in sync with the registry.
func TestRegistryCoversNewDetectors(t *testing.T) {
	cfg := config.Default().Strategy
	want := []string{"adx", "stochastic", "supertrend", "donchian"}
	avail := strings.Join(AvailableDetectors(), ",")
	for _, name := range want {
		if !strings.Contains(avail, name) {
			t.Errorf("AvailableDetectors missing %q", name)
		}
		if buildDetector(name, cfg) == nil {
			t.Errorf("buildDetector(%q) == nil", name)
		}
	}
	// Every advertised detector must be buildable (no drift between the two).
	for _, name := range AvailableDetectors() {
		if buildDetector(name, cfg) == nil {
			t.Errorf("AvailableDetectors lists %q but buildDetector returns nil", name)
		}
	}
	// A couple of aliases resolve.
	for _, alias := range []string{"stoch", "dc", "st", "dmi"} {
		if buildDetector(alias, cfg) == nil {
			t.Errorf("alias %q did not resolve", alias)
		}
	}
}

// TestWeightsFlipWeightedDecision shows a per-detector weight can change a
// weighted-combine outcome: two opposed detectors of equal raw strength net to
// zero unweighted, but weighting the SELL side tips the decision to SELL.
func TestWeightsFlipWeightedDecision(t *testing.T) {
	dets := []Detector{
		fakeDetector{"a", Buy, 0.5},
		fakeDetector{"b", Sell, 0.5},
	}
	// Unweighted: net strength 0 => no BUY, no SELL => HOLD.
	flat := &Combined{detectors: dets, mode: Weighted, sellMode: Weighted, warmup: 1}
	if got := flat.Evaluate("X", twoBars).Action; got != Hold {
		t.Fatalf("equal opposed weighted => %s, want HOLD", got)
	}
	// Weight the SELL detector heavier => net negative => SELL.
	weighted := &Combined{
		detectors: dets, mode: Weighted, sellMode: Weighted, warmup: 1,
		weights: map[string]float64{"b": 3},
	}
	if got := weighted.Evaluate("X", twoBars).Action; got != Sell {
		t.Errorf("weighting b => %s, want SELL", got)
	}
}

// TestNewDetectorsProduceSignals drives the new detectors through New() over a
// trending series and checks the combined strategy reaches a decision.
func TestNewDetectorsProduceSignals(t *testing.T) {
	cfg := config.Default().Strategy
	cfg.Detectors = []string{"adx", "supertrend", "donchian"}
	cfg.Combine = "majority"
	cfg.TrendGate = false
	cfg.MinStrength = 0
	s := New(cfg)

	candles := make([]market.Candle, 300)
	price := 100.0
	for i := range candles {
		price += 0.5 // steady uptrend
		candles[i] = market.Candle{
			Open: price - 0.1, High: price + 0.5, Low: price - 0.5, Close: price, Volume: 1000,
		}
	}
	sig := s.Evaluate("X", candles)
	if sig.Action != Buy {
		t.Errorf(
			"steady uptrend with trend-following detectors => %s (%s), want BUY",
			sig.Action,
			sig.Reason,
		)
	}
}
