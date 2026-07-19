package strategy

import (
	"fmt"
	"strings"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// markSet returns the set of canonical detector names produced by the given
// configured names.
func markSet(names []string, cfg config.StrategyConfig) map[string]bool {
	set := make(map[string]bool)
	for _, n := range names {
		if d := buildDetector(strings.ToLower(strings.TrimSpace(n)), cfg); d != nil {
			set[d.Name()] = true
		}
	}

	return set
}

// DetectorResult is one detector's output for a symbol.
type DetectorResult struct {
	Name        string  `json:"name"`
	Action      Action  `json:"action"`
	Strength    float64 `json:"strength"`
	Reason      string  `json:"reason"`
	Enabled     bool    `json:"enabled"`      // in the BUY detector set?
	EnabledSell bool    `json:"enabled_sell"` // in the SELL detector set?
}

// EvaluateAll runs every available detector against the candles, regardless of
// the configured detector set, so the UI can show all of them side by side. The
// Enabled flag marks which detectors are actually in cfg.Detectors.
func EvaluateAll(cfg config.StrategyConfig, candles []market.Candle) []DetectorResult {
	enabled := markSet(cfg.Detectors, cfg)
	enabledSell := enabled

	if len(cfg.DetectorsSell) > 0 {
		enabledSell = markSet(cfg.DetectorsSell, cfg)
	}

	var s series

	if len(candles) > 0 {
		s = toSeries(candles)
	}

	names := AvailableDetectors()
	out := make([]DetectorResult, 0, len(names))

	for _, name := range names {
		d := buildDetector(name, cfg)
		if d == nil {
			continue
		}

		r := DetectorResult{
			Name:        name,
			Action:      Hold,
			Enabled:     enabled[name],
			EnabledSell: enabledSell[name],
		}
		if len(candles) < d.WarmupBars() {
			r.Reason = fmt.Sprintf("warming up (%d/%d bars)", len(candles), d.WarmupBars())
		} else {
			r.Action, r.Strength, r.Reason = d.Detect(s)
		}

		out = append(out, r)
	}

	return out
}
