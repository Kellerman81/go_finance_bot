package backtest

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes body to a temp file and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return path
}

// TestLoadConfigDistinguishesReadFailureFromValidationFailure reproduces the
// "lenient LoadConfig sometimes discards the whole file, sometimes runs an
// invalid config as-is" finding: a YAML syntax error and a Validate failure
// used to share one message — "continuing with best-effort values" — even
// though they mean opposite things about what actually runs next. A syntax
// error means NONE of the file's settings were applied (the returned config
// is essentially config.Default()); a Validate failure means EVERY setting
// in the file WAS applied, and is running exactly as invalid as Validate
// found it. The two cases must now say which happened, distinctly.
func TestLoadConfigDistinguishesReadFailureFromValidationFailure(t *testing.T) {
	t.Run("syntax error", func(t *testing.T) {
		path := writeConfig(t, "engine: [this is not a mapping\n")

		var warn bytes.Buffer

		LoadConfig(path, &warn)

		msg := warn.String()
		if !strings.Contains(msg, "could not be read") {
			t.Errorf("syntax-error message does not say the file was not read: %q", msg)
		}

		if !strings.Contains(msg, "NONE of your settings were applied") {
			t.Errorf("syntax-error message does not say nothing was applied: %q", msg)
		}

		if strings.Contains(msg, "best-effort") {
			t.Errorf("syntax-error message still uses the misleading old wording: %q", msg)
		}
	})

	t.Run("validation error", func(t *testing.T) {
		// fast_ma >= slow_ma is rejected by Config.Validate regardless of
		// trading_enabled — the same class of mistake the finding names
		// ("inverted MA periods").
		path := writeConfig(t, "strategy:\n  fast_ma: 50\n  slow_ma: 10\n")

		var warn bytes.Buffer

		cfg := LoadConfig(path, &warn)

		msg := warn.String()
		if !strings.Contains(msg, "failed validation") {
			t.Errorf("validation-error message does not say validation failed: %q", msg)
		}

		if !strings.Contains(msg, "running it ANYWAY") {
			t.Errorf("validation-error message does not say the invalid config still runs: %q", msg)
		}

		if strings.Contains(msg, "NONE of your settings") {
			t.Errorf("validation-error message wrongly claims nothing was applied: %q", msg)
		}

		// The file's own (invalid) values must actually be the ones running —
		// this is the "still applies the best-effort parsed values" behavior,
		// unchanged by this fix.
		if cfg.Strategy.FastMA != 50 || cfg.Strategy.SlowMA != 10 {
			t.Errorf("the file's values were not applied despite failing validation: %+v",
				cfg.Strategy)
		}
	})
}

// A config that is missing entirely keeps its own, older message — this
// path does not go through config.Load at all.
func TestLoadConfigMissingFileMessageIsUnchanged(t *testing.T) {
	var warn bytes.Buffer

	LoadConfig(filepath.Join(t.TempDir(), "nope.yaml"), &warn)

	if !strings.Contains(warn.String(), "not found") {
		t.Errorf("missing-file message changed unexpectedly: %q", warn.String())
	}
}

// A valid config prints no warning at all.
func TestLoadConfigValidFileWarnsNothing(t *testing.T) {
	path := writeConfig(t, "strategy:\n  fast_ma: 10\n  slow_ma: 50\n")

	var warn bytes.Buffer

	LoadConfig(path, &warn)

	if warn.Len() != 0 {
		t.Errorf("a valid config printed a warning: %q", warn.String())
	}
}
