package engine

import (
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
)

// Loss-triggered re-tuning (config.AutotuneOnLossConfig).
//
// The scheduled autotune pass runs on a clock. A symbol whose config stops
// working an hour after the last pass keeps trading that broken config until
// the next one — up to a day of losing trades on a config the bot could
// already tell was failing. The engine is the only place that sees a realized
// close the moment it happens, so it is where the counting belongs.
//
// The engine cannot call internal/autotune (that package imports this one), so
// it does not try to. It counts losses, and when a symbol's streak reaches the
// configured counter it publishes the symbol on a channel that autotune's own
// scheduler goroutine selects on alongside its ticker. The decision to tune,
// and every safeguard around it, stays in autotune where the rest of that
// judgement already lives.

// lossTuneQueue bounds the pending-request channel. A tune is a heavyweight
// search and only one runs at a time, so this only ever needs to absorb the
// case of several symbols closing losing trades in the same instant — far more
// than that in flight means the searches cannot keep up, and dropping is the
// right answer rather than growing an unbounded backlog of stale requests.
const lossTuneQueue = 64

// defaultAfterLosses is AutotuneOnLossConfig.AfterLosses' documented zero-value
// fallback, applied here rather than in config.Default() so a config written
// before this field existed behaves identically to one that explicitly says 0.
const defaultAfterLosses = 3

// LossTuneRequests returns the channel of symbols whose consecutive-loss
// counter has reached autotune.on_loss.after_losses.
//
// Receiving a symbol here is a request to CONSIDER re-tuning it, never a
// verdict that its config is bad: the search that follows applies the same
// gates a scheduled pass does, and is expected to decline most of the time.
func (e *Engine) LossTuneRequests() <-chan string { return e.lossTune }

// noteRealizedClose updates symbol's consecutive-loss counter from one closed
// trade and publishes a tune request when the counter reaches the configured
// threshold.
//
// Simulated (test-mode) closes count, deliberately. Test mode exists so that
// simulated fills feed the real trade-stats machinery and autotune's own
// gating — the scheduled pass already judges symbols on them — so excluding
// them here would make the reactive trigger the one part of autotune that goes
// blind during the training phase it was designed to learn from.
func (e *Engine) noteRealizedClose(t TradeRecord) {
	// Only a realized closing sell carries an outcome. A blocked record is a
	// decision not to trade, not a loss.
	if t.Side != broker.Sell || t.PnL == nil || t.Status == "blocked" {
		return
	}

	cfg := e.AutotuneConfig()
	dropAfter := e.ProtectionsConfig().DropAfterLosses

	tuneOn := cfg.Enabled && cfg.OnLoss.Enabled
	if !tuneOn && dropAfter <= 0 {
		// Nothing counts the streak, so nothing should accumulate one:
		// re-enabling a trigger later must not fire immediately off a run of
		// losses recorded while it was off.
		e.clearLossStreak(t.Symbol)

		return
	}

	// A win or a breakeven close means the symbol is still working. Reset
	// rather than decrement: this counts streaks, not a running score.
	if *t.PnL >= 0 {
		e.clearLossStreak(t.Symbol)

		return
	}

	streak := e.bumpLossStreak(t.Symbol)

	// Giving up is checked first and wins: once a symbol is off the
	// watchlist there is nothing left to re-tune, and asking for a search on
	// the way out would waste a slot on a symbol already abandoned.
	if dropAfter > 0 && streak >= dropAfter {
		if e.dropAfterLosses(t.Symbol, streak, dropAfter) {
			return
		}
	}

	if !tuneOn {
		return
	}

	after := cfg.OnLoss.AfterLosses
	if after <= 0 {
		after = defaultAfterLosses
	}

	e.log.Debug("autotune on_loss: losing close recorded",
		"symbol", t.Symbol, "streak", streak, "after_losses", after)

	// Fire every `after` losses rather than resetting the counter on the
	// first trigger. Resetting made the streak mean "losses since we last did
	// something", which is not what a consecutive-loss count should say — and
	// it capped the number at `after`, so protections.drop_after_losses could
	// never be reached while on_loss was enabled. The modulo keeps the
	// re-tune cadence identical (every N in a row) while letting one honest
	// counter serve both triggers.
	if streak%after != 0 {
		return
	}

	if !e.LossTuneCooldownPassed(t.Symbol, cfg.OnLoss.Cooldown) {
		e.log.Info("autotune on_loss: streak reached but symbol is in cooldown",
			"symbol", t.Symbol, "streak", streak, "cooldown", cfg.OnLoss.Cooldown)

		return
	}

	select {
	case e.lossTune <- t.Symbol:
		e.log.Info("autotune on_loss: requesting a re-tune",
			"symbol", t.Symbol, "consecutive_losses", streak)
	default:
		// Never block the trading loop on a full queue — the same rule the
		// notifier follows. A dropped request costs one re-tune opportunity;
		// a blocked record() call would stall every decision behind it.
		e.log.Warn("autotune on_loss: request queue full, dropping",
			"symbol", t.Symbol)
	}
}

// LossTuneCooldownPassed reports whether enough time has passed since autotune
// last applied a change to symbol. A cooldown of 0 disables the check.
//
// It measures from the last APPLY rather than the last request: a request that
// found nothing worth changing did not rewrite the symbol's config, so it is
// no reason to stop looking again.
func (e *Engine) LossTuneCooldownPassed(symbol string, cooldown time.Duration) bool {
	if cooldown <= 0 {
		return true
	}

	last, ok := e.LastAutotuneApply(symbol)
	if !ok {
		return true
	}

	return time.Since(last) >= cooldown
}

// bumpLossStreak increments symbol's consecutive-loss counter and returns the
// new value.
func (e *Engine) bumpLossStreak(symbol string) int {
	e.mu.Lock()
	e.lossStreaks[symbol]++
	streak := e.lossStreaks[symbol]
	e.mu.Unlock()

	e.persistLossStreaks()

	return streak
}

// clearLossStreak resets symbol's counter. It is a no-op when the counter is
// already zero, so a long run of winning trades does not rewrite the store on
// every close.
func (e *Engine) clearLossStreak(symbol string) {
	e.mu.Lock()

	if e.lossStreaks[symbol] == 0 {
		e.mu.Unlock()

		return
	}

	delete(e.lossStreaks, symbol)
	e.mu.Unlock()

	e.persistLossStreaks()
}

// LossStreaks returns a copy of the per-symbol consecutive-loss counters, for
// the API and the UI: "why did this symbol just get re-tuned?" and "how close
// is this one?" are the two questions an operator actually has.
func (e *Engine) LossStreaks() map[string]int {
	e.mu.RLock()
	defer e.mu.RUnlock()

	out := make(map[string]int, len(e.lossStreaks))
	for sym, n := range e.lossStreaks {
		out[sym] = n
	}

	return out
}

// persistLossStreaks writes the counters so a restart does not silently
// forgive a streak in progress — losing three in a row across a restart is
// still losing three in a row.
func (e *Engine) persistLossStreaks() {
	if err := e.store.SaveLossStreaks(e.LossStreaks()); err != nil {
		e.log.Warn("persist loss streaks failed", "err", err)
	}
}

// restoreLossStreaks loads the persisted counters.
func (e *Engine) restoreLossStreaks() {
	streaks, err := e.store.LoadLossStreaks()
	if err != nil || len(streaks) == 0 {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	for sym, n := range streaks {
		if n > 0 {
			e.lossStreaks[sym] = n
		}
	}
}
