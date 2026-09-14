package engine

import (
	"fmt"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/errlog"
	"github.com/Kellerman81/go_finance_bot/internal/notify"
)

// Giving up on a symbol (protections.drop_after_losses).
//
// Every other per-symbol guard in this codebase locks an instrument out for a
// while and then lets it back in. That is right for a bad patch and wrong for a
// bad match: some instruments simply do not suit the strategy, and a lockout
// that expires hands capital back to them every time it does.
//
// This is the one that gives up. It removes the symbol from the running
// watchlist AND comments its entry out of config.yaml, because a drop that only
// changed the running state would not survive a restart — cmd/bot re-adds every
// symbol in watchlist: on start, so the bot would abandon an instrument and then
// immediately take it back.
//
// Commented rather than deleted, so the symbol, the reason and the date stay in
// the file and undoing it is removing one "#".

// SetConfigPath tells the engine which file to edit when a protection has to
// write back — today only drop_after_losses. Empty (the default) means the
// engine never touches a config file, and a drop then applies to the running
// watchlist only, which cmd/bot's own startup would undo.
func (e *Engine) SetConfigPath(path string) {
	e.mu.Lock()
	e.configPath = path
	e.mu.Unlock()
}

// configFile returns the path to write protection changes back to.
func (e *Engine) configFile() string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.configPath
}

// dropAfterLosses removes symbol from the watchlist after a losing run, and
// reports whether it actually did.
//
// It returns false — leaving the streak intact so the next loss tries again —
// when the symbol still has an open position. Dropping a held symbol would
// strand it: no exits, no trailing stop, and no way for the strategy to sell
// it. The same rule the screener applies to held symbols, for the same reason,
// and it means a losing position is closed on its own terms first and dropped
// on the close after.
func (e *Engine) dropAfterLosses(symbol string, streak, after int) bool {
	if h, held := e.pf.Holding(symbol); held && h.Qty > sellDustQty {
		e.log.Warn("drop_after_losses: symbol is at its limit but still held — "+
			"waiting for the position to close",
			"symbol", symbol, "streak", streak, "qty", h.Qty)

		return false
	}

	reason := droppedReason(streak, after, now())

	// RemoveSymbol, not dropFromScreen: this IS a decision to stop trading the
	// instrument, not a ranking that may reverse next pass, so the pin goes
	// too. Leaving it pinned would have screener.keep_manual put it straight
	// back on the watchlist.
	e.RemoveSymbol(symbol)
	e.clearLossStreak(symbol)

	commented := e.commentOutOfConfig(symbol, reason)

	e.log.Warn("drop_after_losses: symbol removed from the watchlist",
		"symbol", symbol, "streak", streak, "max", after, "config_updated", commented)

	e.record(TradeRecord{
		Time: now(), Symbol: symbol, Status: "blocked", Reason: reason,
	})

	e.Notify(notify.Event{
		Kind: notify.KindAudit, Symbol: symbol,
		Title: "symbol dropped from the watchlist",
		Text:  fmt.Sprintf("%s: %s", symbol, reason),
	})

	return true
}

// commentOutOfConfig comments symbol out of the config file's watchlist block,
// reporting whether the file actually changed.
//
// Every failure here is logged and swallowed rather than propagated: the symbol
// is already off the running watchlist, and the useful part of the drop has
// happened. What a failure costs is durability — a restart re-adds the symbol —
// so it is recorded where an operator will see it instead of being retried
// silently against a file that may be read-only by design.
func (e *Engine) commentOutOfConfig(symbol, reason string) bool {
	path := e.configFile()
	if path == "" {
		return false
	}

	changed, err := config.CommentOutWatchlistSymbol(path, symbol, reason)
	if err != nil {
		e.log.Error("drop_after_losses: could not update the config file",
			"symbol", symbol, "path", path, "err", err)
		errlog.Recordf("config", "comment out %s from watchlist: %v", symbol, err)

		return false
	}

	if !changed {
		// Ordinary for a screener discovery: it was never in the file. Worth a
		// line at debug so an operator chasing "why did it come back" can see
		// which of the two cases they are in.
		e.log.Debug("drop_after_losses: no watchlist entry in the config file",
			"symbol", symbol, "path", path)
	}

	return changed
}

// droppedReason renders the note written beside the commented-out entry and
// into the trade log, so the file and the UI say the same thing.
func droppedReason(streak, after int, when time.Time) string {
	return fmt.Sprintf(
		"dropped by drop_after_losses: %d consecutive losing closes (max %d), %s",
		streak, after, when.Format(time.DateOnly))
}
