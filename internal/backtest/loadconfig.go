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
// a missing file warns and uses defaults; a parse/validation error warns but
// still applies the best-effort parsed values.
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
	if err != nil {
		fmt.Fprintf(
			warn,
			"WARNING: config %q: %v — continuing with best-effort values.\n",
			path,
			err,
		)
	}

	return cfg
}
