package engine

import (
	"errors"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/notify"
)

// Some instruments a broker will price, chart and resolve, but never sell you.
// The live case that prompted this: QQQ is US-domiciled, and PRIIPs requires a
// Key Information Document that US issuers do not produce, so Trading 212
// answers an EU account's buy with `/api-errors/instrument-invisible` —
// "Instrument can not be traded".
//
// Nothing about that changes on the next evaluation. Without a guard the engine
// re-sent the order every eval_interval for as long as the signal held: a
// rejected order every 30 seconds, each one an API call against a broker with a
// strict rate limit, each one another error row in the trade log.
//
// The guard is deliberately one-directional. It suppresses ENTRIES only, and is
// only ever set from a failed buy. If a sell were ever to be refused this way
// the position must still be exitable — a guard that blocked exits would strand
// stock with no way out, which is far worse than a noisy log.

// noteUntradeable records a permanent instrument-level refusal so the bot stops
// sending orders the broker has already said it will not accept.
//
// Only buys arm it: see the file comment. A transient failure (rate limit, 5xx,
// insufficient funds) is not wrapped in ErrInstrumentUnavailable by the broker
// and so never reaches here.
func (e *Engine) noteUntradeable(symbol string, side broker.Side, err error) {
	if side != broker.Buy || !errors.Is(err, broker.ErrInstrumentUnavailable) {
		return
	}

	e.mu.Lock()

	_, already := e.untradeable[symbol]
	if !already {
		e.untradeable[symbol] = err.Error()
	}

	e.mu.Unlock()

	if already {
		return // announce it once, not once per evaluation
	}

	e.log.Error("broker refuses to trade this instrument; no further entries "+
		"will be attempted for it",
		"symbol", symbol, "err", err)

	if n := e.notifyOf(); n != nil {
		// An error, not a halt: one instrument refused, trading continues. As a
		// Halt every refused instrument re-announced on the urgent lane after
		// a restart, all in one tick — enough of them filled its 8 slots and a
		// real drawdown halt arriving then was dropped (NT2-4).
		n.Sendf(notify.KindError, "instrument not tradeable",
			"%s: the broker will not accept orders for this instrument. "+
				"Entries are suspended for it until restart.\n\n%v", symbol, err)
	}
}

// announcedOrderError reports whether this exact order failure has already been
// announced for the symbol, and records it when it has not. A different failure
// (or the same one after a success in between) is announced again.
//
// It exists because a failing order is retried every evaluation for as long as
// the signal holds, and each attempt used to raise its own notification: the
// reported case was "sim: no price for NTA.F" arriving every 30 seconds,
// indefinitely, for a symbol whose live feed never ticks. noteUntradeable
// already applies this "announce it once, not once per evaluation" rule to a
// broker's permanent instrument refusal; this is the same rule for every other
// repeating failure, including ones on the SELL side, which noteUntradeable
// deliberately never covers.
//
// The trade-log row is still written every time — the log is the honest record
// of what the bot actually attempted, and an operator investigating needs to
// see the cadence. Only the notification is suppressed.
func (e *Engine) announcedOrderError(symbol, signature string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.lastOrderErr == nil {
		e.lastOrderErr = make(map[string]string)
	}

	if e.lastOrderErr[symbol] == signature {
		return true
	}

	e.lastOrderErr[symbol] = signature

	return false
}

// clearOrderError forgets a symbol's last failure, so the next one is announced
// even if it is identical to the one before it. Called when an order for the
// symbol succeeds: the condition evidently cleared, and a recurrence is news
// again rather than a continuation.
func (e *Engine) clearOrderError(symbol string) {
	e.mu.Lock()
	delete(e.lastOrderErr, symbol)
	e.mu.Unlock()
}

// entryGates runs the checks every buy shares, whatever put it in motion: has
// the broker refused this instrument outright, and is its venue open. It returns
// the reason to record when an entry is declined.
//
// Both apply to a scale-in exactly as they do to a fresh signal — an add is new
// money at a venue, not a continuation of the original order — and scale-in was
// the one entry path that had neither.
func (e *Engine) entryGates(symbol string) (reason string, ok bool) {
	if refused, why := e.entryBlocked(symbol); refused {
		return "broker refuses this instrument: " + why, false
	}

	if e.cfg.MarketHoursCheck {
		if open, why := e.entryAllowed(symbol); !open {
			return "market closed: " + why, false
		}
	}

	return "", true
}

// entryBlocked reports whether the broker has permanently refused this
// instrument, and why. Consulted on the buy path only.
func (e *Engine) entryBlocked(symbol string) (bool, string) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	why, blocked := e.untradeable[symbol]

	return blocked, why
}

// Untradeable lists the symbols the broker has refused, for the API and UI.
// The map is in-memory on purpose: it is the broker's answer to a question only
// the broker can answer, so a restart should ask again rather than trust a
// verdict that may be months old.
func (e *Engine) Untradeable() map[string]string {
	e.mu.RLock()
	defer e.mu.RUnlock()

	out := make(map[string]string, len(e.untradeable))
	for sym, why := range e.untradeable {
		out[sym] = why
	}

	return out
}
