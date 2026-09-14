package engine

import (
	"io"
	"log/slog"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
)

// evaluableEngine builds the minimum Engine evaluable touches.
func evaluableEngine(cfg config.EngineConfig, limits config.Limits) *Engine {
	pf := portfolio.New()

	return &Engine{
		cfg:     cfg,
		pf:      pf,
		rm:      risk.New(limits, config.CostsConfig{}),
		streaks: make(map[string]signalStreak),
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestEvaluableResetsTheStreakOfAnExcludedSymbol reproduces the
// "skip_full_slots freezes (not resets) confirmation streaks" finding: a
// symbol skip_full_slots excludes from evaluation is never passed to
// bumpStreak that cycle, so its confirmation streak used to sit exactly
// where it stood — frozen, not reset — for however long the slot stayed
// full. The first evaluation after a slot frees up could then immediately
// extend a streak that started arbitrarily long ago, firing a trade off a
// "confirmation" that was never actually consecutive. evaluable must reset
// (delete) the streak of any symbol it excludes.
func TestEvaluableResetsTheStreakOfAnExcludedSymbol(t *testing.T) {
	e := evaluableEngine(
		config.EngineConfig{SkipFullSlots: true},
		config.Limits{MaxOpenPositions: 1},
	)

	// HELD already fills the one open-position slot; NEW is not held and
	// would be excluded by skip_full_slots.
	e.pf.SetCash(100000)
	e.pf.SyncPositions([]portfolio.Holding{{Symbol: "HELD", Qty: 10}})

	// NEW accumulated a partial confirmation streak before the slot filled.
	e.streaks["NEW"] = signalStreak{action: "BUY", count: 1}

	out := e.evaluable([]string{"HELD", "NEW"})

	if len(out) != 1 || out[0] != "HELD" {
		t.Fatalf("evaluable = %v, want only [HELD] (NEW is unheld with the slot full)", out)
	}

	if _, stillThere := e.streaks["NEW"]; stillThere {
		t.Error("NEW's confirmation streak survived being excluded — " +
			"it will resume instead of starting fresh when a slot frees up")
	}
}

// A held symbol is never excluded, so its streak must survive untouched.
func TestEvaluableLeavesAHeldSymbolsStreakAlone(t *testing.T) {
	e := evaluableEngine(
		config.EngineConfig{SkipFullSlots: true},
		config.Limits{MaxOpenPositions: 1},
	)

	e.pf.SetCash(100000)
	e.pf.SyncPositions([]portfolio.Holding{{Symbol: "HELD", Qty: 10}})
	e.streaks["HELD"] = signalStreak{action: "SELL", count: 1}

	e.evaluable([]string{"HELD"})

	if st, ok := e.streaks["HELD"]; !ok || st.count != 1 {
		t.Errorf("HELD's streak = %+v, ok=%v, want it untouched (count 1)", st, ok)
	}
}

// With slots not actually full, skip_full_slots has nothing to do — every
// symbol is evaluated and no streak is reset.
func TestEvaluableDoesNothingWhenSlotsAreNotFull(t *testing.T) {
	e := evaluableEngine(
		config.EngineConfig{SkipFullSlots: true},
		config.Limits{MaxOpenPositions: 5},
	)

	e.pf.SetCash(100000)
	e.streaks["NEW"] = signalStreak{action: "BUY", count: 1}

	out := e.evaluable([]string{"NEW"})

	if len(out) != 1 || out[0] != "NEW" {
		t.Fatalf("evaluable = %v, want [NEW] (slots are not full)", out)
	}

	if st, ok := e.streaks["NEW"]; !ok || st.count != 1 {
		t.Errorf("NEW's streak = %+v, ok=%v, want it untouched", st, ok)
	}
}
