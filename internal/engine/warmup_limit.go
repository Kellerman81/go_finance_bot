package engine

import (
	"github.com/Kellerman81/go_finance_bot/internal/errlog"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// MaxHistoryBars is how many bars per symbol the live engine keeps — all the
// history a live strategy ever evaluates on. The backtest bounds its per-bar
// window by it too, or the two read different trends (ST3-4).
const MaxHistoryBars = maxBars

// reportWarmup surfaces a strategy that can never leave warmup live: one whose
// warmup needs more bars than the engine keeps. A higher-timeframe multiple,
// a long trend_period or ml.train_bars above ~2,830 did exactly that — every
// detector of every symbol voted HOLD forever, nothing said why, and the
// backtest, which is not bounded the same way, kept trading (ST3-3, ML3-3).
// scope names the strategy ("strategy", or the overridden symbol).
func (e *Engine) reportWarmup(scope string, s strategy.Strategy) {
	if s == nil {
		return
	}

	if need := s.WarmupBars(); need > MaxHistoryBars {
		e.log.Error("strategy can never leave warmup: it needs more history than the engine keeps",
			"scope", scope, "warmup_bars", need, "history_bars", MaxHistoryBars)
		errlog.RecordOncef("strategy",
			"%s: warmup needs %d bars but the engine keeps %d — it will vote HOLD forever "+
				"(shorten trend_period, htf_multiple or ml.train_bars)", scope, need, MaxHistoryBars)
	}
}
