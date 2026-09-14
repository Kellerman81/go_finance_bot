package engine

import (
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
)

// A buy that cannot be sized is a recurring condition, not an event.
//
// min_order_size_usd refuses an order the risk arithmetic has scaled below the
// smallest size worth paying commission on (see risk.Manager.SizeBuy), and
// "no headroom under the money limits" is the same shape of answer. Both
// persist: the stop stays wide, or the account stays full, for as long as they
// stay that way. The evaluation loop revisits every symbol every
// eval_interval — 30s by default — so recording either one per evaluation
// would produce the same blocked trade twice a minute indefinitely, and
// `alert` turns a blocked trade into a notification.
//
// That exact failure has happened here before: a symbol whose venue could not
// be priced produced 332 identical blocked trades across one weekend. This is
// noteUntradeable's "announce it once, not once per evaluation" rule applied
// to the sizing refusals, keyed on the reason so a CHANGED reason is still
// reported.

// noteUnsizeable records that symbol's buy could not be sized, at most once
// per distinct reason. A different reason for the same symbol is reported
// again — it is new information — and clearUnsizeable resets the symbol once
// it sizes successfully.
func (e *Engine) noteUnsizeable(symbol, reason string, price float64, ind string) {
	e.mu.Lock()

	seen := e.unsizeable[symbol] == reason
	if !seen {
		e.unsizeable[symbol] = reason
	}

	e.mu.Unlock()

	if seen {
		return
	}

	e.record(TradeRecord{
		Time: time.Now(), Symbol: symbol, Side: broker.Buy, Price: price,
		Status: "blocked", Reason: reason, Indicators: ind,
	})
}

// clearUnsizeable forgets a symbol's sizing refusal, so the next one is
// reported rather than swallowed as a repeat.
func (e *Engine) clearUnsizeable(symbol string) {
	e.mu.RLock()

	_, had := e.unsizeable[symbol]

	e.mu.RUnlock()

	if !had {
		return // the common path: no write lock for a symbol that is sizing fine
	}

	e.mu.Lock()
	delete(e.unsizeable, symbol)
	e.mu.Unlock()
}
