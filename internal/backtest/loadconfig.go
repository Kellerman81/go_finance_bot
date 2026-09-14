package backtest

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// LoadConfig loads configuration for a backtest or optimize run. Unlike a live
// run — where a missing config is fine because env vars can supply everything —
// a backtest that silently falls back to built-in defaults makes the user's
// edits look like they have no effect ("changing settings doesn't change the
// result"). So this surfaces the problem on the given writer (use os.Stderr):
// a missing file warns and uses defaults.
//
// A read/parse failure and a Validate failure get distinct messages, because
// they mean very different things about what actually runs next: a read or
// parse failure means config.Default() runs — none of the file's settings
// were applied, not even the well-formed ones — while a Validate failure
// means the file's settings WERE fully applied and are running exactly as
// invalid as Validate found them (an inverted MA period, a stop-loss ladder
// over 100%, etc.). A single "continuing with best-effort values" message
// used to cover both, which was actively misleading for the second case: an
// operator's own settings, known broken, are not a "best-effort fallback".
// This does not change the underlying policy of continuing to run either
// way rather than refusing outright — only what the operator is told.
func LoadConfig(path string, warn io.Writer) config.Config {
	if warn == nil {
		warn = os.Stderr
	}

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(warn, "WARNING: config %q not found — running on built-in defaults; "+
			"your edits are NOT applied. Pass -config <path> (e.g. -config data/config.yaml).\n", path)

		return config.Default()
	}

	cfg, err := config.Load(path)

	switch {
	case err == nil:
		// no warning to print
	case errors.Is(err, config.ErrValidation):
		// The file parsed and every setting in it was applied — Validate
		// rejected the RESULT (e.g. an inverted MA period, a stop-loss
		// ladder over 100%), and that invalid result is what continues to
		// run. "Best-effort values" would be actively misleading here: they
		// are not a fallback, they are the operator's own settings, known
		// broken.
		fmt.Fprintf(
			warn,
			"WARNING: config %q failed validation: %v — running it ANYWAY, exactly as invalid "+
				"as reported. Fix the file; do not assume this backtest is representative.\n",
			path,
			err,
		)
	default:
		// A read or parse failure: cfg is essentially config.Default() — none
		// of the file's settings were applied, not even the ones with no
		// syntax problem.
		fmt.Fprintf(
			warn,
			"WARNING: config %q could not be read: %v — running on built-in defaults; "+
				"NONE of your settings were applied.\n",
			path,
			err,
		)
	}

	return cfg
}
