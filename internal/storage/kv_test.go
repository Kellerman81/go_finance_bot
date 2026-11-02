package storage

import (
	"path/filepath"
	"reflect"
	"testing"
)

func openKV(t *testing.T) *SQLite {
	t.Helper()

	db, err := OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}

// TestKVCachesRoundTrip guards the restart path of every JSON-in-kv cache the
// engine restores at startup: the watchlist, the pinned set, the name cache
// and the loss streaks. A key typo in either half of a Save/Load pair loses
// the state silently — Load answers "nothing saved" rather than an error — so
// a broken pair shows up only as a bot that forgot its screened watchlist, or
// a drop_after_losses streak that restarts at zero on every deploy.
func TestKVCachesRoundTrip(t *testing.T) {
	t.Parallel()

	db := openKV(t)

	// Before any save every Load answers empty, not an error: a fresh install
	// must start, not fail.
	if w, err := db.LoadWatchlist(); err != nil || w != nil {
		t.Fatalf("LoadWatchlist before save = %v, %v; want nil, nil", w, err)
	}

	if p, err := db.LoadPinned(); err != nil || p != nil {
		t.Fatalf("LoadPinned before save = %v, %v; want nil, nil", p, err)
	}

	if n, err := db.LoadNames(); err != nil || n != nil {
		t.Fatalf("LoadNames before save = %v, %v; want nil, nil", n, err)
	}

	if s, err := db.LoadLossStreaks(); err != nil || s != nil {
		t.Fatalf("LoadLossStreaks before save = %v, %v; want nil, nil", s, err)
	}

	watch := []string{"AAPL", "SAP.DE", "BTCUSDT"}
	pinned := []string{"SAP.DE"}
	names := map[string]string{"AAPL": "Apple Inc.", "SAP.DE": "SAP SE"}
	streaks := map[string]int{"AAPL": 2, "SAP.DE": 0}

	if err := db.SaveWatchlist(watch); err != nil {
		t.Fatalf("SaveWatchlist: %v", err)
	}

	if err := db.SavePinned(pinned); err != nil {
		t.Fatalf("SavePinned: %v", err)
	}

	if err := db.SaveNames(names); err != nil {
		t.Fatalf("SaveNames: %v", err)
	}

	if err := db.SaveLossStreaks(streaks); err != nil {
		t.Fatalf("SaveLossStreaks: %v", err)
	}

	// The four caches must not share a key: each reads back its own value.
	if got, err := db.LoadWatchlist(); err != nil || !reflect.DeepEqual(got, watch) {
		t.Errorf("LoadWatchlist = %v, %v; want %v", got, err, watch)
	}

	if got, err := db.LoadPinned(); err != nil || !reflect.DeepEqual(got, pinned) {
		t.Errorf("LoadPinned = %v, %v; want %v", got, err, pinned)
	}

	if got, err := db.LoadNames(); err != nil || !reflect.DeepEqual(got, names) {
		t.Errorf("LoadNames = %v, %v; want %v", got, err, names)
	}

	if got, err := db.LoadLossStreaks(); err != nil || !reflect.DeepEqual(got, streaks) {
		t.Errorf("LoadLossStreaks = %v, %v; want %v", got, err, streaks)
	}

	// A save REPLACES: removing a symbol from the watchlist must not be
	// undone by the next restart.
	if err := db.SaveWatchlist([]string{"AAPL"}); err != nil {
		t.Fatalf("SaveWatchlist (replace): %v", err)
	}

	if got, _ := db.LoadWatchlist(); !reflect.DeepEqual(got, []string{"AAPL"}) {
		t.Errorf("LoadWatchlist after replace = %v, want [AAPL]", got)
	}
}

// TestDailySpendRoundTrip guards max_daily_spend across a restart: the
// rolling spend is persisted as "day:amount" and parsed back with Sscanf, the
// parse CLAUDE.md flags as fragile. A parse that silently failed would hand
// the risk manager a zero spend and re-open the whole day's budget after
// every restart.
func TestDailySpendRoundTrip(t *testing.T) {
	t.Parallel()

	db := openKV(t)

	if day, amt, err := db.LoadDailySpend(); err != nil || day != "" || amt != 0 {
		t.Fatalf("LoadDailySpend before save = %q, %v, %v; want empty", day, amt, err)
	}

	if err := db.SaveDailySpend("2026-11-01", 1234.56); err != nil {
		t.Fatalf("SaveDailySpend: %v", err)
	}

	day, amt, err := db.LoadDailySpend()
	if err != nil {
		t.Fatalf("LoadDailySpend: %v", err)
	}

	if day != "2026-11-01" || abs(amt-1234.56) > 1e-6 {
		t.Errorf("LoadDailySpend = %q, %v; want 2026-11-01, 1234.56", day, amt)
	}

	// A malformed value is "start fresh", deliberately not an error that
	// would stop the bot from starting.
	if err := db.kvSet("daily_spend", "garbage"); err != nil {
		t.Fatalf("kvSet: %v", err)
	}

	if day, amt, err := db.LoadDailySpend(); err != nil || day != "" || amt != 0 {
		t.Errorf("LoadDailySpend(malformed) = %q, %v, %v; want empty, nil", day, amt, err)
	}
}
