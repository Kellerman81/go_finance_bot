package main

import (
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// TestValidateFlagsRejectsNonPositiveCash reproduces the "-cash 0 and -bars
// too low don't crash but produce a misleading report" finding: a
// non-positive -cash used to flow straight into the run and surface only as
// a "strategy made no trades" report, indistinguishable from a strategy
// that legitimately never signalled.
func TestValidateFlagsRejectsNonPositiveCash(t *testing.T) {
	for _, cash := range []float64{0, -100} {
		if err := validateFlags(cash, 500, "AAPL"); err == nil {
			t.Errorf("cash=%v: want a validation error, got nil", cash)
		}
	}
}

// -bars only matters with -generate; -csv reads however many rows the file
// has, so a low/zero -bars value must not be rejected there.
func TestValidateFlagsRejectsNonPositiveBarsOnlyWithGenerate(t *testing.T) {
	for _, bars := range []int{0, -10} {
		if err := validateFlags(100000, bars, "AAPL"); err == nil {
			t.Errorf("bars=%d with -generate: want a validation error, got nil", bars)
		}

		if err := validateFlags(100000, bars, ""); err != nil {
			t.Errorf("bars=%d without -generate (i.e. -csv): want no error, got %v", bars, err)
		}
	}
}

func TestValidateFlagsAllowsSensibleValues(t *testing.T) {
	if err := validateFlags(100000, 500, "AAPL"); err != nil {
		t.Errorf("sensible cash/bars: want no error, got %v", err)
	}
}

// TestBacktesterCarriesMinPositions is the cmd half of CM2-1: backtesterFor
// mirrored every sizing setting the live engine trades with except
// engine.min_positions, so a replay sized each buy against the whole
// investable pot where the live bot caps it at one slot of it.
func TestBacktesterCarriesMinPositions(t *testing.T) {
	cfg := config.Default()
	cfg.Engine.MinPositions = 4

	if got := backtesterFor(cfg, 100000, 0).MinPositions; got != 4 {
		t.Errorf("backtester MinPositions = %d, want the configured 4", got)
	}
}

// CM2-4: live buckets symbols by the learned classes layered under
// config.yaml's; this command had only the hand-written map, so with classes
// left to the classifier every symbol ran uncapped and unslotted, silently.
// -db layers the learned labels in (config still wins), and a run that would
// still miss some says so.
func TestBacktestBucketsLikeLiveOrSaysWhyNot(t *testing.T) {
	hand := map[string]string{"IWDA.AS": "stock"} // operator's override
	learned := map[string]string{"iwda.as": "etf", "SAP.DE": "stock"}

	merged := layerLabels(learned, hand)
	if merged["IWDA.AS"] != "stock" || merged["SAP.DE"] != "stock" {
		t.Errorf("merged = %v; want config.yaml to win and learned entries to fill the gaps", merged)
	}

	cfg := config.Config{
		Portfolio:    config.PortfolioConfig{Enabled: true, MaxPctPerAssetClass: map[string]float64{"etf": 0.4}},
		AssetClasses: merged,
	}
	data := map[string][]market.Candle{"IWDA.AS": nil, "SAP.DE": nil, "NOVA.CO": nil}

	warn := unbucketedWarnings(cfg, data)
	if len(warn) != 1 || !strings.Contains(warn[0], "NOVA.CO") || strings.Contains(warn[0], "SAP.DE") {
		t.Errorf("warnings = %v, want one naming only the unclassified NOVA.CO", warn)
	}

	cfg.Portfolio.Enabled = false
	if w := unbucketedWarnings(cfg, data); len(w) != 0 {
		t.Errorf("warned with portfolio caps off: %v", w)
	}
}
