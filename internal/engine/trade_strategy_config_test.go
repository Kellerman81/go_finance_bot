package engine

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
)

// TestRecordSnapshotsTheResolvedStrategyConfig reproduces the need to answer
// "which config produced this trade" after the fact: a symbol with its own
// strategy_overrides entry must have ITS parameters snapshotted onto the
// trade record, not the shared base config — otherwise every trade for an
// overridden symbol would misleadingly look like it came from the base
// strategy.
func TestRecordSnapshotsTheResolvedStrategyConfig(t *testing.T) {
	provider := market.NewMock(time.Hour)
	defer provider.Close()

	pf := portfolio.New()
	pf.SetCash(1000)

	brk := broker.NewSim(1000, func(string) (float64, bool) { return 0, false }, nil)
	rm := risk.New(config.Limits{MaxOpenPositions: 10}, config.CostsConfig{})
	prices := NewPriceCache()

	base := config.StrategyConfig{Combine: "weighted", MinStrength: 0.3}

	e := New(
		config.EngineConfig{EvalInterval: time.Hour}, config.ExitsConfig{},
		config.SafetyConfig{}, base,
		provider, brk, rm, pf, prices, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	override := base
	override.Combine = "unanimous"
	override.MinStrength = 0.20
	e.SetSymbolStrategies(map[string]config.StrategyConfig{"GOOG": override})

	e.record(TradeRecord{Symbol: "GOOG", Side: broker.Buy, Status: "dry-run"})
	e.record(TradeRecord{Symbol: "AAPL", Side: broker.Buy, Status: "dry-run"})

	var gotGOOG, gotAAPL config.StrategyConfig

	for _, tr := range e.Trades() {
		switch tr.Symbol {
		case "GOOG":
			if err := json.Unmarshal([]byte(tr.StrategyConfig), &gotGOOG); err != nil {
				t.Fatalf("unmarshal GOOG snapshot: %v", err)
			}
		case "AAPL":
			if err := json.Unmarshal([]byte(tr.StrategyConfig), &gotAAPL); err != nil {
				t.Fatalf("unmarshal AAPL snapshot: %v", err)
			}
		}
	}

	if gotGOOG.Combine != "unanimous" || gotGOOG.MinStrength != 0.20 {
		t.Errorf("GOOG snapshot = %+v, want the override (unanimous/0.20)", gotGOOG)
	}

	if gotAAPL.Combine != "weighted" || gotAAPL.MinStrength != 0.3 {
		t.Errorf("AAPL snapshot = %+v, want the shared base config (weighted/0.3)", gotAAPL)
	}
}
