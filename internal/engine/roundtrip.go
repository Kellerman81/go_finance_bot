package engine

// roundTripOutcome accumulates the tranches of one position's exit, so the
// protections judge the round trip rather than each slice of it.
type roundTripOutcome struct {
	pnl     float64
	stopped bool
}

// closeTranche folds one realised sell into symbol's round trip and reports
// whether that sell closes it, with the trip's cumulative P&L and whether any
// tranche of it was a protective stop.
//
// Feeding every tranche to protect.RecordClose, as before, made a two-rung
// scale-out ladder look like three trades to low_profit_trades, counted one
// partially filled stop-loss twice toward stop_guard_trades, and judged a
// trade that banked +5% on its rungs by the -0.1% of its last slice — a loss
// cooldown for a winner. The backtest already fed only completed trades, so
// live and offline also disagreed (PR2-6).
//
// The accumulator is in memory only: a restart mid-exit judges the trip on
// the tranches since the restart, which is what the old per-tranche rule did
// for every trade. A sell with no open-position record closes at once.
func (e *Engine) closeTranche(
	symbol string, qty, pnl float64, stopped bool,
) (roundTripOutcome, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.roundTrip == nil {
		e.roundTrip = make(map[string]roundTripOutcome)
	}

	prev := e.roundTrip[symbol]
	acc := roundTripOutcome{pnl: prev.pnl + pnl, stopped: prev.stopped || stopped}

	// Same threshold recordClose uses to delete the record (sellDustQty,
	// PR3-1), read before it runs: record() — and so this — precedes
	// recordClose at every fill site.
	if p, ok := e.openPos[symbol]; ok && p.Qty-qty > sellDustQty {
		e.roundTrip[symbol] = acc

		return acc, false
	}

	delete(e.roundTrip, symbol)

	return acc, true
}
