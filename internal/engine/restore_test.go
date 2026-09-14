package engine

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/errlog"
)

// failingStore reports a configurable error from each load/save call,
// reproducing a storage-layer failure such as a schema column missing from
// storage.legacyColumns (a live database file the running binary's queries
// no longer match).
type failingStore struct {
	nopStore

	recentTradesErr      error
	loadDailySpendErr    error
	loadOpenPositionsErr error
	saveTradeErr         error
}

func (s *failingStore) RecentTrades(int) ([]TradeRecord, error) {
	return nil, s.recentTradesErr
}

func (s *failingStore) LoadDailySpend() (string, float64, error) {
	return "", 0, s.loadDailySpendErr
}

func (s *failingStore) LoadOpenPositions() ([]OpenPosition, error) {
	return nil, s.loadOpenPositionsErr
}

func (s *failingStore) SaveTrade(TradeRecord) error {
	return s.saveTradeErr
}

// restoreEngine builds the minimum Engine Restore touches.
func restoreEngine(st Store) *Engine {
	return &Engine{
		openPos:     make(map[string]*OpenPosition),
		peaks:       make(map[string]float64),
		pendingSell: make(map[string]float64),
		unconfirmed: make(map[string]UnconfirmedOrder),
		pinned:      make(map[string]struct{}),
		names:       make(map[string]string),
		store:       st,
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// hasErrlogEntry reports whether errlog.Recent contains an entry from source
// whose message contains substr.
func hasErrlogEntry(source, substr string) bool {
	for _, e := range errlog.Recent() {
		if e.Source == source && strings.Contains(e.Message, substr) {
			return true
		}
	}

	return false
}

// TestRestoreLoadOpenPositionsFailureIsRecorded reproduces the "a schema
// column missing from legacyColumns fails at the first trade, and the
// failure is swallowed" finding's worst consequence: Restore used to act on
// LoadOpenPositions only when err == nil, silently doing nothing on a
// failure — so the bot could start believing it held no positions while the
// broker showed real holdings, with nothing anywhere pointing an operator at
// why. The failure must now be visible via errlog.
func TestRestoreLoadOpenPositionsFailureIsRecorded(t *testing.T) {
	errlog.Clear()

	e := restoreEngine(&failingStore{loadOpenPositionsErr: errors.New("no such column: adds")})

	if _, err := e.Restore(); err != nil {
		t.Fatalf("Restore returned an error itself: %v", err)
	}

	if len(e.openPos) != 0 {
		t.Fatalf("openPos = %v, want empty on a load failure", e.openPos)
	}

	if !hasErrlogEntry("storage", "restore open positions failed") {
		t.Error("the LoadOpenPositions failure was not recorded to errlog")
	}
}

// The same visibility must hold for the other two silently-swallowed loads.
func TestRestoreRecentTradesAndDailySpendFailuresAreRecorded(t *testing.T) {
	errlog.Clear()

	e := restoreEngine(&failingStore{
		recentTradesErr:   errors.New("no such column: entry_indicators"),
		loadDailySpendErr: errors.New("no such table: kv"),
	})

	if _, err := e.Restore(); err != nil {
		t.Fatalf("Restore returned an error itself: %v", err)
	}

	if !hasErrlogEntry("storage", "restore recent trades failed") {
		t.Error("the RecentTrades failure was not recorded to errlog")
	}

	if !hasErrlogEntry("storage", "restore daily spend failed") {
		t.Error("the LoadDailySpend failure was not recorded to errlog")
	}
}

// A load failure must not prevent Restore from finishing and returning the
// watchlist — a degraded restore is still better than none, and
// syncPortfolio reconciles real holdings from the broker shortly after.
func TestRestoreStillReturnsTheWatchlistDespiteALoadFailure(t *testing.T) {
	st := &failingStore{loadOpenPositionsErr: errors.New("boom")}
	st.nopStore = nopStore{}

	e := restoreEngine(st)

	// LoadWatchlist is unaffected by the other failures; nopStore's default
	// returns (nil, nil), which is a legitimate empty watchlist.
	if _, err := e.Restore(); err != nil {
		t.Fatalf("Restore returned an error despite a healthy LoadWatchlist: %v", err)
	}
}

// TestSaveTradeFailureIsRecorded reproduces the other half of the finding:
// SaveTrade's failure at record() used to be downgraded to a bare log.Warn,
// invisible to anyone not tailing the process log.
func TestSaveTradeFailureIsRecorded(t *testing.T) {
	errlog.Clear()

	e := restoreEngine(&failingStore{saveTradeErr: errors.New("no such column: win")})

	e.record(TradeRecord{Symbol: "AAPL", Status: "filled"})

	if !hasErrlogEntry("storage", "persist trade failed") {
		t.Error("the SaveTrade failure was not recorded to errlog")
	}
}
