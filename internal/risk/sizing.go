package risk

import (
	"fmt"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/exits"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
)

// Buy sizing lives here, in one implementation, for the same reason
// internal/exits exists: the live engine and the backtester each used to carry
// their own copy (Engine.riskSizedNotional and Backtester.riskSize), mirrored
// by hand and by comment. Two copies of "how big is this order" is how a
// backtest quietly stops describing the bot.

// ratioEpsilon is how far below 1 the risk ratio must fall before it counts as
// a real reduction rather than floating-point residue. A reduction smaller
// than this is worth less than a cent on any account this bot can trade.
const ratioEpsilon = 1e-9

// SizingConfig is what risk sizing needs beyond the money limits the Manager
// already holds.
type SizingConfig struct {
	// Exits supplies the stop the order's risk is measured against.
	Exits config.ExitsConfig
	// MaxRiskSizeUSD caps what sizing may produce (0 = no cap).
	MaxRiskSizeUSD float64
	// MinOrderSizeUSD is the smallest order worth placing. An order the risk
	// scaling pushes below it is REFUSED, not rounded up — see SizeBuy.
	MinOrderSizeUSD float64
}

// BuySize is the outcome of sizing one buy.
type BuySize struct {
	// Notional is the order value to place, meaningful only when OK.
	Notional float64
	// Note describes the sizing for the trade record, empty when nothing
	// interesting happened.
	Note string
	// Reason explains a refusal, empty when OK.
	Reason string
	// OK is false when no order should be placed at all.
	OK bool
}

// SizeBuy decides how much of symbol to buy at price.
//
// It starts from the real money-limit headroom — the tightest of
// max_order_value, max_per_position, the position slot, max_total_invested,
// max_daily_spend and cash_reserve, via BuyHeadroom — and scales that DOWN by
// how much wider this trade's stop is than the configured stop_loss_pct
// baseline. There is no separate fixed order size to keep in sync with the
// limits: "how much room is actually left" is the only ceiling, and risk is
// the only thing that reduces it.
//
// The scaling is always applied when a stop exists. A trade at exactly the
// baseline stop keeps the full headroom; one whose stop is wider right now (a
// genuinely riskier entry, per exits.Compute's "wider stop wins") gets
// proportionally less, approaching zero as the stop widens without bound. With
// no ATR stop configured every trade uses the same fixed stop_loss_pct, so the
// ratio is exactly 1 and every trade gets the full headroom — the variation
// only appears once atr_stop_mult can produce a stop wider than the baseline.
//
// min_order_size_usd is a REFUSAL threshold, not a floor. Raising a
// risk-reduced order back up to it would enlarge precisely the orders the
// scaling just judged too risky to hold at full size — the floor used to do
// exactly that. An order the risk arithmetic puts below the smallest size
// worth paying commission on is an order not worth placing.
func (m *Manager) SizeBuy(
	symbol string,
	price float64,
	snap portfolio.Snapshot,
	cfg SizingConfig,
	atr float64,
) BuySize {
	if reason := unsizeable(cfg, price, atr); reason != "" {
		return BuySize{Reason: reason}
	}

	order := broker.Order{Symbol: symbol, Side: broker.Buy}

	base, binding := m.BuyHeadroomBinding(order, price, snap)
	if base <= 0 {
		return BuySize{Reason: refusalNoHeadroom(binding)}
	}

	notional := base
	note := ""
	// Whether risk scaling actually reduced anything, tracked as a FACT rather
	// than rediscovered later by comparing notional against base. With a
	// percentage-only stop the ratio is stop_loss_pct/stop_loss_pct, which in
	// binary floating point is 0.9999999999999991 as often as it is exactly 1
	// — so a `notional < base` test flips between "risk did this" and "the
	// limits did this" on identical inputs. That is not just a cosmetic
	// wrong answer: the refusal reason feeds engine.noteUnsizeable's
	// announce-once dedupe, which keys on the reason, so an alternating
	// message defeats the dedupe and records a blocked trade — and fires a
	// notification — every evaluation. Observed in production doing exactly
	// that, ~once a minute per symbol.
	scaled := false

	if frac, ok := stopFraction(cfg.Exits, price, atr); ok {
		// exits.Compute always picks the WIDER of the percentage and ATR
		// stops, so frac >= StopLossPct by construction and this ratio is
		// always in (0, 1] — it can only reduce.
		//
		// The tolerance is what makes this stable. When the percentage stop
		// is the one in force, frac is stop_loss_pct recomputed through
		// (price - price*(1-pct))/price, which for most prices lands on
		// 0.05000000000000004 rather than 0.05 — so a bare `ratio < 1` sees a
		// reduction of one part in 10^15 and calls it risk scaling, for some
		// prices and not others. Anything above this threshold is
		// floating-point noise, not a trading decision.
		if ratio := cfg.Exits.StopLossPct / frac; ratio < 1-ratioEpsilon {
			notional = base * ratio
			scaled = true
			note = fmt.Sprintf("risk-sized %.2f of %.2f headroom (stop %.2f%% vs. %.2f%% baseline)",
				notional, base, frac*100, cfg.Exits.StopLossPct*100)
		}
	}

	if cfg.MaxRiskSizeUSD > 0 && notional > cfg.MaxRiskSizeUSD {
		notional = cfg.MaxRiskSizeUSD
	}

	if notional > base {
		notional = base // defensive; the ratio above cannot exceed 1
	}

	if cfg.MinOrderSizeUSD > 0 && notional < cfg.MinOrderSizeUSD {
		// Name the cause. Risk scaling and a tight money limit both end here,
		// and they need different fixes: one is "this trade is too volatile
		// to hold at size right now", the other is "this limit is what stops
		// you". Blaming risk for a binding limit sends the operator to the
		// wrong setting; not naming the limit tells them nothing at all.
		if scaled {
			return BuySize{Reason: fmt.Sprintf(
				"risk-sized to %.2f from %.2f of headroom, below min_order_size_usd %.2f — "+
					"too risky to place at a size worth trading",
				notional, base, cfg.MinOrderSizeUSD)}
		}

		return BuySize{Reason: fmt.Sprintf(
			"%s leaves %.2f, below min_order_size_usd %.2f — "+
				"no room for a position worth opening",
			limitName(binding), notional, cfg.MinOrderSizeUSD)}
	}

	return BuySize{Notional: notional, Note: note, OK: true}
}

// unsizeable is why a buy at price cannot be sized at all, or "".
//
// An ATR so large that the ATR stop cannot sit above zero leaves no stop to
// size against: the percentage stop takes over, and the most volatile entry
// would go out at FULL size behind the narrowest stop (EX3-4). Refused — a
// buy is the side that fails closed. The reason carries no numbers, so the
// announce-once dedupe holds.
func unsizeable(cfg SizingConfig, price, atr float64) string {
	switch {
	case price <= 0:
		return "no valid price"
	case cfg.Exits.Enabled && exits.ATROutOfRange(cfg.Exits, price, atr):
		return "ATR × atr_stop_mult reaches the price — no stop the position " +
			"could be sized against; entry refused until volatility settles"
	}

	return ""
}

// stopFraction returns the distance from entry to the stop as a fraction of
// price, and whether a stop could be derived at all. Entry is "now", so peak
// equals entry and the trailing stop does not apply yet.
func stopFraction(cfg config.ExitsConfig, price, atr float64) (float64, bool) {
	if !cfg.Enabled || cfg.StopLossPct <= 0 {
		return 0, false // no baseline to measure against
	}

	lv := exits.Compute(cfg, price, price, atr, 0)
	if lv.Stop <= 0 || lv.Stop >= price {
		return 0, false
	}

	return (price - lv.Stop) / price, true
}

// refusalNoHeadroom explains an outright-zero headroom, naming the limit that
// produced it so the operator knows which line of config to look at.
func refusalNoHeadroom(binding string) string {
	if binding == "" {
		return "no headroom under the money limits"
	}

	return "no headroom left under " + binding
}

// limitName renders a binding cap for an operator-facing reason, falling back
// to a generic phrase when nothing was binding (no limits configured at all).
func limitName(binding string) string {
	if binding == "" {
		return "the money limits"
	}

	return binding
}
