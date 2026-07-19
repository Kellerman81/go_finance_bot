package strategy

import (
	"slices"
	"strings"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// detectorFactory builds a Detector from configuration.
type detectorFactory func(config.StrategyConfig) Detector

// detectorReg registers a detector under a canonical name plus optional aliases.
// Adding a detector is a single entry here — buildDetector and
// AvailableDetectors both derive from this list, so the two can no longer drift.
type detectorReg struct {
	canonical string
	aliases   []string
	factory   detectorFactory
}

var detectorRegs = []detectorReg{
	{
		"ema_cross",
		[]string{"ema", "macross"},
		func(c config.StrategyConfig) Detector { return newEMACrossDetector(c) },
	},
	{"rsi", nil, func(c config.StrategyConfig) Detector { return newRSIDetector(c) }},
	{"macd", nil, func(c config.StrategyConfig) Detector { return newMACDDetector(c) }},
	{
		"bollinger",
		[]string{"bbands", "bb"},
		func(c config.StrategyConfig) Detector { return newBollingerDetector(c) },
	},
	{"vwap", nil, func(c config.StrategyConfig) Detector { return newVWAPDetector(c) }},
	{"atr", nil, func(c config.StrategyConfig) Detector { return newATRDetector(c) }},
	{
		"rvol",
		[]string{"relvol"},
		func(c config.StrategyConfig) Detector { return newRVOLDetector(c) },
	},
	{
		"volume_profile",
		[]string{"vp", "volumeprofile"},
		func(c config.StrategyConfig) Detector { return newVolumeProfileDetector(c) },
	},
	{
		"trend",
		[]string{"linreg", "regression"},
		func(c config.StrategyConfig) Detector { return newTrendDetector(c) },
	},
	{"adx", []string{"dmi"}, func(c config.StrategyConfig) Detector { return newADXDetector(c) }},
	{
		"stochastic",
		[]string{"stoch"},
		func(c config.StrategyConfig) Detector { return newStochasticDetector(c) },
	},
	{
		"supertrend",
		[]string{"st"},
		func(c config.StrategyConfig) Detector { return newSupertrendDetector(c) },
	},
	{
		"donchian",
		[]string{"dc"},
		func(c config.StrategyConfig) Detector { return newDonchianDetector(c) },
	},
}

// detectorByName maps every canonical name and alias to its factory.
var detectorByName = func() map[string]detectorFactory {
	m := make(map[string]detectorFactory, len(detectorRegs)*2)
	for _, r := range detectorRegs {
		m[r.canonical] = r.factory
		for _, a := range r.aliases {
			m[a] = r.factory
		}
	}

	return m
}()

// buildDetector constructs the detector registered under name (canonical or
// alias, case-insensitive), or nil when unknown.
func buildDetector(name string, cfg config.StrategyConfig) Detector {
	if f, ok := detectorByName[strings.ToLower(strings.TrimSpace(name))]; ok {
		return f(cfg)
	}

	return nil
}

// AvailableDetectors lists the canonical detector names that can be configured.
func AvailableDetectors() []string {
	names := make([]string, len(detectorRegs))
	for i, r := range detectorRegs {
		names[i] = r.canonical
	}

	slices.Sort(names)

	return names
}
