package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// A watchlist of LXS.DE, XD5E.DE and PRAJL.XC says nothing about what the bot
// holds. The name cache is what makes it readable — and it is persisted, because
// a name never changes and re-fetching twenty of them on every start would be a
// network round trip per symbol to re-learn that BAYN.DE is Bayer.

// nameStore records the persisted cache and can replay it across a "restart".
type nameStore struct {
	nopStore

	names  map[string]string
	writes int

	sectors      map[string]string
	sectorWrites int

	assetClasses map[string]string
	classWrites  int

	applies     map[string]time.Time
	applyWrites int
}

func (s *nameStore) SaveNames(names map[string]string) error {
	s.names = names
	s.writes++

	return nil
}

func (s *nameStore) LoadNames() (map[string]string, error) { return s.names, nil }

func (s *nameStore) SaveSectors(sectors map[string]string) error {
	s.sectors = sectors
	s.sectorWrites++

	return nil
}

func (s *nameStore) LoadSectors() (map[string]string, error) { return s.sectors, nil }

func (s *nameStore) SaveAssetClasses(classes map[string]string) error {
	s.assetClasses = classes
	s.classWrites++

	return nil
}

func (s *nameStore) LoadAssetClasses() (map[string]string, error) { return s.assetClasses, nil }

func (s *nameStore) SaveAutotuneApplies(applies map[string]time.Time) error {
	s.applies = applies
	s.applyWrites++

	return nil
}

func (s *nameStore) LoadAutotuneApplies() (map[string]time.Time, error) { return s.applies, nil }

// fakeSearcher answers from a fixed table and counts lookups. sectors is
// optional (symbol -> sector, e.g. "Technology") — most tests only care
// about names and leave it nil, which yields an empty Sector, exactly like
// Yahoo's response does for a fund/ETF or anything it can't classify.
type fakeSearcher struct {
	table   map[string]string
	sectors map[string]string
	types   map[string]string // symbol -> provider instrument type (e.g. "ETF")
	lookups int
}

func (f *fakeSearcher) Search(_ context.Context, q string, _ int) ([]market.Instrument, error) {
	if name, ok := f.table[q]; ok {
		return []market.Instrument{
			{Symbol: q, Name: name, Sector: f.sectors[q], Type: f.types[q]},
		}, nil
	}

	return nil, nil
}

// Candles makes it installable as the engine's history source, which is where
// the engine looks for a searcher first.
func (f *fakeSearcher) Candles(
	context.Context, string, market.Resolution, time.Time, time.Time,
) ([]market.Candle, error) {
	return nil, nil
}

func (f *fakeSearcher) Lookup(_ context.Context, sym string) (market.Instrument, bool) {
	f.lookups++

	name, ok := f.table[sym]
	if !ok {
		return market.Instrument{}, false
	}

	return market.Instrument{
		Symbol: sym, Name: name, Sector: f.sectors[sym], Type: f.types[sym],
	}, true
}

// nameEngine wires an engine with a searcher and a recording store.
func nameEngine(st *nameStore, s *fakeSearcher) *Engine {
	e := pinEngine(&pinStore{})
	e.names = make(map[string]string)
	e.sectors = make(map[string]string)
	e.assetClasses = make(map[string]string)
	e.autotuneApplies = make(map[string]time.Time)
	e.store = st

	if s != nil {
		e.history = s
	}

	return e
}

// A name learned from a picked search result costs no lookup.
func TestSetSymbolNamePersists(t *testing.T) {
	t.Parallel()

	st := &nameStore{}
	e := nameEngine(st, nil)

	e.SetSymbolName("BAYN.DE", "Bayer Aktiengesellschaft")

	if got := e.SymbolName("BAYN.DE"); got != "Bayer Aktiengesellschaft" {
		t.Errorf("name = %q, want the company name", got)
	}

	if st.writes != 1 {
		t.Errorf("wrote %d times, want 1", st.writes)
	}
}

// Writing the same name again must not churn the store: every add re-asserts
// names it already knows.
func TestSetSymbolNameIsIdempotent(t *testing.T) {
	t.Parallel()

	st := &nameStore{}
	e := nameEngine(st, nil)

	e.SetSymbolName("BAYN.DE", "Bayer Aktiengesellschaft")
	e.SetSymbolName("BAYN.DE", "Bayer Aktiengesellschaft")

	if st.writes != 1 {
		t.Errorf("wrote %d times for one distinct name, want 1", st.writes)
	}
}

// The cache survives a restart, or every start would re-fetch every name.
func TestNamesSurviveARestart(t *testing.T) {
	t.Parallel()

	st := &nameStore{}
	first := nameEngine(st, nil)

	first.SetSymbolName("LXS.DE", "LANXESS Aktiengesellschaft")

	second := nameEngine(st, nil)
	second.restoreNames()

	if got := second.SymbolName("LXS.DE"); got != "LANXESS Aktiengesellschaft" {
		t.Errorf("name after restart = %q, want it restored from the store", got)
	}
}

// SymbolNames hands out a copy, not the engine's own map.
func TestSymbolNamesSnapshotIsACopy(t *testing.T) {
	t.Parallel()

	e := nameEngine(&nameStore{}, nil)
	e.SetSymbolName("SAP.DE", "SAP SE")

	snap := e.SymbolNames()
	delete(snap, "SAP.DE")

	if e.SymbolName("SAP.DE") == "" {
		t.Error("mutating the snapshot changed the engine's cache")
	}
}

// An unknown name is blank, never a guess — the view falls back to the ticker.
func TestUnknownSymbolHasNoName(t *testing.T) {
	t.Parallel()

	e := nameEngine(&nameStore{}, nil)

	if got := e.SymbolName("NOPE"); got != "" {
		t.Errorf("name = %q, want empty for a symbol never seen", got)
	}
}

// Search must report "cannot look" distinctly from "found nothing".
func TestSearchWithoutAProviderIsAnError(t *testing.T) {
	t.Parallel()

	e := nameEngine(&nameStore{}, nil)

	_, err := e.SearchInstruments(context.Background(), "bayer", 10)
	if !errors.Is(err, errNoSearcher) {
		t.Errorf("err = %v, want errNoSearcher.\n\"Search is unavailable\" and "+
			"\"no matches\" need different messages to someone typing a name.", err)
	}

	if e.CanSearch() {
		t.Error("CanSearch is true with no searching provider")
	}
}

// Symbols that joined the watchlist before this feature existed have no name.
// resolveNames backfills them, which is what start-up re-adding the persisted
// watchlist triggers.
func TestResolveNamesBackfillsUnknownSymbols(t *testing.T) {
	t.Parallel()

	search := &fakeSearcher{table: map[string]string{
		"BAYN.DE": "Bayer Aktiengesellschaft",
		"LXS.DE":  "LANXESS Aktiengesellschaft",
	}}
	e := nameEngine(&nameStore{}, search)

	e.resolveNames(context.Background(), []string{"BAYN.DE", "LXS.DE"})

	if got := e.SymbolName("BAYN.DE"); got != "Bayer Aktiengesellschaft" {
		t.Errorf("BAYN.DE = %q, want it resolved", got)
	}

	if got := e.SymbolName("LXS.DE"); got != "LANXESS Aktiengesellschaft" {
		t.Errorf("LXS.DE = %q, want it resolved", got)
	}
}

// A name already known must not be looked up again. The whole watchlist is
// re-added on every start, so this is the difference between zero requests and
// one per symbol, forever.
func TestResolveNamesSkipsWhatItAlreadyKnows(t *testing.T) {
	t.Parallel()

	search := &fakeSearcher{table: map[string]string{"SAP.DE": "SAP SE"}}
	e := nameEngine(&nameStore{}, search)

	// Every field resolveNames fills is already known (e.g. from config.yaml),
	// so there is nothing left to look up.
	e.SetSymbolName("SAP.DE", "SAP SE")
	e.sectors["SAP.DE"] = "tech"
	e.assetClasses["SAP.DE"] = "stock"

	e.resolveNames(context.Background(), []string{"SAP.DE"})

	if search.lookups != 0 {
		t.Errorf("looked up %d times for a symbol whose name, sector AND asset class "+
			"are already known, want 0", search.lookups)
	}
}

// A symbol whose name is already known but whose sector isn't must still be
// looked up — resolveNames checks the two independently, so an install that
// already resolved names before this feature existed backfills sectors on
// its very next restart instead of never revisiting these symbols again.
func TestResolveNamesStillFetchesAMissingSectorWithAKnownName(t *testing.T) {
	t.Parallel()

	search := &fakeSearcher{
		table:   map[string]string{"SAP.DE": "SAP SE"},
		sectors: map[string]string{"SAP.DE": "Technology"},
	}
	e := nameEngine(&nameStore{}, search)

	e.SetSymbolName("SAP.DE", "SAP SE") // name already known from before

	if e.hasSector("SAP.DE") {
		t.Fatal("sector should not be known yet")
	}

	e.resolveNames(context.Background(), []string{"SAP.DE"})

	if search.lookups != 1 {
		t.Errorf("looked up %d times, want 1 (a known name must not suppress a missing-sector lookup)",
			search.lookups)
	}

	e.mu.RLock()
	got := e.sectors["SAP.DE"]
	e.mu.RUnlock()

	if got != "technology" {
		t.Errorf("sector = %q, want %q (learned and lowercased)", got, "technology")
	}
}

// A symbol the provider cannot name is left blank rather than retried forever
// or filled with a guess. PRAJL.XC and UNPRF are exactly this case.
func TestResolveNamesToleratesAnUnknownSymbol(t *testing.T) {
	t.Parallel()

	search := &fakeSearcher{table: map[string]string{}}
	e := nameEngine(&nameStore{}, search)

	e.resolveNames(context.Background(), []string{"PRAJL.XC"})

	if got := e.SymbolName("PRAJL.XC"); got != "" {
		t.Errorf("name = %q, want empty rather than a guess", got)
	}
}

// A big watchlist arriving at once must not become a burst of search requests.
func TestResolveNamesIsBounded(t *testing.T) {
	t.Parallel()

	table := map[string]string{}
	syms := make([]string, 0, 40)

	for i := range 40 {
		sym := string(rune('A'+i%26)) + string(rune('0'+i/26))
		table[sym] = "Name of " + sym

		syms = append(syms, sym)
	}

	search := &fakeSearcher{table: table}
	e := nameEngine(&nameStore{}, search)

	e.resolveNames(context.Background(), syms)

	if search.lookups > nameLookupLimit {
		t.Errorf("made %d lookups for %d symbols; the per-pass cap is %d",
			search.lookups, len(syms), nameLookupLimit)
	}
}

// main.go wraps the history source in a cache before the engine sees it, so a
// plain `historySource().(market.Searcher)` finds the wrapper and concludes
// search is unavailable. The picker would have vanished in production while
// every test passed, because tests install the provider bare.
func TestSearchIsFoundThroughTheCachingWrapper(t *testing.T) {
	t.Parallel()

	search := &fakeSearcher{table: map[string]string{"BAYN.DE": "Bayer Aktiengesellschaft"}}
	e := nameEngine(&nameStore{}, nil)
	e.history = market.NewCachingHistory(search, time.Second)

	if !e.CanSearch() {
		t.Fatal("search was not found through the cache decorator that main.go " +
			"always installs; the picker would be hidden in production")
	}

	got, err := e.SearchInstruments(context.Background(), "BAYN.DE", 5)
	if err != nil || len(got) != 1 {
		t.Fatalf("SearchInstruments = %v, %v; want one match", got, err)
	}
}

// The wrapper must not manufacture the capability: a source that cannot search
// must still report unavailable, or the UI offers a picker that finds nothing.
func TestCacheDoesNotInventSearch(t *testing.T) {
	t.Parallel()

	e := nameEngine(&nameStore{}, nil)
	e.history = market.NewCachingHistory(plainHistory{}, time.Second)

	if e.CanSearch() {
		t.Error("wrapping a non-searching source in the cache made it look " +
			"searchable")
	}
}

// plainHistory fetches candles and nothing else.
type plainHistory struct{}

func (plainHistory) Candles(
	context.Context, string, market.Resolution, time.Time, time.Time,
) ([]market.Candle, error) {
	return nil, nil
}
