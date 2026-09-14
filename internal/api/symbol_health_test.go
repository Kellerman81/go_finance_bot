package api

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/engine"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/protect"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
	"github.com/Kellerman81/go_finance_bot/internal/storage"
)

// TestSymbolHealthJoinsStatsAndLocks reproduces the health panel's whole
// point: a symbol's realized track record (from storage, via engine.Stats)
// and its current protection-lock state (from protect.Manager) have to line
// up correctly by symbol, not just both be present somewhere on the page.
// AAPL is a stop-loss-heavy loser and currently locked; MSFT is a clean
// winner and unlocked — the row for each must reflect only its own data.
func TestSymbolHealthJoinsStatsAndLocks(t *testing.T) {
	db, err := storage.OpenSQLite(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	ptrF := func(v float64) *float64 { return &v }
	ptrB := func(v bool) *bool { return &v }

	now := time.Now().UTC()

	trades := []engine.TradeRecord{
		{
			Time: now.Add(-time.Hour), Symbol: "AAPL", Side: broker.Sell, Qty: 1,
			Status: "filled", Reason: "EXIT stop-loss 90.00<=100.00",
			PnL: ptrF(-10), Win: ptrB(false),
		},
		{
			Time: now.Add(-2 * time.Hour), Symbol: "AAPL", Side: broker.Sell, Qty: 1,
			Status: "filled", Reason: "EXIT stop-loss 88.00<=100.00",
			PnL: ptrF(-12), Win: ptrB(false),
		},
		{
			Time: now.Add(-time.Hour), Symbol: "MSFT", Side: broker.Sell, Qty: 1,
			Status: "filled", Reason: "ema_cross:SELL(0.4) => SELL by weighted",
			PnL: ptrF(25), Win: ptrB(true),
		},
	}

	for _, tr := range trades {
		if err := db.SaveTrade(tr); err != nil {
			t.Fatalf("save trade: %v", err)
		}
	}

	prot := protect.New(config.ProtectionsConfig{
		Enabled: true, StopGuardTrades: 2, StopGuardLookback: time.Hour,
		StopGuardLock: time.Hour,
	})
	prot.RecordClose("AAPL", -10, true)
	prot.RecordClose("AAPL", -12, true) // trips the stop guard

	feed := market.NewMock(time.Hour)
	defer feed.Close()

	pf := portfolio.New()
	rm := risk.New(config.Limits{MaxOpenPositions: 10}, config.CostsConfig{})
	prices := engine.NewPriceCache()
	brk := broker.NewSim(10000, prices.Get, nil)

	eng := engine.New(
		config.EngineConfig{}, config.ExitsConfig{}, config.SafetyConfig{},
		config.StrategyConfig{}, feed, brk, rm, pf, prices, db,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	defer eng.Close()

	eng.SetProtections(prot)

	rows := symbolHealth(eng)

	byRow := map[string]symbolHealthRow{}
	for _, r := range rows {
		byRow[r.Symbol] = r
	}

	aapl, ok := byRow["AAPL"]
	if !ok {
		t.Fatal("AAPL missing from symbolHealth")
	}

	if aapl.Trades != 2 || aapl.StopLosses != 2 {
		t.Errorf("AAPL trades/stop-losses = %d/%d, want 2/2", aapl.Trades, aapl.StopLosses)
	}

	if !aapl.Locked || aapl.LockReason != protect.LockStopGuard {
		t.Errorf("AAPL lock = %+v, want locked with reason %q", aapl, protect.LockStopGuard)
	}

	msft, ok := byRow["MSFT"]
	if !ok {
		t.Fatal("MSFT missing from symbolHealth")
	}

	if msft.Trades != 1 || msft.StopLosses != 0 || msft.WinRatePct != 100 {
		t.Errorf("MSFT stats = %+v, want 1 trade, 0 stop-losses, 100%% win rate", msft)
	}

	if msft.Locked {
		t.Errorf("MSFT should not be locked, got %+v", msft)
	}
}
