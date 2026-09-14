package backtest

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/Kellerman81/go_finance_bot/internal/ai"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/ml"
)

// ConfigureAI installs the LLM advisor for a backtest or optimize run and warns
// on the given writer (use os.Stderr) about the two ways an "ai" detector can
// quietly do nothing:
//
//   - it is in the detector set but the advisor is disabled — every bar votes
//     HOLD, so the detector is dead weight;
//   - it is enabled but not blocking — verdicts are refreshed in the background,
//     which is right for live trading but means a backtest races past the model
//     and mostly reads an empty cache. Set ai.blocking: true for a backtest that
//     actually consults the model (and expect it to take a very long time: one
//     model call per symbol per refresh interval of simulated time).
func ConfigureAI(cfg config.Config, warn io.Writer) {
	if warn == nil {
		warn = os.Stderr
	}

	configureML(cfg, warn)

	if err := ai.Configure(cfg.AI); err != nil {
		fmt.Fprintf(warn, "WARNING: ai advisor disabled: %v\n", err)

		return
	}

	if !usesAI(cfg.Strategy) {
		return
	}

	switch {
	case !cfg.AI.Enabled:
		fmt.Fprintln(
			warn,
			"WARNING: \"ai\" is in the detector set but ai.enabled is false — it will always vote HOLD.",
		)

	case !cfg.AI.Blocking:
		fmt.Fprintln(warn,
			"WARNING: \"ai\" is in the detector set but ai.blocking is false — the backtest will "+
				"outrun the model and mostly see no verdict. Set ai.blocking: true to consult it "+
				"(much slower), or drop \"ai\" from the detector set for this run.")
	}
}

// usesAI reports whether the AI detector is in either detector set, under its
// canonical name or an alias.
func usesAI(s config.StrategyConfig) bool {
	return usesDetector(s, "ai", "llm", "ollama")
}

// usesDetector reports whether any of the given names appears in either
// detector set.
func usesDetector(s config.StrategyConfig, names ...string) bool {
	match := func(n string) bool {
		return slices.Contains(names, strings.ToLower(strings.TrimSpace(n)))
	}

	return slices.ContainsFunc(s.Detectors, match) ||
		slices.ContainsFunc(s.DetectorsSell, match)
}

// configureML installs the learned predictor and warns when the "ml" detector is
// configured but cannot do anything.
//
// Unlike the LLM, ML works properly in a backtest: the predictor trains on rows
// whose forward window has closed and predicts the current bar, so replaying
// history is a continuous walk-forward rather than a fit to the scored data. The
// cost is time — a refit every retrain_every bars per symbol.
func configureML(cfg config.Config, warn io.Writer) {
	ml.Configure(cfg.ML, ml.ParamsFor(cfg.Strategy))

	if !usesDetector(cfg.Strategy, "ml", "model", "classifier") {
		return
	}

	if !cfg.ML.Enabled {
		fmt.Fprintln(
			warn,
			"WARNING: \"ml\" is in the detector set but ml.enabled is false — it will always vote HOLD.",
		)

		return
	}

	fmt.Fprintf(warn,
		"ml detector active: horizon %d bars, threshold %.3f, training window %d bars, "+
			"refit every %d bars (expect a slow run)\n",
		cfg.ML.Horizon, cfg.ML.Threshold, cfg.ML.TrainBars, cfg.ML.RetrainEvery)
}
