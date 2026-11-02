package engine

import (
	"strings"
	"time"
	"unicode"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
)

// Some reasons a buy is refused are CONDITIONS, not events.
//
// A stop that stays wide, an account that stays full, a symbol the data
// provider only has daily candles for — each persists for as long as it
// persists. The evaluation loop revisits every symbol every eval_interval
// (30s by default), so recording one of these per evaluation produces the
// same blocked trade twice a minute indefinitely, and `alert` turns every
// blocked trade into a notification.
//
// That failure keeps recurring here. A symbol whose venue could not be priced
// once produced 332 identical blocked trades across a weekend; by the time
// this guard was generalised, three symbols with no intraday history had
// produced 2,436 between them, 1,566 from one ticker. This is
// noteUntradeable's "announce it once, not once per evaluation" rule, keyed on
// symbol AND reason so a CHANGED reason is still reported.

// noteBlockedOnce records that symbol's buy was refused, at most once per
// distinct reason. A different reason for the same symbol is reported again —
// it is new information — and clearBlockedOnce resets the symbol once it gets
// past the gate.
func (e *Engine) noteBlockedOnce(symbol, reason string, price float64, ind string) {
	e.noteBlockedOnceKeyed(symbol, symbol, reason, price, ind)
}

// scaleInBlockKey keys a scale-in refusal apart from an entry refusal for the
// same symbol, so one cannot swallow — or keep re-announcing — the other.
func scaleInBlockKey(symbol string) string { return "scale-in:" + symbol }

// noteBlockedOnceKeyed is noteBlockedOnce with the dedupe key separate from
// the symbol the record names.
func (e *Engine) noteBlockedOnceKeyed(key, symbol, reason string, price float64, ind string) {
	e.recordOnce(key, reason, TradeRecord{
		Time: time.Now(), Symbol: symbol, Side: broker.Buy, Price: price,
		Status: "blocked", Reason: reason, Indicators: ind,
	})
}

// recordOnce records rec unless key's last recorded reason is the same
// condition — the general form of noteBlockedOnce, for records that are not
// blocked buys (an exit dry-run held by a halt, EX3-3). Reasons compare with
// their digits masked: a cap refusal carries figures that drift with every
// price ("31.2% of equity"), and the same cap refusing the same symbol is
// one condition, not a new one each evaluation (RK3-3).
func (e *Engine) recordOnce(key, reason string, rec TradeRecord) {
	family := reasonFamily(reason)

	e.mu.Lock()

	if e.blockedOnce == nil {
		e.blockedOnce = make(map[string]string)
	}

	seen := e.blockedOnce[key] == family
	if !seen {
		e.blockedOnce[key] = family
	}

	e.mu.Unlock()

	if seen {
		return
	}

	e.record(rec)
}

// reasonFamily masks every digit in reason, so two reasons that differ only
// in their figures compare equal.
func reasonFamily(reason string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsDigit(r) {
			return '#'
		}

		return r
	}, reason)
}

// clearBlockedOnce forgets a symbol's refusal, so the next one is reported
// rather than swallowed as a repeat.
func (e *Engine) clearBlockedOnce(symbol string) {
	e.mu.RLock()

	_, had := e.blockedOnce[symbol]

	e.mu.RUnlock()

	if !had {
		return // the common path: no write lock for a symbol that is sizing fine
	}

	e.mu.Lock()
	delete(e.blockedOnce, symbol)
	e.mu.Unlock()
}
