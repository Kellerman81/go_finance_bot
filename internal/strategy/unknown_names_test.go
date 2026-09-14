package strategy

import (
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/errlog"
)

// TestUnknownDetectorNameIsRecorded reproduces the typo'd-detector-names
// finding: a misspelled detector name was silently dropped, so the
// strategy that actually ran could differ from what was configured with
// nothing in the backtest report or live log to reveal it. It must now be
// recorded where an operator can find it.
func TestUnknownDetectorNameIsRecorded(t *testing.T) {
	errlog.Clear()

	cfg := config.StrategyConfig{Detectors: []string{"ema_cros", "rsi"}} // "ema_cros" is a typo
	s := New(cfg)

	if s == nil {
		t.Fatal("New returned nil")
	}

	if !hasErrlogEntry("strategy", "ema_cros") {
		t.Error("no errlog entry recorded the unknown detector name \"ema_cros\"")
	}
}

// TestAllUnknownDetectorNamesFallsBackWithARecord checks the "every name was
// unknown" case is also recorded, not just silently substituted with a
// plain RSI strategy.
func TestAllUnknownDetectorNamesFallsBackWithARecord(t *testing.T) {
	errlog.Clear()

	cfg := config.StrategyConfig{Detectors: []string{"emacross", "totally_bogus"}}
	s := New(cfg)

	if s == nil {
		t.Fatal("New returned nil")
	}

	if !hasErrlogEntry("strategy", "falling back to a plain RSI") {
		t.Error("no errlog entry recorded the all-unknown fallback to a plain RSI strategy")
	}
}

// TestUnknownCombineModeIsRecorded checks the same treatment applies to an
// unrecognised combine mode, while a genuinely empty one (meaning "use the
// default") is not flagged as an error.
func TestUnknownCombineModeIsRecorded(t *testing.T) {
	errlog.Clear()

	New(config.StrategyConfig{Detectors: []string{"rsi"}, Combine: "concensus"}) // typo

	if !hasErrlogEntry("strategy", "unknown combine mode") {
		t.Error("no errlog entry recorded the unknown combine mode")
	}

	errlog.Clear()

	New(config.StrategyConfig{Detectors: []string{"rsi"}, Combine: ""}) // valid: means "use default"

	if hasErrlogEntry("strategy", "unknown combine mode") {
		t.Error("an empty combine mode (meaning \"use the default\") must not be flagged as unknown")
	}
}

func hasErrlogEntry(source, substr string) bool {
	for _, e := range errlog.Recent() {
		if e.Source == source && strings.Contains(e.Message, substr) {
			return true
		}
	}

	return false
}

// TestCanonicalDetectorResolvesAliases covers the helper internal/autotune uses
// to compare a configured detector set against the optimizer's search space.
// Without alias resolution a config written as `detectors: [ema, bb]` would
// read as two detectors no search set contains, and autotune would refuse to
// tune a perfectly ordinary symbol.
func TestCanonicalDetectorResolvesAliases(t *testing.T) {
	cases := map[string]string{
		"ema_cross":  "ema_cross",
		"ema":        "ema_cross",
		"macross":    "ema_cross",
		"bb":         "bollinger",
		"bbands":     "bollinger",
		"llm":        "ai",
		"ollama":     "ai",
		"model":      "ml",
		"classifier": "ml",
		"  RSI  ":    "rsi", // trimmed and lower-cased, as buildDetector does
	}

	for in, want := range cases {
		got, ok := CanonicalDetector(in)
		if !ok || got != want {
			t.Errorf("CanonicalDetector(%q) = %q, %v; want %q, true", in, got, ok, want)
		}
	}

	if got, ok := CanonicalDetector("not-a-detector"); ok {
		t.Errorf("CanonicalDetector(unknown) = %q, true; want ok=false", got)
	}

	// Every canonical name must resolve to itself, so the registry cannot
	// gain a detector this helper silently fails to recognise.
	for _, name := range AvailableDetectors() {
		if got, ok := CanonicalDetector(name); !ok || got != name {
			t.Errorf("canonical name %q did not round-trip: got %q, %v", name, got, ok)
		}
	}
}
