package storage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/engine"
)

// SG3-3: reason/order_id/err are nullable, but were scanned into plain
// strings — one NULL row (an older binary, a hand fix, an outside tool)
// failed RecentTrades outright, losing all trade history at Restore, and
// failed TradeStats for any window holding the row.
func TestANullTextColumnDoesNotBreakTheTradeHistory(t *testing.T) {
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.db.Exec(`INSERT INTO trades (ts, symbol, side, qty, price, value, status, reason, order_id, err, win)
		VALUES (?, 'AAPL', 'sell', 1, 100, 100, 'filled', NULL, NULL, NULL, 1)`,
		time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}

	if got, err := s.RecentTrades(10); err != nil || len(got) != 1 {
		t.Errorf("RecentTrades = %d rows, %v; want the NULL row read", len(got), err)
	}

	if _, err := s.TradeStats(time.Time{}); err != nil {
		t.Errorf("TradeStats failed on a NULL reason: %v", err)
	}
}

// EX3-5: the trail floor is now kept with the trailing settings it was set
// under; the key must survive a restart, or every floor would read as a
// legacy one and never lapse.
func TestTheTrailFloorKeySurvivesARestart(t *testing.T) {
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	in := engine.OpenPosition{Symbol: "AAPL", Qty: 2, EntryPrice: 105, EntryTime: time.Now(), Peak: 110,
		TrailFloor: 106.7, TrailFloorKey: "0.03/0/0/false/0"}
	if err := s.SaveOpenPosition(in); err != nil {
		t.Fatal(err)
	}

	got, err := s.LoadOpenPositions()
	if err != nil || len(got) != 1 || got[0].TrailFloorKey != in.TrailFloorKey {
		t.Errorf("loaded %+v, %v; want the trail floor key kept", got, err)
	}
}
