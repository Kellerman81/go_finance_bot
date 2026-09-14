package autotune

import (
	"context"
	"log/slog"

	"github.com/Kellerman81/go_finance_bot/internal/backtest"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/engine"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
	"github.com/Kellerman81/go_finance_bot/internal/screener"
)

// The screener's probe stage lives here rather than in internal/screener
// because it needs the backtester AND the optimizer, and this package already
// imports the engine that imports the screener. cmd/bot wires the two together
// with Engine.SetScreenProber.
//
// It answers the question no indicator filter can: the filters establish that
// an instrument is tradeable — liquid enough, volatile enough, cheap enough to
// clear its own spread — and this one establishes whether trading it would
// actually have made money.

// ScreenProbe scores a screening candidate by backtesting it.
//
// Two modes, answering deliberately different questions:
//
//   - Backtest runs the LIVE strategy exactly as configured. "Would what I am
//     running now have worked here?" — the honest question for adopting a
//     symbol into a book that trades one configuration.
//   - Autotune runs the bounded per-symbol search. "Could this symbol work if
//     I tuned for it?" — which finds instruments today's settings happen to
//     suit badly, and whose number is only achievable if you then tune for it.
//
// With both set, the autotune score wins where the search found something
// better, because the two are not comparable and the achievable figure is the
// more informative one to rank a candidate universe by. The Probe's Source
// field always says which produced the number.
type ScreenProbe struct {
	eng *engine.Engine
	rm  *risk.Manager
	pf  *portfolio.Portfolio
	log *slog.Logger
}

// NewScreenProbe builds the probe stage. It reads its own settings live from
// the engine on every call, so turning probing on or off — or switching between
// backtest and autotune — applies without a restart.
func NewScreenProbe(
	eng *engine.Engine, rm *risk.Manager, pf *portfolio.Portfolio, log *slog.Logger,
) *ScreenProbe {
	return &ScreenProbe{eng: eng, rm: rm, pf: pf, log: log}
}

// Probe implements screener.Prober.
//
// ok is false only when the candidate could not be scored at all; a genuinely
// bad result is reported as a probe with a bad expectancy, which is what lets
// the screener refuse it for the right reason.
func (p *ScreenProbe) Probe(
	ctx context.Context,
	symbol string,
	candles []market.Candle,
) (screener.Probe, bool) {
	if p == nil || len(candles) < MinBars {
		return screener.Probe{}, false
	}

	cfg := p.eng.ScreenerConfig().Probe
	if !cfg.Enabled {
		return screener.Probe{}, false
	}

	if bars := cfg.Bars; bars > 0 && len(candles) > bars {
		candles = candles[len(candles)-bars:]
	}

	var (
		out    screener.Probe
		scored bool
	)

	if cfg.Backtest {
		if r, ok := p.runLiveStrategy(symbol, candles); ok {
			out, scored = r, true
		}
	}

	// The search runs second and wins: it measures the same instrument under
	// the best configuration found rather than today's, so where it differs it
	// is strictly the more informative number to rank a universe by.
	if cfg.Autotune && ctx.Err() == nil {
		if r, ok := p.runSearch(symbol, candles); ok {
			out, scored = r, true
		}
	}

	return out, scored
}

// runLiveStrategy backtests the engine's current configuration over the
// candidate's own bars.
//
// Sizing is deliberately the relaxed, single-symbol kind the manual optimizer
// uses: a candidate must be judged on its own merits, not on whether today's
// cash happened to be committed elsewhere. A symbol that scores badly only
// because the account is full is not a bad symbol.
func (p *ScreenProbe) runLiveStrategy(symbol string, candles []market.Candle) (screener.Probe, bool) {
	cfg := p.probeConfig(symbol)

	cash, frac := probeCapital(p.pf)

	res := runBaseline(cfg, map[string][]market.Candle{symbol: candles}, cash, frac)

	return probeFrom("backtest", res, res.TotalReturnPct), res.NumTrades > 0
}

// runSearch runs the same bounded per-symbol optimisation a manual "optimize
// this symbol" click does, and scores the candidate on the winner.
//
// It reports the BEST trial found, not RunSymbolOptimize's Improved verdict:
// Improved answers "should this replace the symbol's live config", which is a
// question about a symbol already being traded. For a candidate that is not in
// the book at all, the question is simply "what is achievable here".
func (p *ScreenProbe) runSearch(symbol string, candles []market.Candle) (screener.Probe, bool) {
	if !TryAcquire() {
		// The shared run slot is busy with a real optimisation pass. A probe
		// is the least important user of it: skip rather than queue, and fall
		// back to whatever the backtest leg already produced.
		p.log.Debug("screener probe: search slot busy, skipping", "symbol", symbol)

		return screener.Probe{}, false
	}
	defer Release()

	result := RunSymbolOptimize(
		p.eng, p.rm, p.pf, symbol, candles,
		DefaultMinTrades, DefaultObjective, DefaultMinReturnPct,
	)

	best := result.Best
	if best.Result.NumTrades == 0 {
		return screener.Probe{}, false
	}

	return probeFrom("autotune", best.Result, best.Score), true
}

// probeConfig assembles the live configuration for a backtest of symbol,
// resolving any per-symbol strategy override exactly as the engine would.
func (p *ScreenProbe) probeConfig(symbol string) config.Config {
	return config.Config{
		Strategy:    p.eng.StrategyConfigFor(symbol),
		Exits:       p.eng.ExitsConfig(),
		Costs:       p.eng.CostsConfig(),
		Engine:      p.eng.EngineConfig(),
		Limits:      p.rm.Limits(),
		Protections: p.eng.ProtectionsConfig(),
	}
}

// probeFrom converts a backtest result into the screener's own probe shape.
func probeFrom(source string, r backtest.Result, score float64) screener.Probe {
	return screener.Probe{
		Source:        source,
		Trades:        r.NumTrades,
		ReturnPct:     r.TotalReturnPct,
		WinRatePct:    r.WinRatePct,
		ExpectancyPct: r.ExpectancyPct,
		Score:         score,
	}
}

// probeCapital picks the cash and per-order fraction a probe backtests with.
// It mirrors the manual optimizer so a screening score and an "optimize this
// symbol" click on the same candidate describe the same thing.
func probeCapital(pf *portfolio.Portfolio) (cash, frac float64) {
	cash = pf.Snapshot().Equity
	if cash <= 0 {
		cash = defaultProbeCash
	}

	return cash, defaultProbeFrac
}

// defaultProbeCash/defaultProbeFrac size a probe when the account reports no
// equity yet (a fresh install). The absolute numbers only need to be
// self-consistent: every candidate is scored with the same capital, so the
// ranking between them is unaffected.
const (
	defaultProbeCash = 10000.0
	defaultProbeFrac = 0.25
)
