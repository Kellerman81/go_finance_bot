package engine

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// The screener may extend the watchlist but, with screener.keep_manual, may not
// prune what the operator chose. That distinction only works if the engine can
// still tell the two apart — including after a restart, when both are just
// strings in the same persisted list.

// pinStore records the pinned set and can replay it, standing in for a restart.
type pinStore struct {
	nopStore

	pinned    []string
	watchlist []string
}

func (s *pinStore) SavePinned(symbols []string) error {
	s.pinned = slices.Clone(symbols)

	return nil
}

func (s *pinStore) LoadPinned() ([]string, error) { return slices.Clone(s.pinned), nil }

func (s *pinStore) SaveWatchlist(symbols []string) error {
	s.watchlist = slices.Clone(symbols)

	return nil
}

func (s *pinStore) LoadWatchlist() ([]string, error) { return slices.Clone(s.watchlist), nil }

// quietProvider satisfies the provider calls AddSymbols makes without doing any
// network work. It reports no history, which leaves warmup a no-op — these
// tests are about watchlist bookkeeping, not indicators.
type quietProvider struct{}

func (quietProvider) Subscribe(...string) error             { return nil }
func (quietProvider) Unsubscribe(...string) error           { return nil }
func (quietProvider) Quotes() <-chan market.Quote           { return nil }
func (quietProvider) LastQuote(string) (market.Quote, bool) { return market.Quote{}, false }
func (quietProvider) Close() error                          { return nil }

func (quietProvider) Candles(
	context.Context, string, market.Resolution, time.Time, time.Time,
) ([]market.Candle, error) {
	return nil, nil
}

// pinEngine is an engine wired for watchlist bookkeeping only.
func pinEngine(st *pinStore) *Engine {
	return &Engine{
		watchlist: make(map[string]struct{}),
		pinned:    make(map[string]struct{}),
		openPos:   make(map[string]*OpenPosition),
		peaks:     make(map[string]float64),
		store:     st,
		provider:  quietProvider{},
		barsStore: newBarStore(maxBars, time.Minute),
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// AddSymbols is the hand-picked path; AddScreened is the discovery path.
func TestAddSymbolsPinsAndAddScreenedDoesNot(t *testing.T) {
	t.Parallel()

	e := pinEngine(&pinStore{})

	if err := e.AddSymbols("MINE"); err != nil {
		t.Fatalf("AddSymbols: %v", err)
	}

	if err := e.AddScreened("FOUND"); err != nil {
		t.Fatalf("AddScreened: %v", err)
	}

	if got := e.Watchlist(); !slices.Equal(got, []string{"FOUND", "MINE"}) {
		t.Errorf("watchlist = %v, want both symbols tracked", got)
	}

	if got := e.Pinned(); !slices.Equal(got, []string{"MINE"}) {
		t.Errorf("pinned = %v, want only MINE — a screener discovery must stay "+
			"prunable", got)
	}
}

// Adding a symbol the screener already found is how an operator adopts it
// permanently, so pinning must apply even when the symbol is already tracked.
// The early "already present" skip in the add loop makes this easy to get wrong.
func TestAddingAnAlreadyTrackedSymbolStillPinsIt(t *testing.T) {
	t.Parallel()

	e := pinEngine(&pinStore{})

	if err := e.AddScreened("FOUND"); err != nil {
		t.Fatalf("AddScreened: %v", err)
	}

	if err := e.AddSymbols("FOUND"); err != nil {
		t.Fatalf("AddSymbols: %v", err)
	}

	if got := e.Pinned(); !slices.Equal(got, []string{"FOUND"}) {
		t.Errorf("pinned = %v, want FOUND adopted.\nAdding a symbol you already "+
			"track by hand is the only way to keep a screener pick, so it must "+
			"pin even though there is nothing new to subscribe to.", got)
	}
}

// TestDropFromScreenPreservesThePin reproduces the "an automated
// (screener-driven) drop is recorded identically to a manual un-pin"
// finding: rescreen's automated drop path used to call RemoveSymbol, the
// same function DELETE /api/watchlist/:symbol uses for a deliberate manual
// choice — which unconditionally un-pins. A symbol the screener drops
// because it stopped ranking is not the same event as an operator
// deciding they no longer want it; if screener.keep_manual is enabled
// AFTER an automated drop already erased the pin, there is nothing left
// for the protection to recover. dropFromScreen must leave the pin intact
// so a later keep_manual: true rescreen (screener.Screen's KeepManual
// handling re-adds anything still in Pinned via appendMissing) can still
// pick the symbol back up.
func TestDropFromScreenPreservesThePin(t *testing.T) {
	t.Parallel()

	e := pinEngine(&pinStore{})

	if err := e.AddSymbols("MINE"); err != nil {
		t.Fatalf("AddSymbols: %v", err)
	}

	e.dropFromScreen("MINE")

	if got := e.Watchlist(); slices.Contains(got, "MINE") {
		t.Errorf("watchlist = %v, want MINE no longer tracked", got)
	}

	if got := e.Pinned(); !slices.Equal(got, []string{"MINE"}) {
		t.Errorf("pinned = %v, want MINE still pinned — an automated drop must not "+
			"un-choose it, or enabling keep_manual afterward could never recover it", got)
	}
}

// The manual removal path, by contrast, must un-pin — that is a deliberate
// operator choice, not an algorithm's.
func TestRemoveSymbolUnpins(t *testing.T) {
	t.Parallel()

	e := pinEngine(&pinStore{})

	if err := e.AddSymbols("MINE"); err != nil {
		t.Fatalf("AddSymbols: %v", err)
	}

	e.RemoveSymbol("MINE")

	if got := e.Pinned(); len(got) != 0 {
		t.Errorf("pinned = %v, want empty — a manual removal is a deliberate un-pin", got)
	}
}

// Re-screening must not demote a symbol the operator pinned.
func TestScreeningAPinnedSymbolDoesNotUnpinIt(t *testing.T) {
	t.Parallel()

	e := pinEngine(&pinStore{})

	if err := e.AddSymbols("MINE"); err != nil {
		t.Fatalf("AddSymbols: %v", err)
	}

	if err := e.AddScreened("MINE"); err != nil {
		t.Fatalf("AddScreened: %v", err)
	}

	if got := e.Pinned(); !slices.Equal(got, []string{"MINE"}) {
		t.Errorf("pinned = %v, want MINE still pinned: the screener happening to "+
			"rediscover a symbol is not the operator changing their mind", got)
	}
}

// Removing a symbol by hand un-chooses it, so re-adding it later starts clean.
func TestRemovingASymbolUnpinsIt(t *testing.T) {
	t.Parallel()

	st := &pinStore{}
	e := pinEngine(st)

	if err := e.AddSymbols("MINE"); err != nil {
		t.Fatalf("AddSymbols: %v", err)
	}

	e.RemoveSymbol("MINE")

	if got := e.Pinned(); len(got) != 0 {
		t.Errorf("pinned = %v, want empty", got)
	}

	if len(st.pinned) != 0 {
		t.Errorf("persisted pinned = %v, want the removal written through", st.pinned)
	}
}

// The whole feature rests on this: after a restart the watchlist is one flat
// list, so without a separately persisted pinned set every symbol would look
// hand-picked and the screener could never prune again.
func TestPinnedSetSurvivesARestart(t *testing.T) {
	t.Parallel()

	st := &pinStore{}
	first := pinEngine(st)

	if err := first.AddSymbols("MINE"); err != nil {
		t.Fatalf("AddSymbols: %v", err)
	}

	if err := first.AddScreened("FOUND"); err != nil {
		t.Fatalf("AddScreened: %v", err)
	}

	// Restart: a fresh engine over the same store, wired the way main.go does
	// it — restore, re-track everything unpinned, then pin what config names.
	second := pinEngine(st)

	persisted, err := second.Restore()
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if err := second.AddScreened(persisted...); err != nil {
		t.Fatalf("AddScreened: %v", err)
	}

	if got := second.Watchlist(); !slices.Equal(got, []string{"FOUND", "MINE"}) {
		t.Fatalf("watchlist after restart = %v, want both symbols back", got)
	}

	if got := second.Pinned(); !slices.Equal(got, []string{"MINE"}) {
		t.Errorf("pinned after restart = %v, want only MINE.\nIf FOUND comes back "+
			"pinned, a screener discovery becomes permanent the first time the "+
			"bot restarts and the watchlist only ever grows.", got)
	}
}

// A fresh install has no persisted pinned set. Restore must cope, and the
// config watchlist that main.go pins afterwards is what ends up chosen.
func TestRestoreWithoutAPersistedPinnedSet(t *testing.T) {
	t.Parallel()

	st := &pinStore{watchlist: []string{"OLD"}}
	e := pinEngine(st)

	persisted, err := e.Restore()
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if err := e.AddScreened(persisted...); err != nil {
		t.Fatalf("AddScreened: %v", err)
	}

	if got := e.Pinned(); len(got) != 0 {
		t.Errorf("pinned = %v, want empty: on upgrade an existing watchlist is as "+
			"likely to be a screener discovery as an operator's pick, and guessing "+
			"'pinned' would freeze a whole screened universe in place", got)
	}
}
