package engine

import "time"

// The engine keeps a symbol -> "when did internal/autotune last apply a
// strategy_overrides change to it" cache, persisted across restarts the
// same way the symbol-name cache is (see names.go). This is what lets a
// future autotune pass judge a symbol not just by how a candidate
// backtests, but by how the LAST config autotune actually chose for that
// symbol has performed in REAL trading since — the whole point being to
// stop readily replacing a config that's been winning, and to make it easy
// to replace one that's been losing. See internal/autotune's own
// RunSymbolOptimize doc comment for how this is used.

// LastAutotuneApply returns the last time internal/autotune (scheduled or
// manual) applied a strategy_overrides change to symbol, and whether one
// has ever been recorded at all.
func (e *Engine) LastAutotuneApply(symbol string) (time.Time, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	t, ok := e.autotuneApplies[symbol]

	return t, ok
}

// RecordAutotuneApply records that autotune just applied a
// strategy_overrides change to symbol, timestamped now — call this only
// once a config.PatchSymbolOverride write has actually succeeded, so the
// recorded time always matches what the file (and this engine) actually
// hold.
func (e *Engine) RecordAutotuneApply(symbol string) {
	e.mu.Lock()
	e.autotuneApplies[symbol] = time.Now()
	e.mu.Unlock()

	e.persistAutotuneApplies()
}

// persistAutotuneApplies writes the cache so a restart does not lose track
// of when each symbol was last touched.
func (e *Engine) persistAutotuneApplies() {
	e.mu.RLock()
	snapshot := make(map[string]time.Time, len(e.autotuneApplies))

	for sym, t := range e.autotuneApplies {
		snapshot[sym] = t
	}
	e.mu.RUnlock()

	if err := e.store.SaveAutotuneApplies(snapshot); err != nil {
		e.log.Warn("persist autotune applies failed", "err", err)
	}
}

// restoreAutotuneApplies loads the persisted cache.
func (e *Engine) restoreAutotuneApplies() {
	applies, err := e.store.LoadAutotuneApplies()
	if err != nil || len(applies) == 0 {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	for sym, t := range applies {
		e.autotuneApplies[sym] = t
	}
}
