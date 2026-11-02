package backtest

import (
	"bytes"
	"math"
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/ai"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/ml"
)

// TestConfigureAIWarnsAboutADeadDetector guards the CLI's warnings about the
// ways an "ai" or "ml" detector does nothing in a backtest: listed but
// disabled (every bar votes HOLD), or the LLM non-blocking (the replay
// outruns it). Without them an operator reads a backtest of a dead detector
// as a measurement of a live one. Aliases count, in either detector set, and
// a run that does not use the detectors stays quiet.
//
// Not parallel: it installs the process-wide ai and ml defaults.
func TestConfigureAIWarnsAboutADeadDetector(t *testing.T) {
	prevAI, prevML := ai.Config(), ml.Default().Config()
	t.Cleanup(func() {
		_ = ai.Configure(prevAI)
		ml.Configure(prevML, ai.Params{})
	})

	run := func(cfg config.Config) string {
		var buf bytes.Buffer

		ConfigureAI(cfg, &buf)

		return buf.String()
	}

	cfg := config.Default()
	cfg.Strategy.Detectors = []string{"ema_cross", "rsi"}

	if out := run(cfg); out != "" {
		t.Errorf("no ai/ml detector, but warned: %q", out)
	}

	cfg.Strategy.DetectorsSell = []string{" LLM "}
	cfg.AI.Enabled = false

	if out := run(cfg); !strings.Contains(out, "ai.enabled is false") {
		t.Errorf("ai alias in the sell set while disabled: got %q", out)
	}

	cfg.AI = config.Default().AI
	cfg.AI.Enabled = true
	cfg.AI.Blocking = false

	if out := run(cfg); !strings.Contains(out, "ai.blocking is false") {
		t.Errorf("non-blocking ai: got %q", out)
	}

	if ai.Default() == nil {
		t.Error("an enabled ai config installed no advisor")
	}

	cfg.AI.Blocking = true

	if out := run(cfg); out != "" {
		t.Errorf("blocking enabled ai warned: %q", out)
	}

	cfg.AI.Provider = "nonsense"

	if out := run(cfg); !strings.Contains(out, "ai advisor disabled") {
		t.Errorf("invalid ai config: got %q", out)
	}

	cfg.AI = config.AIConfig{}
	cfg.Strategy.DetectorsSell = nil
	cfg.Strategy.Detectors = []string{"ema_cross", "Classifier"}
	cfg.ML.Enabled = false

	if out := run(cfg); !strings.Contains(out, "ml.enabled is false") {
		t.Errorf("ml alias while disabled: got %q", out)
	}

	cfg.ML = config.MLConfig{Enabled: true, Horizon: 10, TrainBars: 500, RetrainEvery: 100}

	if out := run(cfg); !strings.Contains(out, "ml detector active: horizon 10") {
		t.Errorf("enabled ml: got %q", out)
	}

	if ml.Default() == nil {
		t.Error("an enabled ml config installed no predictor")
	}
}

// TestBarReturnsAtHasNoLookahead guards the correlation guard's history in
// a backtest: at bar i it may see closes up to and including i and NOTHING
// after, or a decision on bar 100 is correlated against bar 5000. It also
// must answer "unknown" rather than an empty series for a symbol with no
// data or a single bar.
func TestBarReturnsAtHasNoLookahead(t *testing.T) {
	t.Parallel()

	closes := []float64{100, 110, 99, 120, 60}

	candles := make([]market.Candle, len(closes))
	for i, c := range closes {
		candles[i] = market.Candle{Close: c}
	}

	b := &barReturnsAt{data: map[string][]market.Candle{"AAA": candles}}

	if _, ok := b.Returns("NOPE", 3); ok {
		t.Error("unknown symbol reported returns")
	}

	b.bar = 0
	if _, ok := b.Returns("AAA", 3); ok {
		t.Error("a single close reported returns")
	}

	b.bar = 2

	rets, ok := b.Returns("AAA", 10)
	if !ok || len(rets) != 2 {
		t.Fatalf("bar 2: rets=%v ok=%v, want the 2 returns up to bar 2", rets, ok)
	}

	if math.Abs(rets[1]-(99.0/110-1)) > 1e-12 {
		t.Errorf("last return at bar 2 = %v, want 99/110-1 (bar 3's 120 would be lookahead)", rets[1])
	}

	b.bar = 4

	if rets, _ := b.Returns("AAA", 2); len(rets) != 2 || math.Abs(rets[1]-(60.0/120-1)) > 1e-12 {
		t.Errorf("bar 4, n=2: rets=%v, want the last two returns", rets)
	}

	// A cursor past the data clamps rather than panicking.
	b.bar = 99
	if rets, ok := b.Returns("AAA", 100); !ok || len(rets) != 4 {
		t.Errorf("cursor past the end: rets=%v ok=%v, want all 4", rets, ok)
	}
}

// TestStrengthFractionScalesLinearly guards the backtest-only conviction
// knob: off it is exactly 1 (the live engine has no equivalent, so a
// backtest that scaled anyway would understate live exposure); on it runs
// linearly from min_size_fraction to 1, clamped at both ends, with an unset
// or out-of-range fraction falling back to 0.25 / capped at 1.
func TestStrengthFractionScalesLinearly(t *testing.T) {
	t.Parallel()

	off := &Backtester{MinSizeFraction: 0.5}
	if got := off.strengthFraction(0); got != 1 {
		t.Errorf("SizeByStrength off: %v, want 1", got)
	}

	on := &Backtester{SizeByStrength: true, MinSizeFraction: 0.4}

	for _, tc := range []struct{ s, want float64 }{
		{0, 0.4}, {0.5, 0.7}, {1, 1}, {-3, 0.4}, {7, 1},
	} {
		if got := on.strengthFraction(tc.s); math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("strengthFraction(%v) = %v, want %v", tc.s, got, tc.want)
		}
	}

	if got := (&Backtester{SizeByStrength: true}).strengthFraction(0); got != 0.25 {
		t.Errorf("unset fraction at strength 0 = %v, want the 0.25 default", got)
	}

	if got := (&Backtester{SizeByStrength: true, MinSizeFraction: 3}).strengthFraction(0); got != 1 {
		t.Errorf("fraction 3 at strength 0 = %v, want it capped at 1", got)
	}
}
