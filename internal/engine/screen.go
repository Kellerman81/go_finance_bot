package engine

import (
	"context"
	"slices"
	"strings"

	"github.com/Kellerman81/go_finance_bot/internal/notify"
	"github.com/Kellerman81/go_finance_bot/internal/screener"
)

// SetScreener installs automatic universe selection. Passing nil (the default)
// leaves the watchlist entirely under config and API control. Call before Run:
// the screening ticker is started there.
func (e *Engine) SetScreener(s *screener.Screener) {
	e.mu.Lock()

	e.screen = s
	e.mu.Unlock()
}

// screener returns the installed screener, which may be nil (safe to call).
func (e *Engine) screener() *screener.Screener {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.screen
}

// LastScreen returns the most recent screening pass, for the API and UI. The
// zero value means it has not run yet.
func (e *Engine) LastScreen() screener.Result {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.lastScreen
}

// recordScreen stores a screening pass and the spreads it measured.
//
// The spreads are kept even for rejected candidates: with costs.measure_spread
// on they are what the cost model charges, and on a candle-only feed this
// estimate is the only bid-ask figure available anywhere in the bot.
func (e *Engine) recordScreen(res screener.Result) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.lastScreen = res

	for _, v := range res.Verdicts {
		if v.SpreadPct > 0 {
			e.spreads[v.Symbol] = v.SpreadPct
		}
	}
}

// rescreen runs one screening pass and reconciles the watchlist with its
// verdict: symbols that newly qualify are added (and warmed up), symbols that no
// longer qualify are dropped.
//
// Two sets survive a pass regardless of their verdict. A symbol the bot still
// holds stays on the watchlist even when it fails every filter — removing it
// would leave the position with no exits, no trailing stop and no way to be
// sold. And with screener.keep_manual, so does anything the operator picked by
// hand. The screener decides what may be *entered*, not what may be forgotten.
func (e *Engine) rescreen(ctx context.Context) {
	s := e.screener()
	if s == nil {
		return
	}

	current := e.Watchlist()
	held := e.heldSymbols()
	pinned := e.Pinned()

	res := s.Screen(ctx, screener.Inputs{Current: current, Held: held, Pinned: pinned})
	if len(res.Verdicts) == 0 {
		return
	}

	// Only held symbols are protected here. Pinned ones are already in Selected
	// when keep_manual is on, and must stay droppable when it is off — whereas
	// dropping a held symbol is never right, so it is guarded independently of
	// any config flag, screener.keep_held included: that flag only changes
	// the wording of a held-but-failing symbol's verdict in res itself (see
	// config.ScreenerConfig.KeepHeld's own doc comment) — held is always
	// exempted from drop here regardless of its value. See
	// CODE_REVIEW_FINDINGS.md's keep_held-inert finding.
	add, drop := diffUniverse(current, res.Selected, held)

	e.recordScreen(res)

	if len(add) == 0 && len(drop) == 0 {
		e.log.Debug("screener: universe unchanged", "tracked", len(current))

		return
	}

	for _, sym := range drop {
		// dropFromScreen, not RemoveSymbol: an automated drop must not
		// un-pin — see dropFromScreen's doc comment.
		e.dropFromScreen(sym)
	}

	if len(add) > 0 {
		// AddScreened, not AddSymbols: a discovery must stay unpinned so a
		// later pass can drop it again once it stops ranking.
		if err := e.AddScreened(add...); err != nil {
			e.log.Warn("screener: add symbols failed", "err", err)
		}
	}

	e.log.Info("screener: universe updated",
		"added", add, "dropped", drop, "tracked", len(e.Watchlist()))

	if n := e.notifyOf(); n != nil {
		n.Sendf(notify.KindHalt, "watchlist updated",
			"added: %s\ndropped: %s\ntracked: %d",
			orNone(add), orNone(drop), len(e.Watchlist()))
	}
}

// diffUniverse returns the symbols to add and to drop so that current becomes
// selected, never dropping one that is held.
func diffUniverse(current, selected, held []string) (add, drop []string) {
	want := make(map[string]bool, len(selected))
	for _, s := range selected {
		want[strings.ToUpper(s)] = true
	}

	have := make(map[string]bool, len(current))
	for _, s := range current {
		have[strings.ToUpper(s)] = true
	}

	heldSet := make(map[string]bool, len(held))
	for _, s := range held {
		heldSet[strings.ToUpper(s)] = true
	}

	for sym := range want {
		if !have[sym] {
			add = append(add, sym)
		}
	}

	for sym := range have {
		if !want[sym] && !heldSet[sym] {
			drop = append(drop, sym)
		}
	}

	slices.Sort(add)
	slices.Sort(drop)

	return add, drop
}

// heldSymbols lists symbols with an open position, from the bot's own durable
// records rather than the broker snapshot, so a position whose fill has not been
// confirmed yet still counts as held.
func (e *Engine) heldSymbols() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	out := make([]string, 0, len(e.openPos))
	for sym, p := range e.openPos {
		if p.Qty > 0 {
			out = append(out, sym)
		}
	}

	return out
}

// orNone renders a symbol list for a notification, or "none".
func orNone(syms []string) string {
	if len(syms) == 0 {
		return "none"
	}

	return strings.Join(syms, ", ")
}
