package strategy

import (
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/ai"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// stubAdvisor is a deterministic stand-in for the LLM advisor.
type stubAdvisor struct {
	verdict ai.Verdict
	have    bool
	maxStr  float64
	model   string
	asked   []string // symbols it was consulted about
}

// Verdict returns the canned verdict and records the symbol.
func (s *stubAdvisor) Verdict(symbol string, _ ai.Features) (ai.Verdict, bool) {
	s.asked = append(s.asked, symbol)

	return s.verdict, s.have
}

// MaxStrength returns the configured strength cap.
func (s *stubAdvisor) MaxStrength() float64 { return s.maxStr }

// Model names the stub model.
func (s *stubAdvisor) Model() string { return s.model }

// aiTestCandles builds a rising series long enough to clear the detector's
// warmup so BuildFeatures produces a usable snapshot.
func aiTestCandles(n int) []market.Candle {
	out := make([]market.Candle, n)
	price := 100.0

	for i := range out {
		price *= 1.001
		out[i] = market.Candle{
			Open: price, High: price * 1.002, Low: price * 0.998,
			Close: price, Volume: 1000,
		}
	}

	return out
}

// newAITestDetector builds the AI detector with a stubbed advisor.
func newAITestDetector(adv advisor) *aiDetector {
	d := newAIDetector(config.Default().Strategy)
	d.adv = adv

	return d
}

// TestAIDetectorVotesVerdict checks a BUY verdict becomes a BUY vote whose
// strength is the model's confidence scaled by the configured cap.
func TestAIDetectorVotesVerdict(t *testing.T) {
	t.Parallel()

	adv := &stubAdvisor{
		verdict: ai.Verdict{Action: ai.Buy, Confidence: 0.8, Reason: "trend up"},
		have:    true, maxStr: 0.5, model: "llama3.2",
	}
	d := newAITestDetector(adv)

	action, strength, reason := d.DetectFor("AAPL", toSeries(aiTestCandles(400)))
	if action != Buy {
		t.Fatalf("action = %v, want BUY", action)
	}

	if strength != 0.4 { // 0.8 confidence × 0.5 max_strength
		t.Errorf("strength = %v, want 0.4 (confidence capped by max_strength)", strength)
	}

	if !strings.Contains(reason, "trend up") || !strings.Contains(reason, "llama3.2") {
		t.Errorf("reason = %q, want the model name and its reason", reason)
	}

	if len(adv.asked) != 1 || adv.asked[0] != "AAPL" {
		t.Errorf("advisor consulted with %v, want [AAPL]", adv.asked)
	}
}

// TestAIDetectorHoldsWhenUnavailable checks every "no answer" path is a HOLD
// with zero strength — the advisor may abstain, never guess.
func TestAIDetectorHoldsWhenUnavailable(t *testing.T) {
	t.Parallel()

	candles := aiTestCandles(400)

	cases := []struct {
		name string
		adv  advisor
		bars []market.Candle
		want string
	}{
		{
			"disabled",
			&stubAdvisor{model: ""},
			candles,
			"disabled",
		},
		{
			"no verdict yet",
			&stubAdvisor{have: false, model: "m", maxStr: 1},
			candles,
			"awaiting",
		},
		{
			"not enough history",
			&stubAdvisor{have: true, model: "m", maxStr: 1},
			candles[:1],
			"history",
		},
		{
			"model says hold",
			&stubAdvisor{
				verdict: ai.Verdict{Action: ai.Hold, Reason: "mixed"},
				have:    true, model: "m", maxStr: 1,
			},
			candles,
			"HOLD",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := newAITestDetector(tc.adv)

			action, strength, reason := d.DetectFor("AAPL", toSeries(tc.bars))
			if action != Hold || strength != 0 {
				t.Errorf("got %v/%v, want HOLD/0", action, strength)
			}

			if !strings.Contains(reason, tc.want) {
				t.Errorf("reason = %q, want it to mention %q", reason, tc.want)
			}
		})
	}
}

// TestAIDetectorWithoutSymbolAbstains checks the plain Detect path: without a
// symbol there is no per-symbol verdict to read, so it must not vote.
func TestAIDetectorWithoutSymbolAbstains(t *testing.T) {
	t.Parallel()

	adv := &stubAdvisor{
		verdict: ai.Verdict{Action: ai.Buy, Confidence: 1},
		have:    true, maxStr: 1, model: "m",
	}
	d := newAITestDetector(adv)

	if action, strength, _ := d.Detect(toSeries(aiTestCandles(400))); action != Hold ||
		strength != 0 {
		t.Errorf("got %v/%v, want HOLD/0 without a symbol", action, strength)
	}

	if len(adv.asked) != 0 {
		t.Errorf("advisor was consulted %v without a symbol", adv.asked)
	}
}

// TestAIDetectorRegistered checks the detector is reachable by name and alias,
// and that an unconfigured advisor leaves the rest of the strategy working.
func TestAIDetectorRegistered(t *testing.T) {
	t.Parallel()

	cfg := config.Default().Strategy

	for _, name := range []string{"ai", "llm", "ollama"} {
		d := buildDetector(name, cfg)
		if d == nil {
			t.Fatalf("buildDetector(%q) = nil, want the AI detector", name)
		}

		if d.Name() != "ai" {
			t.Errorf("buildDetector(%q).Name() = %q, want ai", name, d.Name())
		}
	}

	// With no advisor configured (the default), a strategy that includes "ai"
	// must still evaluate: the AI vote is simply HOLD.
	cfg.Detectors = []string{"rsi", "ai"}
	cfg.MinStrength = 0
	cfg.TrendGate = false // the gate would veto before the votes are formatted

	sig := New(cfg).Evaluate("AAPL", aiTestCandles(400))
	if !strings.Contains(sig.Reason, "ai:") {
		t.Errorf("reason = %q, want the ai detector's vote listed", sig.Reason)
	}
}

// TestSymbolDetectorRoutingIsOptional checks that plain detectors are unaffected
// by the symbol-aware path.
func TestSymbolDetectorRoutingIsOptional(t *testing.T) {
	t.Parallel()

	rsi := newRSIDetector(config.Default().Strategy)
	s := toSeries(aiTestCandles(400))

	a1, s1, _ := detect(rsi, "AAPL", s)
	a2, s2, _ := rsi.Detect(s)

	if a1 != a2 || s1 != s2 {
		t.Errorf("detect() = %v/%v, Detect() = %v/%v; must match for plain detectors",
			a1, s1, a2, s2)
	}
}
