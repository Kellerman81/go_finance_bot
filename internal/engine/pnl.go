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
	return e.indicatorSnapshotFrom(symbol, e.seriesFor(symbol))
}

// indicatorSnapshotFrom is indicatorSnapshot over an already-computed series,
// avoiding a second full-history copy on the trade-decision path.
func (e *Engine) indicatorSnapshotFrom(symbol string, series []market.Candle) string {
	results := strategy.EvaluateAll(e.StrategyConfigFor(symbol), symbol, series)

	b, err := json.Marshal(results)
	if err != nil {
		e.log.Warn("marshal indicator snapshot failed", "err", err)

		return ""
	}

	return string(b)
}

// closePnL computes realized P&L for selling qty of symbol at exitPrice,
// against the CURRENT open-position entry price, NET of the round trip's
// modelled commission, and returns the entry indicator snapshot to carry
// forward onto the closing trade record.
//
// Netting matters beyond bookkeeping accuracy. internal/backtest books every
// round trip net of commission (see RoundTrip.PnL) and internal/autotune
// compares a candidate's backtested ExpectancyPct/WinRatePct directly against
// this record's — engine.SymbolStats.ExpectancyPct is AVG(pnl_pct) over
// exactly these values. While this was gross, the live side was flattered on
// both metrics (a trade up 1% on a 0.1%-commission account booked 1.000% here
// against the 0.799% a backtest would report, and a marginally-positive trade
// counted as a win live and a loss in the backtest), so the gate was
// comparing two different quantities. See CODE_REVIEW_FINDINGS.md's EN-1.
//
// It MUST be called before recordClose mutates/deletes the openPos entry for
// symbol — all three call sites (act, ManualOrder, exitPosition) already call
// this before recordClose runs. ok is false when there is no open-position
// record for symbol (e.g. a position that predates this feature, or was
// opened outside the bot's own tracking), in which case pnl/pnlPct/win must
// be left nil on the TradeRecord.
func (e *Engine) closePnL(symbol string, exitPrice, qty float64) (realised, bool) {
	e.mu.RLock()

	p, has := e.openPos[symbol]

	var (
		heldQty    float64
		entryComm  float64
		entryPrice float64
		entryInd   string
	)

	if has {
		heldQty, entryComm, entryPrice, entryInd = p.Qty, p.EntryCommission, p.EntryPrice, p.EntryIndicators
	}
	e.mu.RUnlock()

	if !has || entryPrice <= 0 || qty <= 0 {
		return realised{}, false
	}

	// Net of the round trip, not gross. The entry's cost is apportioned by the
	// fraction of the position being sold — the same apportioning
	// internal/backtest does (entryCommission * filledQty/qtyBeforeSell) — so
	// a scale-out rung carries its share and no more, and a flat/minimum fee
	// is never re-charged per tranche.
	entryShare := entryComm
	if heldQty > 0 && qty < heldQty {
		entryShare = entryComm * (qty / heldQty)
	}

	commission := entryShare + e.commissionFor(exitPrice*qty)
	gross := (exitPrice - entryPrice) * qty
	basis := entryPrice * qty

	return realised{
		PnL: gross - commission,
		// Percent of the position's cost basis, so it stays comparable with
		// backtest.RoundTrip.PnLPct ("as % of the position's cost basis").
		PnLPct:          (gross - commission) / basis * 100,
		EntryIndicators: entryInd,
	}, true
}

// commissionFor is the modelled commission for a notional, nil-safe in the
// risk manager so a bare Engine (constructed directly in a test, or before
// wiring completes) books gross rather than panicking. Production always has
// one installed — cmd/bot builds it before the engine.
func (e *Engine) commissionFor(notional float64) float64 {
	if e.rm == nil {
		return 0
	}

	return e.rm.Commission(notional)
}

// realised is one closing sale's outcome.
//
// It is a struct rather than four return values because four unnamed results at
// a call site say nothing about which is which — and two of them are float64,
// so transposing them compiles and silently reports the wrong P&L.
type realised struct {
	PnL             float64
	PnLPct          float64
	EntryIndicators string
}

// apply writes the outcome onto a trade record. A nil outcome leaves the record
// untouched, which is the case for a buy and for a sale of a position the bot
// has no entry record for.
func (r *realised) apply(t *TradeRecord) {
	if r == nil {
		return
	}

	t.PnL = &r.PnL
	t.PnLPct = &r.PnLPct
	t.Win = winFromPnL(r.PnL)
	t.EntryIndicators = r.EntryIndicators
}

// winFromPnL applies the win convention used consistently across the engine
// and the backtester (internal/backtest/backtest.go): pnl >= 0 is a win.
func winFromPnL(pnl float64) *bool {
	w := pnl >= 0
	return &w
}
