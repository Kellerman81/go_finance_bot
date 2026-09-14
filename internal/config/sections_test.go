package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// These reproduce the "numerous sections have no range validation at load"
// finding: Portfolio, Screener, Notify and most of ML had no Validate at
// all, so a config.yaml loaded directly (not through the API, which
// checked some of the same fields on its own, inconsistently) could carry
// a value that inverts what it's supposed to guard — most concretely,
// limits.cash_reserve: -100 *increasing* spendable headroom instead of
// reserving any, and a swapped ml.buy_prob/sell_prob silently inverting
// the detector's BUY/SELL logic.

func TestLimitsRejectsANegativeValue(t *testing.T) {
	base := Limits{
		MaxTotalInvested: 1000, MaxPerPosition: 100, MaxOrderValue: 100,
		MaxDailySpend: 500, CashReserve: 100, MaxOpenPositions: 5,
	}

	if err := base.Validate(); err != nil {
		t.Fatalf("a fully valid Limits should pass, got: %v", err)
	}

	// cash_reserve: -100 is the finding's own example: a negative reserve
	// increases spendable headroom instead of reserving anything.
	negativeReserve := base
	negativeReserve.CashReserve = -100

	if err := negativeReserve.Validate(); err == nil {
		t.Error("a negative cash_reserve: want a validation error, got nil")
	}

	negativeOpen := base
	negativeOpen.MaxOpenPositions = -1

	if err := negativeOpen.Validate(); err == nil {
		t.Error("a negative max_open_positions: want a validation error, got nil")
	}
}

func TestPortfolioConfigRejectsOutOfRangeValues(t *testing.T) {
	if err := (PortfolioConfig{CorrelationThreshold: 1.5}).Validate(); err == nil {
		t.Error("correlation_threshold > 1: want a validation error, got nil")
	}

	if err := (PortfolioConfig{CorrelationThreshold: -0.1}).Validate(); err == nil {
		t.Error("negative correlation_threshold: want a validation error, got nil")
	}

	if err := (PortfolioConfig{MaxPerSector: -1}).Validate(); err == nil {
		t.Error("negative max_per_sector: want a validation error, got nil")
	}

	if err := (PortfolioConfig{CorrelationThreshold: 0.8}).Validate(); err != nil {
		t.Errorf("a valid portfolio config should pass, got: %v", err)
	}
}

func TestScreenerConfigRejectsOutOfRangeAndCrossedBounds(t *testing.T) {
	if err := (ScreenerConfig{MinAvgVolume: -1}).Validate(); err == nil {
		t.Error("negative min_avg_volume: want a validation error, got nil")
	}

	if err := (ScreenerConfig{MinPrice: 100, MaxPrice: 10}).Validate(); err == nil {
		t.Error("min_price > max_price: want a validation error, got nil")
	}

	if err := (ScreenerConfig{
		MinVolatilityPct: 5, MaxVolatilityPct: 1,
	}).Validate(); err == nil {
		t.Error("min_volatility_pct > max_volatility_pct: want a validation error, got nil")
	}

	if err := (ScreenerConfig{MinPrice: 5, MaxPrice: 100}).Validate(); err != nil {
		t.Errorf("a valid screener config should pass, got: %v", err)
	}

	// 0 means "no bound" on either side and must not be treated as a crossed
	// pair.
	if err := (ScreenerConfig{MinPrice: 5}).Validate(); err != nil {
		t.Errorf("min_price alone (max_price unbounded) should pass, got: %v", err)
	}
}

func TestAutotuneConfigRejectsNegativeValuesAndOverlappingSymbolLists(t *testing.T) {
	if err := (AutotuneConfig{Enabled: true, Interval: -time.Hour}).Validate(); err == nil {
		t.Error("negative interval: want a validation error, got nil")
	}

	if err := (AutotuneConfig{Enabled: true, Bars: -1}).Validate(); err == nil {
		t.Error("negative bars: want a validation error, got nil")
	}

	if err := (AutotuneConfig{Enabled: true, MinTrades: -1}).Validate(); err == nil {
		t.Error("negative min_trades: want a validation error, got nil")
	}

	if err := (AutotuneConfig{
		Enabled: true, RunOnlyForSymbols: []string{"GOOG"}, ExcludeSymbols: []string{"GOOG"},
	}).Validate(); err == nil {
		t.Error("GOOG in both run_only_for_symbols and exclude_symbols: want a validation error, got nil")
	}

	// A disabled block is never acted on, so an otherwise-invalid value in
	// it is harmless and must not block loading the rest of the config.
	if err := (AutotuneConfig{Enabled: false, Interval: -time.Hour}).Validate(); err != nil {
		t.Errorf("a disabled autotune block should never fail validation, got: %v", err)
	}

	if err := (AutotuneConfig{
		Enabled: true, Interval: time.Hour, Bars: 1440, MinTrades: 10,
		RunOnlyForSymbols: []string{"GOOG"}, ExcludeSymbols: []string{"AMZN"},
	}).Validate(); err != nil {
		t.Errorf("a valid autotune config should pass, got: %v", err)
	}
}

func TestNotifyConfigRejectsNegativeValues(t *testing.T) {
	if err := (NotifyConfig{MinNotional: -1}).Validate(); err == nil {
		t.Error("negative min_notional: want a validation error, got nil")
	}

	if err := (NotifyConfig{MinNotional: 25}).Validate(); err != nil {
		t.Errorf("a valid notify config should pass, got: %v", err)
	}
}

// TestMLConfigRejectsSwappedProbabilities reproduces the finding's second
// concrete example: a swapped buy_prob/sell_prob used to load clean through
// config.yaml (only the API's PUT /api/settings/ml checked this) and
// silently invert the detector's BUY/SELL logic.
func TestMLConfigRejectsSwappedProbabilities(t *testing.T) {
	if err := (MLConfig{Enabled: true, BuyProb: 0.4, SellProb: 0.6}).Validate(); err == nil {
		t.Error("sell_prob above buy_prob: want a validation error, got nil")
	}

	if err := (MLConfig{Enabled: true, BuyProb: 1.5, SellProb: 0.4}).Validate(); err == nil {
		t.Error("buy_prob outside 0..1: want a validation error, got nil")
	}

	// The crossed check only applies when enabled — a disabled section with
	// stale/placeholder values is not actively deciding anything.
	if err := (MLConfig{Enabled: false, BuyProb: 0.4, SellProb: 0.6}).Validate(); err != nil {
		t.Errorf("a disabled ML section should not be checked for crossed thresholds, got: %v", err)
	}

	if err := (MLConfig{Enabled: true, BuyProb: 0.58, SellProb: 0.42}).Validate(); err != nil {
		t.Errorf("a valid ml config should pass, got: %v", err)
	}
}

// TestLoadRejectsANegativeCashReserve reproduces the finding's own concrete
// example end to end, through Load rather than Validate in isolation: a
// config.yaml with cash_reserve: -100 used to load without error — nothing
// in the YAML path checked Limits at all — even though it silently
// increases spendable headroom instead of reserving any.
func TestLoadRejectsANegativeCashReserve(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	body := "limits:\n  cash_reserve: -100\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := Load(path); err == nil {
		t.Error("a config.yaml with limits.cash_reserve: -100 loaded without error, want a validation error")
	}
}
