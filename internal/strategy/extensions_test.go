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

// TestWeightedStrengthUnaffectedByAbstainingDetectors reproduces the AT-8
// fix: sideQualifies used to divide the "weighted" mode's net strength by
// `total` — every configured detector, including ones that abstained
// (voted HOLD) — so a reported strength shrank purely from adding more
// detectors to the config, independent of whether the decision itself got
// any more or less convincing. An abstaining detector must not move the
// strength of the side that actually decided.
func TestWeightedStrengthUnaffectedByAbstainingDetectors(t *testing.T) {
	agreeing := []Detector{
		fakeDetector{"a", Buy, 0.8},
		fakeDetector{"b", Buy, 0.6},
	}

	sigWithout := combinedWith(Weighted, agreeing...).Evaluate("X", twoBars)

	withAbstainer := append(append([]Detector(nil), agreeing...), fakeDetector{"c", Hold, 0})
	sigWith := combinedWith(Weighted, withAbstainer...).Evaluate("X", twoBars)

	if sigWithout.Action != Buy || sigWith.Action != Buy {
		t.Fatalf("actions = %s / %s, want BUY on both", sigWithout.Action, sigWith.Action)
	}

	if sigWithout.Strength != sigWith.Strength {
		t.Errorf("strength without an abstaining detector = %.3f, with one = %.3f — "+
			"a HOLD-voting detector must not change the strength of the side that "+
			"actually decided", sigWithout.Strength, sigWith.Strength)
	}
}

// TestWeightedStrengthComparableToMajority reproduces the other half of the
// AT-8 fix: with no opposing votes, "weighted" and "majority" must report
// the identical strength scale for the identical votes — before the fix,
// weighted alone divided by every configured detector rather than the
// deciding side's own count, so the same conviction read very differently
// depending purely on which combine mode was configured, even though
// min_strength/min_strength_sell gate both with the same numbers and
// Evaluate's own SELL-vs-BUY tie-break compares them directly.
func TestWeightedStrengthComparableToMajority(t *testing.T) {
	dets := []Detector{
		fakeDetector{"a", Buy, 0.8},
		fakeDetector{"b", Buy, 0.6},
	}

	sigMaj := combinedWith(Majority, dets...).Evaluate("X", twoBars)
	sigWtd := combinedWith(Weighted, dets...).Evaluate("X", twoBars)

	if sigMaj.Strength != sigWtd.Strength {
		t.Errorf("majority strength = %.3f, weighted strength = %.3f — with no "+
			"opposing votes, the two modes must agree on scale for the same votes",
			sigMaj.Strength, sigWtd.Strength)
	}
}

// TestNegativeWeightMutesRatherThanInvertsAVote reproduces the "negative
// detector weights invert a vote instead of muting it" finding.
// config.StrategyConfig.Weights documents "a zero or negative weight mutes a
// detector's strength contribution without removing its vote" — but
// multiplying strength by a negative weight instead INVERTS the
// contribution, turning a BUY detector's positive strength into a negative
// addend. Two agreeing BUY detectors, one weighted negative, must still
// combine to BUY (the muted detector's vote survives at zero strength, not
// a large negative one dragging the net below zero).
func TestNegativeWeightMutesRatherThanInvertsAVote(t *testing.T) {
	dets := []Detector{
		fakeDetector{"a", Buy, 1.0},
		fakeDetector{"b", Buy, 1.0},
	}

	c := &Combined{
		detectors: dets, mode: Weighted, sellMode: Weighted, warmup: 1,
		weights: map[string]float64{"b": -3},
	}

	sig := c.Evaluate("X", twoBars)
	if sig.Action != Buy {
		t.Fatalf("two agreeing BUY detectors (one weighted -3) => %s, want BUY — "+
			"a negative weight inverted the vote instead of muting it", sig.Action)
	}

	// b's strength contribution should be 0 (muted), not -3.0 (inverted): net
	// = 1.0 (a) + 0 (b, muted) = 1.0, strength = net/total = 1.0/2.
	if want := 0.5; sig.Strength < want-1e-9 || sig.Strength > want+1e-9 {
		t.Errorf("strength = %v, want %v (b's contribution muted to 0, not inverted to -3)",
			sig.Strength, want)
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
