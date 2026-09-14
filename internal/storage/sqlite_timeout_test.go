package storage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/engine"
)

// TestSaveTradeIsBoundedWhenTheSoleConnectionIsBusy reproduces the "No bound
// on connection hold time" finding: SetMaxOpenConns(1) serializes every call
// through one connection, and every call used bare context.Background(),
// with no bound at all on how long a caller would wait for that connection
// to free up. An uncommitted transaction (standing in for the finding's own
// example — a slow, unindexed TradeStats scan holding the connection for its
// duration) occupies the sole connection here; SaveTrade, needed to book a
// live fill, must give up within roughly dbTimeout rather than blocking for
// as long as the other call happens to take.
func TestSaveTradeIsBoundedWhenTheSoleConnectionIsBusy(t *testing.T) {
	old := dbTimeout
	dbTimeout = 200 * time.Millisecond

	t.Cleanup(func() { dbTimeout = old })

	db, err := OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer db.Close()

	// Occupy the sole connection with a transaction that is never committed
	// for the duration of this test.
	tx, err := db.db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // best-effort cleanup

	start := time.Now()
	err = db.SaveTrade(engine.TradeRecord{
		Time: time.Now(), Symbol: "X", Side: broker.Buy, Qty: 1, Price: 100, Status: "filled",
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("SaveTrade succeeded while the sole connection was held by another transaction")
	}

	if elapsed > 2*time.Second {
		t.Errorf("SaveTrade took %v waiting for the connection, want bounded near "+
			"dbTimeout (%v) — the wait for a free connection must not be unbounded",
			elapsed, dbTimeout)
	}
}
