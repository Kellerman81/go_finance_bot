package autotune

import (
	"context"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/backtest"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// AT3-4: with both probe legs on, the search's winner replaced the backtest
// leg unconditionally. The search gated on 3 trades (its Best can be the
// baseline, gated on none) and the screener trusts a probe only over
// probe.min_trades (5): a 4-trade winner overwrote a usable 7-trade backtest,
// the screener then ignored it as too thin, and the measured candidate lost
// its ranking. A winner below the screener's floor leaves the backtest leg.
func TestAThinSearchWinnerKeepsTheBacktestLeg(t *testing.T) {
	eng, rm, pf := newTestEngine(t, nil)
	strat := config.Default().Strategy
	strat.Detectors = []string{"ema_cross", "rsi", "macd"}
	eng.SetStrategyConfig(strat)
	eng.SetExitsConfig(config.Default().Exits)

	sc := eng.ScreenerConfig()
	sc.Probe = config.ScreenerProbeConfig{Enabled: true, Backtest: true, Autotune: true}
	eng.SetScreenerConfig(sc)

	p := NewScreenProbe(eng, rm, pf, discardLog())
	c := backtest.Generate([]string{"S3"}, 3000)["S3"]

	bt, _ := p.runLiveStrategy("S3", c)
	if bt.Trades < 5 {
		t.Fatalf("fixture: the backtest leg has %d closed trades, the test needs a usable (>= 5) one", bt.Trades)
	}

	got, ok := p.Probe(context.Background(), "S3", c)
	if !ok || got.Trades < 5 {
		t.Errorf("a usable %d-trade backtest was replaced by a %d-trade %s result the screener ignores (ok=%v)",
			bt.Trades, got.Trades, got.Source, ok)
	}
}

// probeBars builds n flat bars step apart.
func probeBars(n int, step time.Duration) []market.Candle {
	out := make([]market.Candle, n)
	base := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)

	for i := range out {
		out[i] = market.Candle{Time: base.Add(time.Duration(i) * step), Open: 10, High: 10.1, Low: 9.9, Close: 10, Volume: 100}
	}

	return out
}

// SC3-4: the probe replayed the live strategy on whatever bars the class
// profile fetched — a year of DAILY bars for an ETF profile — while live
// trades it on engine.bar_interval (1m): every period and ATR multiple then
// spanned days, and adoption was decided on an unrelated number. Coarser
// bars are declined; finer ones are aggregated up to the bar interval.
func TestTheProbeRunsOnTheEnginesBarInterval(t *testing.T) {
	eng, rm, pf := newTestEngine(t, nil) // bar_interval unset = 1m
	p := NewScreenProbe(eng, rm, pf, discardLog())

	if _, ok := p.atBarInterval(probeBars(400, 24*time.Hour)); ok {
		t.Error("daily bars were accepted for a probe of a 1-minute strategy")
	}

	got, ok := p.atBarInterval(probeBars(400, 30*time.Second))
	if !ok || len(got) != 200 {
		t.Errorf("400 half-minute bars gave %d bars (ok=%v), want them aggregated to 200 one-minute bars", len(got), ok)
	}
}
