package storage

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/engine"
	"github.com/Kellerman81/go_finance_bot/internal/protect"
)

// TestProtectionStateRoundTrip reproduces the halt-not-persisted finding at
// the storage layer: a halt (and the peak it was measured against) must
// survive being written and read back, exactly as it would across a
// restart.
func TestProtectionStateRoundTrip(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if _, ok, err := db.LoadProtectionState(); err != nil || ok {
		t.Fatalf("LoadProtectionState before any save: ok=%v err=%v, want ok=false, err=nil", ok, err)
	}

	haltedAt := time.Now().UTC().Truncate(time.Second)
	want := protect.State{
		Halted: true, HaltReason: "max drawdown 25.0% >= 20.0%", HaltedAt: haltedAt,
		PeakEquity: 12345.67, DayEquity: 11000, Day: "2026-08-23",
	}

	if err := db.SaveProtectionState(want); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, ok, err := db.LoadProtectionState()
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}

	if got.Halted != want.Halted || got.HaltReason != want.HaltReason ||
		!got.HaltedAt.Equal(want.HaltedAt) || got.PeakEquity != want.PeakEquity ||
		got.DayEquity != want.DayEquity || got.Day != want.Day {
		t.Errorf("round trip mismatch:\n got  %+v\n want %+v", got, want)
	}
}

// TestAutotuneAppliesRoundTrip mirrors TestProtectionStateRoundTrip: the
// map of symbol -> last-applied timestamp autotune's own recent-outcome
// gate depends on must survive being written and read back exactly, since
// it's what lets a later pass judge how the LAST config autotune chose for
// a symbol has performed in real trading since.
func TestAutotuneAppliesRoundTrip(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if got, err := db.LoadAutotuneApplies(); err != nil || len(got) != 0 {
		t.Fatalf("LoadAutotuneApplies before any save: got=%v err=%v, want empty, nil", got, err)
	}

	want := map[string]time.Time{
		"AMZN":   time.Now().UTC().Truncate(time.Second),
		"SAP.DE": time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second),
	}

	if err := db.SaveAutotuneApplies(want); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := db.LoadAutotuneApplies()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(got) != len(want) {
		t.Fatalf("round trip mismatch: got %d entries, want %d: got=%+v want=%+v", len(got), len(want), got, want)
	}

	for sym, wantAt := range want {
		gotAt, ok := got[sym]
		if !ok || !gotAt.Equal(wantAt) {
			t.Errorf("%s: got %v (present=%v), want %v", sym, gotAt, ok, wantAt)
		}
	}

	// A second save fully REPLACES the map (mirrors SaveSectors/SaveNames),
	// so a symbol no longer present must not linger.
	if err := db.SaveAutotuneApplies(map[string]time.Time{"AMZN": want["AMZN"]}); err != nil {
		t.Fatalf("save (replace): %v", err)
	}

	got, err = db.LoadAutotuneApplies()
	if err != nil {
		t.Fatalf("load (after replace): %v", err)
	}

	if len(got) != 1 {
		t.Errorf("after replacing with a smaller map, got %d entries, want 1 (SAP.DE should be gone): %+v", len(got), got)
	}
}

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

	stratCfg := `{"detectors":["rsi","macd"],"combine":"weighted","min_strength":0.3}`

	buy := engine.TradeRecord{
		Time: base, Symbol: "AAPL", Side: broker.Buy, Qty: 1, Price: 100, Value: 100,
		Status: "filled", Indicators: buyInd, StrategyConfig: stratCfg,
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

	if gotBuy.StrategyConfig != stratCfg {
		t.Errorf("buy strategy_config mismatch: got %q want %q", gotBuy.StrategyConfig, stratCfg)
	}

	if gotWin.PnL == nil || *gotWin.PnL != 10 || gotWin.PnLPct == nil || *gotWin.PnLPct != 10 ||
		gotWin.Win == nil || !*gotWin.Win || gotWin.EntryIndicators != buyInd || gotWin.Indicators != sellInd {
		t.Errorf("winning sell round-trip mismatch: %+v", gotWin)
	}

	if gotLoss.PnL == nil || *gotLoss.PnL != -5 || gotLoss.Win == nil || *gotLoss.Win {
		t.Errorf("losing sell round-trip mismatch: %+v", gotLoss)
	}
}

// TestSimulatedTradeRoundTripsAndCountsInStats reproduces the test-mode
// provenance requirement: a trade filled while test mode was active is
// tagged Simulated on save/load, but TradeStats must still count it — the
// whole point of test mode is that these feed internal/autotune's own
// real-track-record gating exactly like a real fill would.
func TestSimulatedTradeRoundTripsAndCountsInStats(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	base := time.Now().UTC().Truncate(time.Second)

	real := engine.TradeRecord{
		Time: base, Symbol: "AAPL", Side: broker.Sell, Qty: 1, Price: 110, Value: 110,
		Status: "filled", PnL: ptrF(10), PnLPct: ptrF(10), Win: ptrB(true),
	}
	sim := engine.TradeRecord{
		Time: base.Add(time.Minute), Symbol: "MSFT", Side: broker.Sell, Qty: 1, Price: 90, Value: 90,
		Status: "filled", PnL: ptrF(-5), PnLPct: ptrF(-5), Win: ptrB(false), Simulated: true,
	}

	for _, tr := range []engine.TradeRecord{real, sim} {
		if err := db.SaveTrade(tr); err != nil {
			t.Fatalf("save %+v: %v", tr, err)
		}
	}

	got, err := db.RecentTrades(10)
	if err != nil || len(got) != 2 {
		t.Fatalf("load: %v len=%d", err, len(got))
	}

	gotReal, gotSim := got[0], got[1]

	if gotReal.Simulated {
		t.Error("a real fill round-tripped with Simulated=true")
	}

	if !gotSim.Simulated {
		t.Error("a simulated fill round-tripped with Simulated=false")
	}

	stats, err := db.TradeStats(base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("TradeStats: %v", err)
	}

	if stats.Trades != 2 {
		t.Errorf("TradeStats.Trades = %d, want 2 — a simulated trade must count like a real one", stats.Trades)
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
			PnLPct: ptrF(10), // +10%
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
			PnLPct: ptrF(-4), // -4%
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
	if s := bySym["AAPL"]; abs(s.ExpectancyPct-3) > 1e-9 { // (10 + -4) / 2
		t.Errorf("AAPL expectancy = %v, want 3 (mean of +10%%/-4%%)", s.ExpectancyPct)
	}
	if s := bySym["MSFT"]; s.Trades != 1 || s.Wins != 1 || abs(s.TotalPnL-20) > 1e-9 {
		t.Errorf("MSFT stats mismatch: %+v", s)
	}
}

// TestTradeStatsExitBreakdown reproduces the per-symbol "why is this symbol
// losing" question: an aggregate win rate can't distinguish a symbol whose
// losses come from repeated stop-loss hits (an entry-timing problem) from
// one whose losses come from strategy exits giving back gains (an exit
// problem). Each SymbolStats row must classify its realized closes by
// engine.ClassifyExitKind, keyed off the same "EXIT <kind> ..." reason
// convention exits.go actually writes.
func TestTradeStatsExitBreakdown(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	trades := []engine.TradeRecord{
		{
			Time: now.Add(-time.Hour), Symbol: "GOOG", Side: broker.Sell, Qty: 1,
			Price: 90, Status: "filled", Reason: "EXIT stop-loss 90.00<=100.00",
			PnL: ptrF(-10), Win: ptrB(false),
		},
		{
			Time: now.Add(-2 * time.Hour), Symbol: "GOOG", Side: broker.Sell, Qty: 1,
			Price: 88, Status: "filled", Reason: "EXIT stop-loss 88.00<=100.00",
			PnL: ptrF(-12), Win: ptrB(false),
		},
		{
			Time: now.Add(-3 * time.Hour), Symbol: "GOOG", Side: broker.Sell, Qty: 1,
			Price: 115, Status: "filled",
			Reason: "ema_cross:SELL(0.4) => SELL by weighted", // a strategy exit
			PnL:    ptrF(15), Win: ptrB(true),
		},
		{
			Time: now.Add(-time.Hour), Symbol: "MSFT", Side: broker.Sell, Qty: 1,
			Price: 108, Status: "filled", Reason: "EXIT trailing-stop 108.00<=peak 112.00",
			PnL: ptrF(8), Win: ptrB(true),
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

	bySym := map[string]engine.SymbolStats{}
	for _, s := range stats.BySymbol {
		bySym[s.Symbol] = s
	}

	goog := bySym["GOOG"]
	if goog.ExitBreakdown[engine.ExitKindStopLoss] != 2 {
		t.Errorf("GOOG stop-loss count = %d, want 2 (breakdown: %v)",
			goog.ExitBreakdown[engine.ExitKindStopLoss], goog.ExitBreakdown)
	}

	if goog.ExitBreakdown[engine.ExitKindStrategy] != 1 {
		t.Errorf("GOOG strategy-exit count = %d, want 1 (breakdown: %v)",
			goog.ExitBreakdown[engine.ExitKindStrategy], goog.ExitBreakdown)
	}

	msft := bySym["MSFT"]
	if msft.ExitBreakdown[engine.ExitKindTrailingStop] != 1 {
		t.Errorf("MSFT trailing-stop count = %d, want 1 (breakdown: %v)",
			msft.ExitBreakdown[engine.ExitKindTrailingStop], msft.ExitBreakdown)
	}
}

// TestTradeStatsIncludesAFractionalSecondJustPastTheBoundary reproduces the
// "TradeStats compares RFC3339Nano timestamps as strings" finding: the
// "ts" column is TEXT, compared with a plain SQL "ts >= ?" string
// inequality, not a datetime function — so its on-disk representation has
// to sort identically to chronological order. time.RFC3339Nano trims
// trailing zero fractional digits, so a trade recorded at exactly
// since+500ms used to format as "...00.5Z" while a since with zero
// nanoseconds formats as "...00Z" (no fraction at all) — and "." (0x2E)
// sorts before "Z" (0x5A), so the trade's timestamp string compared as
// LESS than since's, silently dropping it from every stats query that
// should have included it.
func TestTradeStatsIncludesAFractionalSecondJustPastTheBoundary(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	since := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC) // zero nanoseconds

	if err := db.SaveTrade(engine.TradeRecord{
		Time:   since.Add(500 * time.Millisecond), // a nonzero fractional second
		Symbol: "AAPL", Side: broker.Sell, Qty: 1, Price: 110,
		Status: "filled", PnL: ptrF(10), Win: ptrB(true),
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	stats, err := db.TradeStats(since)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}

	if stats.Trades != 1 {
		t.Errorf("Trades = %d, want 1 — a trade 500ms after the query boundary was dropped "+
			"by a string-comparison ordering bug", stats.Trades)
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
