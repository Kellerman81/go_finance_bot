package engine

import (
	"encoding/json"

	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// indicatorSnapshot returns a JSON-marshaled snapshot of every available
// detector's current read for symbol (strategy.EvaluateAll output — action,
// strength, reason, whether it's in the active buy/sell set). It is the
// "what were the indicators saying" record attached to every trade decision,
// and (on a buy) carried forward on the resulting OpenPosition so it can
// later be copied onto the closing sell. Returns "" if the snapshot can't be
// marshaled.
func (e *Engine) indicatorSnapshot(symbol string) string {
	return e.indicatorSnapshotFrom(e.seriesFor(symbol))
}

// indicatorSnapshotFrom is indicatorSnapshot over an already-computed series,
// avoiding a second full-history copy on the trade-decision path.
func (e *Engine) indicatorSnapshotFrom(series []market.Candle) string {
	results := strategy.EvaluateAll(e.StrategyConfig(), series)
	b, err := json.Marshal(results)
	if err != nil {
		e.log.Warn("marshal indicator snapshot failed", "err", err)
		return ""
	}
	return string(b)
}

// closePnL computes realized P&L for selling qty of symbol at exitPrice,
// against the CURRENT open-position entry price, and returns the entry
// indicator snapshot to carry forward onto the closing trade record.
//
// It MUST be called before recordClose mutates/deletes the openPos entry for
// symbol — all three call sites (act, ManualOrder, exitPosition) already call
// this before recordClose runs. ok is false when there is no open-position
// record for symbol (e.g. a position that predates this feature, or was
// opened outside the bot's own tracking), in which case pnl/pnlPct/win must
// be left nil on the TradeRecord.
func (e *Engine) closePnL(symbol string, exitPrice, qty float64) (pnl, pnlPct float64, entryIndicators string, ok bool) {
	e.mu.RLock()
	p, has := e.openPos[symbol]
	e.mu.RUnlock()
	if !has || p.EntryPrice <= 0 || qty <= 0 {
		return 0, 0, "", false
	}
	pnl = (exitPrice - p.EntryPrice) * qty
	pnlPct = (exitPrice/p.EntryPrice - 1) * 100
	return pnl, pnlPct, p.EntryIndicators, true
}

// winFromPnL applies the win convention used consistently across the engine
// and the backtester (internal/backtest/backtest.go): pnl >= 0 is a win.
func winFromPnL(pnl float64) *bool {
	w := pnl >= 0
	return &w
}
