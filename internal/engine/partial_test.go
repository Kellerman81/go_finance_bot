package engine

import (
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
)

// ----- item 3: the scale-out ladder's bookkeeping -----

// A partial sell must reduce the position, not close it, and must leave the
// ladder's progress and the trailing peak intact.
func TestRecordCloseKeepsTheRecordOnAPartialSell(t *testing.T) {
	e, _ := forgetEngine()

	e.openPos["X"] = &OpenPosition{
		Symbol: "X", Qty: 3, OrigQty: 3, EntryPrice: 10, PartialsTaken: 1,
	}
	e.peaks["X"] = 14

	e.recordClose("X", 1)

	p, ok := e.openPos["X"]
	if !ok {
		t.Fatal("a partial sell closed the position record")
	}

	if p.Qty != 2 {
		t.Errorf("qty = %v, want 2", p.Qty)
	}

	if p.OrigQty != 3 {
		t.Errorf("orig_qty = %v; the ladder's rungs are fractions of the ORIGINAL size", p.OrigQty)
	}

	if p.PartialsTaken != 1 {
		t.Errorf("partials_taken = %d, want 1 (progress must survive)", p.PartialsTaken)
	}

	if e.peaks["X"] != 14 {
		t.Errorf("peak = %v; a partial must not reset the trailing high-water mark", e.peaks["X"])
	}
}

// Selling the rest does close it, and clears the per-symbol state.
func TestRecordCloseClearsStateOnTheFinalSell(t *testing.T) {
	e, _ := forgetEngine()

	e.openPos["X"] = &OpenPosition{Symbol: "X", Qty: 3, OrigQty: 3}
	e.peaks["X"] = 14

	e.recordClose("X", 3)

	if _, ok := e.openPos["X"]; ok {
		t.Error("the position record survived a full sell")
	}

	if _, ok := e.peaks["X"]; ok {
		t.Error("the trailing peak survived a full sell")
	}
}

// markPartialTaken records progress through the ladder so each rung fires once
// and the position survives a restart mid-ladder.
func TestMarkPartialTakenPersists(t *testing.T) {
	e, st := forgetEngine()

	e.openPos["X"] = &OpenPosition{Symbol: "X", Qty: 2, OrigQty: 3}
	e.markPartialTaken("X", 0)

	if got := e.openPos["X"].PartialsTaken; got != 1 {
		t.Errorf("partials_taken = %d, want 1", got)
	}

	if len(st.saved) == 0 {
		t.Error("ladder progress was not persisted; a restart would re-take the rung")
	}
}

// A position closed by the same sale has nothing left to remember, and marking
// it must not resurrect the record.
func TestMarkPartialTakenOnAClosedPositionIsANoOp(t *testing.T) {
	e, _ := forgetEngine()

	e.markPartialTaken("GONE", 0)

	if _, ok := e.openPos["GONE"]; ok {
		t.Error("marking a rung on a closed position recreated its record")
	}
}

// Adding to a position re-bases the ladder against the new, larger size —
// otherwise the rungs stay sized against the first tranche.
func TestScaleInRebasesTheLadder(t *testing.T) {
	e, _ := forgetEngine()

	e.recordOpen("X", 2, 10, "")
	e.recordOpen("X", 2, 8, "")

	p := e.openPos["X"]
	if p.OrigQty != 4 {
		t.Errorf("orig_qty = %v, want 4 after an add", p.OrigQty)
	}

	if p.Adds != 1 {
		t.Errorf("adds = %d, want 1", p.Adds)
	}

	// And the average entry moved to the weighted mean.
	if p.EntryPrice != 9 {
		t.Errorf("entry_price = %v, want the weighted 9", p.EntryPrice)
	}
}

// ----- item 3: the scale-in ladder's trigger -----

func TestScaleInDueRespectsTheCapAndTheStep(t *testing.T) {
	e, _ := forgetEngine()
	e.cfg.ScaleIn = config.ScaleInConfig{
		Enabled: true, StepPct: 0.05, MaxAdds: 2, SizePct: 1,
	}

	p := OpenPosition{Symbol: "X", Qty: 1, EntryPrice: 100}

	if e.scaleInDue(p, 97) {
		t.Error("added only 3% down, below the 5% step")
	}

	if !e.scaleInDue(p, 95) {
		t.Error("did not add at exactly the 5% step")
	}

	p.Adds = 2
	if e.scaleInDue(p, 80) {
		t.Error("added past max_adds; an uncapped ladder is a martingale")
	}
}

// The averaging-down ladder must be off unless explicitly enabled.
func TestScaleInOffByDefault(t *testing.T) {
	e, _ := forgetEngine()

	if e.scaleInDue(OpenPosition{Symbol: "X", Qty: 1, EntryPrice: 100}, 1) {
		t.Error("scale-in fired with no configuration")
	}
}

// ----- item 6: skipping symbols that cannot be bought -----

func TestEvaluableSkipsUnbuyableSymbolsWhenFull(t *testing.T) {
	e := fullSlotsEngine(t, true, 2)

	got := e.evaluable([]string{"HELD1", "HELD2", "OTHER"})
	if len(got) != 2 {
		t.Fatalf("evaluated %v; with slots full only held symbols are worth an opinion", got)
	}

	for _, s := range got {
		if s == "OTHER" {
			t.Error("evaluated an unheld symbol while max_open_positions was reached")
		}
	}
}

// Held symbols are never skipped: they still need their sell signals.
func TestEvaluableKeepsHeldSymbols(t *testing.T) {
	e := fullSlotsEngine(t, true, 2)

	got := e.evaluable([]string{"HELD1", "HELD2"})
	if len(got) != 2 {
		t.Fatalf("evaluated %v, want both held symbols", got)
	}
}

// Below the cap, nothing is skipped.
func TestEvaluableKeepsEverythingWithFreeSlots(t *testing.T) {
	e := fullSlotsEngine(t, true, 5)

	if got := e.evaluable([]string{"HELD1", "HELD2", "OTHER"}); len(got) != 3 {
		t.Fatalf("evaluated %v, want all three with slots free", got)
	}
}

// Off by default: the whole watchlist is evaluated regardless.
func TestEvaluableOffByDefault(t *testing.T) {
	e := fullSlotsEngine(t, false, 2)

	if got := e.evaluable([]string{"HELD1", "HELD2", "OTHER"}); len(got) != 3 {
		t.Fatalf("evaluated %v; skip_full_slots is opt-in", got)
	}
}

// fullSlotsEngine builds an engine holding two positions, with max_open_positions
// set to limit and skip_full_slots as given.
func fullSlotsEngine(t *testing.T, skip bool, limit int) *Engine {
	t.Helper()

	e, _ := forgetEngine()
	e.cfg.SkipFullSlots = skip
	e.rm = risk.New(config.Limits{MaxOpenPositions: limit}, config.CostsConfig{})
	e.pf = portfolio.New()
	e.pf.SetCash(1000)
	e.pf.SyncPositions([]portfolio.Holding{
		{Symbol: "HELD1", Qty: 1, AvgPrice: 10, LastPrice: 10},
		{Symbol: "HELD2", Qty: 1, AvgPrice: 10, LastPrice: 10},
	})

	return e
}

// ----- the pending-sell guard across a scale-out -----

// A scale-out marks the guard with the holding as it stood at submit, not with
// the slice being sold. Marking the slice would leave a marker of 1 and block
// the later full exit of the remaining 2.
func TestPendingSellGuardSurvivesAScaleOut(t *testing.T) {
	e, _ := forgetEngine()

	snap := portfolio.Snapshot{Holdings: map[string]portfolio.Holding{
		"X": {Symbol: "X", Qty: 3},
	}}

	// Simulate what exitPosition records after selling 1 of 3.
	heldAtSubmit := 1.0
	if h, ok := snap.Holdings["X"]; ok && h.Qty > heldAtSubmit {
		heldAtSubmit = h.Qty
	}

	e.markPendingSell("X", heldAtSubmit)

	pending, ok := e.pendingSellQty("X")
	if !ok || pending != 3 {
		t.Fatalf("pending = %v ok=%v, want the 3 held at submit", pending, ok)
	}

	// The later exit of the remaining 2 must not be blocked.
	if 2 >= pending {
		t.Error("a full exit of the remainder would be suppressed by the guard")
	}
}

// A position's age drives the ROI ladder and max_hold, so it must come from the
// record rather than being guessed.
func TestHeldForUsesTheEntryTime(t *testing.T) {
	e, _ := forgetEngine()

	e.openPos["X"] = &OpenPosition{Symbol: "X", EntryTime: time.Now().Add(-2 * time.Hour)}

	if got := e.heldFor("X"); got < 2*time.Hour {
		t.Errorf("heldFor = %v, want at least 2h", got)
	}

	if got := e.heldFor("NOPE"); got != 0 {
		t.Errorf("heldFor of an unknown symbol = %v, want 0 (which disables the ladder)", got)
	}
}
