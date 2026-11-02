package engine

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/errlog"
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
//
// It is single-flight and reports whether it ran: the startup pass, the
// interval's passes (both off the Run loop) and the API's RescreenNow can
// otherwise overlap — two diffs racing over one watchlist and doubled provider
// load (SC3-1). A panic inside a pass is contained here: the pass runs on its
// own goroutine, outside runSupervised, and a backtester or provider bug must
// not take the live process down with it.
func (e *Engine) rescreen(ctx context.Context) (ran bool) {
	if !e.screening.CompareAndSwap(false, true) {
		e.log.Debug("screener: a pass is already running, skipping")

		return false
	}
	defer e.screening.Store(false)

	defer func() {
		if r := recover(); r != nil {
			e.log.Error("screener: pass panicked; watchlist unchanged", "panic", r)
			errlog.Recordf("screener", "screening pass panicked: %v", r)
		}
	}()

	s := e.screener()
	if s == nil {
		return true
	}

	current := e.Watchlist()
	held := e.heldSymbols()
	pinned := e.Pinned()

	res := s.Screen(ctx, screener.Inputs{Current: current, Held: held, Pinned: pinned})
	if len(res.Verdicts) == 0 {
		return true
	}

	// Only held symbols are protected here. Pinned ones are already in Selected
	// when keep_manual is on, and must stay droppable when it is off — whereas
	// dropping a held symbol is never right, so it is guarded independently of
	// any config flag, screener.keep_held included: that flag only changes
	// the wording of a held-but-failing symbol's verdict in res itself (see
	// config.ScreenerConfig.KeepHeld's own doc comment) — held is always
	// exempted from drop here regardless of its value. See
	// CODE_REVIEW_FINDINGS.md's keep_held-inert finding.
	// A symbol the screener could not MEASURE — its history fetch failed or
	// came back empty — is kept, exactly like a held one. Treating a provider
	// outage as a rejection emptied the watchlist: in a pass where Yahoo was
	// down or rate-limiting, every symbol not held or pinned was dropped, with
	// a notification, and nothing came back until the next pass, 6h later by
	// default (SC2-3).
	// Held symbols are also ADDED back when missing: adoption never touches
	// the watchlist, so a position off it — dropped before its fill was
	// confirmed, or bought by hand — came back only through Selected, which
	// takes keep_held, and with keep_held off never came back (SC3-2).
	add, drop := diffUniverse(current, append(slices.Clone(res.Selected), held...),
		protectedFromDrop(held, res.Verdicts))

	e.recordScreen(res)

	if len(add) == 0 && len(drop) == 0 {
		e.log.Debug("screener: universe unchanged", "tracked", len(current))

		return true
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
		// Audit, not Halt: a screening pass changing the universe is the bot
		// editing itself, not trading stopping. As a Halt it went out one
		// Pushover priority higher (past quiet hours at priority 0), reached
		// operators subscribed to halts only, and competed for the 8-slot
		// urgent lane with real drawdown halts (NT2-4).
		n.Sendf(notify.KindAudit, "watchlist updated",
			"added: %s\ndropped: %s\ntracked: %d",
			orNone(add), orNone(drop), len(e.Watchlist()))
	}

	return true
}

// diffUniverse returns the symbols to add and to drop so that current becomes
// selected, never dropping one that is held.
// protectedFromDrop is what a screening pass may not drop: every held symbol,
// and every symbol the screener could not measure (see rescreen, SC2-3).
func protectedFromDrop(held []string, verdicts []screener.Verdict) []string {
	keep := slices.Clone(held)

	for _, v := range verdicts {
		if v.Unmeasured {
			keep = append(keep, v.Symbol)
		}
	}

	return keep
}

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

// heldSymbols lists every symbol a screening pass must keep tracking: an open
// position record, an unconfirmed buy, a resting limit order, and a broker
// holding with no record yet.
//
// It read openPos alone, under a comment claiming unconfirmed fills counted —
// they do not: an unconfirmed buy is kept out of openPos until reconciled,
// and a broker holding inside the adopt grace has no record either. Both were
// dropped like any other symbol: unsubscribed, bars and peak deleted, ML/AI
// state forgotten, and the fill that then arrived ran with no tick feed —
// no ATR stop, no strategy SELL, no venue price (SC3-2).
func (e *Engine) heldSymbols() []string {
	set := map[string]bool{}

	for sym, h := range e.pf.Snapshot().Holdings {
		if h.Held() {
			set[strings.ToUpper(sym)] = true
		}
	}

	e.mu.RLock()

	for sym, p := range e.openPos {
		if p.Qty > sellDustQty {
			set[strings.ToUpper(sym)] = true
		}
	}

	for _, u := range e.unconfirmed {
		if u.Side == broker.Buy {
			set[strings.ToUpper(u.Symbol)] = true
		}
	}

	for sym := range e.restingWatchers {
		set[strings.ToUpper(sym)] = true
	}

	e.mu.RUnlock()

	return slices.Sorted(maps.Keys(set))
}

// orNone renders a symbol list for a notification, or "none".
func orNone(syms []string) string {
	if len(syms) == 0 {
		return "none"
	}

	return strings.Join(syms, ", ")
}
