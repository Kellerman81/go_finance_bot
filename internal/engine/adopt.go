package engine

import (
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
)

// A position the broker reports but the bot has no durable record for gets
// DEGRADED management, not none: checkExits still applies the percentage and
// ATR stop-loss, the trailing stop and the take-profit, because those read the
// broker snapshot directly. Everything keyed to the durable record silently
// does nothing — the keep_interval hold (heldLongEnough returns "don't block"
// with no record), the max_hold time barrier and the ROI ladder (heldFor
// reports a zero age), the scale-out ladder (checkPartialExit needs OrigQty),
// and realized P&L (closePnL), which in turn means the eventual exit reports
// no P&L and never reaches the stop-loss/low-profit protections at all
// (feedProtections skips a nil PnL).
//
// The usual cause is an order the broker filled but whose outcome the bot's
// own confirmation read never saw — flagUnconfirmed deliberately refuses to
// book a guess into openPos, so act() returns before recordOpen and the
// position exists at the broker with nothing tracking it here.
//
// AdoptHeldPosition has always been able to fix that, but only when an
// operator noticed the Reconciliation panel's "held, not tracked" row and
// clicked it. Until they did, the position traded without any of the above —
// which is how a 2h keep_interval fails to stop a sell twenty minutes after
// the buy. Adopting automatically closes that window: the broker reporting a
// holding IS the evidence that the position is real, and the engine is already
// trading it either way.

// adoptMargin is how long past a limit order's own working time a holding must
// stay untracked before it is adopted. See adoptGrace.
const adoptMargin = time.Minute

// adoptGrace is how long a broker holding must be continuously visible with no
// durable record before adoption. It exists to lose one specific race.
//
// A resting limit order is also "held at the broker with no openPos record":
// watchLimitOrder owns its resolution and calls recordOpen itself once the
// order fills or falls back to market. recordOpen ADDS to an existing record,
// so adopting a position that watchLimitOrder is about to book would count the
// same fill twice and leave openPos.Qty at double the real position — which
// then misreports realized P&L and the scale-out ladder's base quantity.
//
// Waiting out the order's full working time plus a margin means watchLimitOrder
// has always resolved first, so adoption only ever sees positions nothing else
// is going to book. An unconfirmed order never resolves on its own (that is the
// whole point of flagUnconfirmed), so it is adopted once the grace elapses —
// minutes, against a keep_interval measured in hours.
func (e *Engine) adoptGrace() time.Duration {
	return limitTimeout(e.executionConfig()) + adoptMargin
}

// adoptUntrackedHoldings creates a durable record for every broker-reported
// holding that has been untracked for longer than adoptGrace, so the
// record-dependent exits and P&L apply to it.
//
// It is deliberately conservative about what it will not touch:
//
//   - dust rows, which are a just-closed position or a rounding remainder
//     rather than something to manage;
//   - a symbol with a sell in flight, where the broker snapshot may simply not
//     have caught up yet and adopting would resurrect a position that is on
//     its way out;
//   - anything seen untracked for less than adoptGrace.
//
// AdoptHeldPosition itself is the single source of truth for what adoption
// means (entry price and quantity from the broker, entry time now, peak seeded
// at max(avg, last)); this only decides *when* to call it, and is a no-op for
// any symbol already tracked.
func (e *Engine) adoptUntrackedHoldings(holdings []portfolio.Holding) {
	grace := e.adoptGrace()
	candidates := make([]portfolio.Holding, 0, len(holdings))
	seen := make(map[string]struct{}, len(holdings))

	for _, h := range holdings {
		if h.Qty <= sellDustQty {
			continue
		}

		sym := portfolio.Normalize(h.Symbol)

		e.mu.RLock()
		_, tracked := e.openPos[sym]
		_, selling := e.pendingSell[sym]
		e.mu.RUnlock()

		if tracked || selling {
			continue
		}

		seen[sym] = struct{}{}

		h.Symbol = sym
		candidates = append(candidates, h)
	}

	// One locked pass to age the candidates: a symbol that has become tracked,
	// been sold, or stopped being reported drops out, so the clock only ever
	// measures a CONTINUOUS untracked stretch rather than a total across
	// unrelated episodes.
	due := make(map[string]bool, len(candidates))

	e.mu.Lock()

	// Lazily initialised, matching pendingExposure's own driftSince handling:
	// an Engine assembled as a struct literal (every test helper in this
	// package) must not panic here.
	if e.untrackedSince == nil {
		e.untrackedSince = make(map[string]time.Time, len(candidates))
	}

	for sym := range e.untrackedSince {
		if _, still := seen[sym]; !still {
			delete(e.untrackedSince, sym)
		}
	}

	for sym := range seen {
		first, ok := e.untrackedSince[sym]
		if !ok {
			e.untrackedSince[sym] = now()

			continue
		}

		due[sym] = now().Sub(first) >= grace
	}

	e.mu.Unlock()

	for _, h := range candidates {
		if !due[h.Symbol] {
			continue
		}

		// AdoptHeldPosition re-checks "already tracked" under its own lock, so
		// the reads above are only an early-out, not the decision.
		if e.AdoptHeldPosition(h.Symbol) == nil {
			e.log.Warn("adopted an untracked broker holding — until now it traded without "+
				"keep_interval, max_hold, the ROI/scale-out ladders or realized P&L",
				"symbol", h.Symbol, "qty", h.Qty, "entry_price", h.AvgPrice)
		}

		e.mu.Lock()

		delete(e.untrackedSince, h.Symbol)
		e.mu.Unlock()
	}
}
