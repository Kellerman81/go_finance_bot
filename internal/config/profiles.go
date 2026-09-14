package config

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Profile names a coherent starting configuration.
//
// The settings in this file interact: a strategy that trades often needs
// different exits and different protections than one that waits for high
// conviction, and picking each knob independently is how people end up with a
// bot that takes 40 trades a day behind a 20% stop. A profile is a set of
// choices that were made together — a starting point to tune from, not a
// recommendation, and never a substitute for backtesting your own data.
type Profile string

const (
	// ProfileConservative trades rarely and defensively: high conviction
	// required, tight risk, quick to protect.
	ProfileConservative Profile = "conservative"
	// ProfileBalanced is the middle ground and the closest to the built-in
	// defaults.
	ProfileBalanced Profile = "balanced"
	// ProfileAggressive trades more often with more capital deployed and looser
	// exits. It loses more, more often, by design.
	ProfileAggressive Profile = "aggressive"
)

// Profiles lists the built-in profile names, for CLI help and validation.
var Profiles = []string{
	string(ProfileConservative), string(ProfileBalanced), string(ProfileAggressive),
}

// ApplyProfile returns cfg with the named profile's settings applied. It is
// meant to run *before* the user's YAML is layered on, so an explicit setting
// always wins over the profile that suggested it.
//
// An unknown name is an error rather than a silent fallback: a typo'd profile
// that quietly trades the aggressive settings is precisely the kind of surprise
// this package exists to prevent.
func ApplyProfile(cfg Config, name string) (Config, error) {
	p := Profile(strings.ToLower(strings.TrimSpace(name)))
	switch p {
	case "":
		return cfg, nil

	case ProfileConservative:
		return conservative(cfg), nil

	case ProfileBalanced:
		return balanced(cfg), nil

	case ProfileAggressive:
		return aggressive(cfg), nil
	}

	return cfg, fmt.Errorf("unknown profile %q (available: %s)",
		name, strings.Join(Profiles, ", "))
}

// KnownProfile reports whether name is a built-in profile (empty counts).
func KnownProfile(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))

	return n == "" || slices.Contains(Profiles, n)
}

// conservative waits for agreement, risks little, and gets out early.
func conservative(c Config) Config {
	c.Strategy.Detectors = []string{"trend", "ema_cross", "macd", "adx"}
	c.Strategy.Combine = "unanimous"
	c.Strategy.CombineSell = "majority" // strict to enter, quick to exit
	c.Strategy.MinStrength = 0.45
	c.Strategy.MinStrengthSell = 0.15
	c.Strategy.TrendGate = true

	c.Engine.ConfirmBuy = 3
	c.Engine.ConfirmSell = 2
	c.Engine.KeepInterval = 4 * time.Hour
	c.Engine.AllowPyramiding = false
	c.Engine.MinPositions = 10 // spread the budget widely

	c.Exits.Enabled = true
	c.Exits.StopLossPct = 0.04
	c.Exits.TrailingStopPct = 0.05
	c.Exits.TakeProfitPct = 0.12
	c.Exits.ATRStopMult = 2

	c.Protections.Enabled = true
	c.Protections.CooldownAfterLoss = 2 * time.Hour
	c.Protections.StopGuardTrades = 3
	c.Protections.StopGuardLookback = 12 * time.Hour
	c.Protections.StopGuardLock = 12 * time.Hour
	c.Protections.MaxDrawdownPct = 0.10
	c.Protections.MaxDailyLossPct = 0.03

	return c
}

// balanced is the middle ground, close to the built-in defaults.
func balanced(c Config) Config {
	c.Strategy.Detectors = []string{"ema_cross", "rsi", "macd", "trend"}
	c.Strategy.Combine = "majority"
	c.Strategy.MinStrength = 0.25
	c.Strategy.MinStrengthSell = -1
	c.Strategy.TrendGate = true

	c.Engine.ConfirmBuy = 2
	c.Engine.ConfirmSell = 2
	c.Engine.KeepInterval = 2 * time.Hour
	c.Engine.MinPositions = 5

	c.Exits.Enabled = true
	c.Exits.StopLossPct = 0.05
	c.Exits.TrailingStopPct = 0.08
	c.Exits.TakeProfitPct = 0.20

	c.Protections.Enabled = true
	c.Protections.CooldownAfterLoss = 30 * time.Minute
	c.Protections.StopGuardTrades = 4
	c.Protections.StopGuardLookback = 24 * time.Hour
	c.Protections.StopGuardLock = 4 * time.Hour
	c.Protections.MaxDrawdownPct = 0.20
	c.Protections.MaxDailyLossPct = 0.08

	return c
}

// aggressive trades more, deploys more and holds through more noise.
//
// It is included because "make it trade more" is what people do to a
// conservative config anyway, and doing it coherently is better than doing it
// one knob at a time. It will produce deeper drawdowns.
func aggressive(c Config) Config {
	c.Strategy.Detectors = []string{
		"ema_cross", "rsi", "macd", "bollinger", "vwap", "supertrend", "donchian",
	}
	c.Strategy.Combine = "weighted"
	c.Strategy.MinStrength = 0.12
	c.Strategy.MinStrengthSell = 0.10
	c.Strategy.TrendGate = false

	c.Engine.ConfirmBuy = 1
	c.Engine.ConfirmSell = 1
	c.Engine.KeepInterval = 30 * time.Minute
	c.Engine.MinPositions = 3

	c.Exits.Enabled = true
	c.Exits.StopLossPct = 0.08
	c.Exits.TrailingStopPct = 0.12
	c.Exits.TakeProfitPct = 0.30

	c.Protections.Enabled = true
	c.Protections.CooldownAfterLoss = 15 * time.Minute
	c.Protections.StopGuardTrades = 6
	c.Protections.StopGuardLookback = 24 * time.Hour
	c.Protections.StopGuardLock = 2 * time.Hour
	c.Protections.MaxDrawdownPct = 0.30
	c.Protections.MaxDailyLossPct = 0.12

	return c
}
