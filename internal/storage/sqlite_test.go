package storage

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/engine"
)

func TestOpenPositionRoundTrip(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	entry := time.Now().UTC().Truncate(time.Second)
	p := engine.OpenPosition{
		Symbol:          "AAPL",
		Qty:             2,
		EntryPrice:      150,
		EntryTime:       entry,
		Peak:            155,
		EntryIndicators: `[{"name":"rsi"}]`,
	}
	if err := db.SaveOpenPosition(p); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := db.LoadOpenPositions()
	if err != nil || len(got) != 1 {
		t.Fatalf("load: %v len=%d", err, len(got))
	}
	if got[0].Symbol != "AAPL" || got[0].Qty != 2 || got[0].Peak != 155 ||
		!got[0].EntryTime.Equal(entry) ||
		got[0].EntryIndicators != p.EntryIndicators {
		t.Errorf("round-trip mismatch: %+v", got[0])
	}

	// Upsert: a higher peak should overwrite, not duplicate.
	p.Peak = 160
	if err := db.SaveOpenPosition(p); err != nil {
		t.Fatal(err)
	}
	got, _ = db.LoadOpenPositions()
	if len(got) != 1 || got[0].Peak != 160 || got[0].EntryIndicators != p.EntryIndicators {
		t.Errorf("upsert failed: %+v", got)
	}

	// Delete closes it out.
	if err := db.DeleteOpenPosition("AAPL"); err != nil {
		t.Fatal(err)
	}
	got, _ = db.LoadOpenPositions()
	if len(got) != 0 {
		t.Errorf("expected no open positions after delete, got %d", len(got))
	}
}

func ptrF(v float64) *float64 { return &v }
func ptrB(v bool) *bool       { return &v }

func TestTradeRoundTripWithPnL(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	base := time.Now().UTC().Truncate(time.Second)
	buyInd := `[{"name":"rsi","action":"BUY","strength":0.8}]`
	sellInd := `[{"name":"rsi","action":"SELL","strength":0.6}]`

	buy := engine.TradeRecord{
		Time: base, Symbol: "AAPL", Side: broker.Buy, Qty: 1, Price: 100, Value: 100,
		Status: "filled", Indicators: buyInd,
	}
	win := engine.TradeRecord{
		Time: base.Add(
			time.Minute,
		),
		Symbol:          "AAPL",
		Side:            broker.Sell,
		Qty:             1,
		Price:           110,
		Value:           110,
		Status:          "filled",
		Indicators:      sellInd,
		EntryIndicators: buyInd,
		PnL:             ptrF(10),
		PnLPct:          ptrF(10),
		Win:             ptrB(true),
	}
	loss := engine.TradeRecord{
		Time: base.Add(
			2 * time.Minute,
		),
		Symbol:          "MSFT",
		Side:            broker.Sell,
		Qty:             1,
		Price:           90,
		Value:           90,
		Status:          "filled",
		Indicators:      sellInd,
		EntryIndicators: buyInd,
		PnL:             ptrF(-5),
		PnLPct:          ptrF(-5),
		Win:             ptrB(false),
	}
	for _, tr := range []engine.TradeRecord{buy, win, loss} {
		if err := db.SaveTrade(tr); err != nil {
			t.Fatalf("save %+v: %v", tr, err)
		}
	}

	got, err := db.RecentTrades(10)
	if err != nil || len(got) != 3 {
		t.Fatalf("load: %v len=%d", err, len(got))
	}
	// RecentTrades returns chronological (oldest first).
	gotBuy, gotWin, gotLoss := got[0], got[1], got[2]

	if gotBuy.PnL != nil || gotBuy.PnLPct != nil || gotBuy.Win != nil {
		t.Errorf("buy row should have nil pnl/win fields, got %+v", gotBuy)
	}
	if gotBuy.Indicators != buyInd {
		t.Errorf("buy indicators mismatch: got %q want %q", gotBuy.Indicators, buyInd)
	}

	if gotWin.PnL == nil || *gotWin.PnL != 10 || gotWin.PnLPct == nil || *gotWin.PnLPct != 10 ||
		gotWin.Win == nil || !*gotWin.Win || gotWin.EntryIndicators != buyInd || gotWin.Indicators != sellInd {
		t.Errorf("winning sell round-trip mismatch: %+v", gotWin)
	}

	if gotLoss.PnL == nil || *gotLoss.PnL != -5 || gotLoss.Win == nil || *gotLoss.Win {
		t.Errorf("losing sell round-trip mismatch: %+v", gotLoss)
	}
}

func TestTradeStats(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	trades := []engine.TradeRecord{
		// AAPL: 1 win (+10), 1 loss (-4) -> total +6
		{
			Time:   now.Add(-time.Hour),
			Symbol: "AAPL",
			Side:   broker.Sell,
			Qty:    1,
			Price:  110,
			Status: "filled",
			PnL:    ptrF(10),
			Win:    ptrB(true),
		},
		{
			Time:   now.Add(-2 * time.Hour),
			Symbol: "AAPL",
			Side:   broker.Sell,
			Qty:    1,
			Price:  96,
			Status: "filled",
			PnL:    ptrF(-4),
			Win:    ptrB(false),
		},
		// MSFT: 1 win (+20)
		{
			Time:   now.Add(-3 * time.Hour),
			Symbol: "MSFT",
			Side:   broker.Sell,
			Qty:    1,
			Price:  120,
			Status: "filled",
			PnL:    ptrF(20),
			Win:    ptrB(true),
		},
		// Outside the 30-day window entirely — must be excluded.
		{
			Time:   now.AddDate(0, 0, -40),
			Symbol: "MSFT",
			Side:   broker.Sell,
			Qty:    1,
			Price:  130,
			Status: "filled",
			PnL:    ptrF(30),
			Win:    ptrB(true),
		},
		// An open buy (no realized outcome) — must be excluded regardless of window.
		{
			Time:   now.Add(-time.Hour),
			Symbol: "GOOG",
			Side:   broker.Buy,
			Qty:    1,
			Price:  50,
			Status: "filled",
		},
	}
	for _, tr := range trades {
		if err := db.SaveTrade(tr); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	stats, err := db.TradeStats(now.AddDate(0, 0, -30))
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Trades != 3 || stats.Wins != 2 || stats.Losses != 1 {
		t.Fatalf("unexpected totals: %+v", stats)
	}
	if want := 2.0 / 3.0 * 100; abs(stats.WinRatePct-want) > 1e-9 {
		t.Errorf("win rate: got %v want %v", stats.WinRatePct, want)
	}
	if abs(stats.TotalPnL-26) > 1e-9 {
		t.Errorf("total pnl: got %v want 26", stats.TotalPnL)
	}
	if abs(stats.AvgWin-15) > 1e-9 { // (10+20)/2
		t.Errorf("avg win: got %v want 15", stats.AvgWin)
	}
	if abs(stats.AvgLoss-(-4)) > 1e-9 {
		t.Errorf("avg loss: got %v want -4", stats.AvgLoss)
	}
	if len(stats.BySymbol) != 2 {
		t.Fatalf("expected 2 symbols, got %d: %+v", len(stats.BySymbol), stats.BySymbol)
	}
	bySym := map[string]engine.SymbolStats{}
	for _, s := range stats.BySymbol {
		bySym[s.Symbol] = s
	}
	if s := bySym["AAPL"]; s.Trades != 2 || s.Wins != 1 || s.Losses != 1 ||
		abs(s.TotalPnL-6) > 1e-9 {
		t.Errorf("AAPL stats mismatch: %+v", s)
	}
	if s := bySym["MSFT"]; s.Trades != 1 || s.Wins != 1 || abs(s.TotalPnL-20) > 1e-9 {
		t.Errorf("MSFT stats mismatch: %+v", s)
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// legacySchema mirrors the trades/open_positions tables exactly as they
// existed before pnl/win/indicators columns were added, to prove OpenSQLite
// migrates a pre-existing database file in place.
const legacySchema = `
CREATE TABLE IF NOT EXISTS trades (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	ts         TEXT NOT NULL,
	symbol     TEXT NOT NULL,
	side       TEXT NOT NULL,
	qty        REAL NOT NULL,
	price      REAL NOT NULL,
	value      REAL NOT NULL,
	status     TEXT NOT NULL,
	reason     TEXT,
	order_id   TEXT,
	err        TEXT
);
CREATE INDEX IF NOT EXISTS idx_trades_symbol ON trades(symbol);
CREATE TABLE IF NOT EXISTS kv (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS open_positions (
	symbol      TEXT PRIMARY KEY,
	qty         REAL NOT NULL,
	entry_price REAL NOT NULL,
	entry_time  TEXT NOT NULL,
	peak        REAL NOT NULL
);
`

func TestSQLiteMigratesLegacyColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(legacySchema); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	db, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite on legacy db: %v", err)
	}
	defer db.Close()

	tr := engine.TradeRecord{
		Time: time.Now().UTC(), Symbol: "AAPL", Side: broker.Sell, Qty: 1, Price: 100,
		Status: "filled", PnL: ptrF(5), Win: ptrB(true), Indicators: `[{"name":"rsi"}]`,
	}
	if err := db.SaveTrade(tr); err != nil {
		t.Fatalf("save after migration: %v", err)
	}
	got, err := db.RecentTrades(10)
	if err != nil || len(got) != 1 {
		t.Fatalf("load after migration: %v len=%d", err, len(got))
	}
	if got[0].PnL == nil || *got[0].PnL != 5 || got[0].Win == nil || !*got[0].Win ||
		got[0].Indicators != tr.Indicators {
		t.Errorf("post-migration round-trip mismatch: %+v", got[0])
	}
}
