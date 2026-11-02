package engine

// persistOpenPosition writes symbol's CURRENT open-position record to the
// store — saved if it exists, deleted if it does not — under persistMu.
//
// Every writer used to snapshot the record under e.mu and write the snapshot
// after unlocking, and the store has no version check, so two goroutines could
// land their writes in the opposite order to their in-memory changes. A peak
// save from checkExits that lost the race to a watchLimitOrder sell's delete
// re-inserted the closed position, and the next start restored a phantom
// holding that stops and max_hold then acted on (SG2-3).
//
// Re-reading under one mutex makes the order irrelevant: whichever persist
// runs last writes the state as it stands then, and every mutation is followed
// by its own persist. One mutex rather than one per symbol — SQLite is
// serialised to a single connection anyway.
//
// Nothing is written while test mode is active: those records are simulated,
// and EndTestMode restores and re-persists the real ones (EN3-1 / SG3-2).
func (e *Engine) persistOpenPosition(symbol, what string) {
	e.persistMu.Lock()
	defer e.persistMu.Unlock()

	e.mu.RLock()

	if e.testModeActive {
		e.mu.RUnlock()

		return
	}

	p, live := e.openPos[symbol]

	var snap OpenPosition

	if live {
		snap = *p
	}

	e.mu.RUnlock()

	var err error

	if live {
		err = e.store.SaveOpenPosition(snap)
	} else {
		err = e.store.DeleteOpenPosition(symbol)
	}

	if err != nil {
		e.log.Warn("persist "+what+" failed", "symbol", symbol, "err", err)
	}
}
