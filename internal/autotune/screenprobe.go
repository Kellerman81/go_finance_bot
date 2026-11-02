package autotune

import (
	"context"
	"log/slog"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/backtest"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/engine"
	"github.com/Kellerman81/go_finance_bot/internal/errlog"
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
// more informative one to rank a candidate universe by; a candidate whose
// search could not run keeps its backtest score and ranks below every
// searched one (see screener.rankTierOf). The Probe's Source field always
// says which produced the number.
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

	candles, ok := p.atBarInterval(candles)
	if !ok {
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
		if r, ok := p.runSearch(ctx, symbol, candles); ok {
			out, scored = r, true
		}
	}

	return out, scored
}

// atBarInterval returns candles on the engine's bar_interval, the bars the
// live strategy trades on. The screener fetches at its class profile's
// resolution — daily over a year for an ETF profile — and the probe replayed
// a strategy tuned on 1-minute bars over those: every period, confirm streak
// and ATR multiple then spans days, and adoption was decided on a number
// with no relation to how the bot would trade the symbol (SC3-4). Finer bars
// are aggregated up; coarser ones cannot be, and the probe declines, saying
// so once per resolution.
func (p *ScreenProbe) atBarInterval(candles []market.Candle) ([]market.Candle, bool) {
	interval := p.eng.EngineConfig().BarInterval
	if interval <= 0 {
		interval = time.Minute
	}

	gap, ok := market.FinestBarGap(candles)
	if !ok {
		return candles, true
	}

	switch {
	case gap > interval*3/2:
		errlog.RecordOncef("screener", "probe skipped: the screener's %s bars are coarser than "+
			"engine.bar_interval %s, so a backtest on them would not measure the live strategy — "+
			"set the profile's resolution to the bar interval to probe", gap.Round(time.Minute), interval)

		return nil, false
	case gap < interval:
		return market.ResampleStep(candles, interval), true
	default:
		return candles, true
	}
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

	// The guard RunSymbolOptimize applies (AT-14), which this path skipped
	// (AT2-3): an "ai" detector in a replay reads the LIVE per-symbol verdict
	// cache — today's opinion on every historical bar, a lookahead — and its
	// refresh, fed historical features, overwrites the verdict the live loop
	// reads for a symbol it may be trading. Unscored, the candidate simply
	// keeps its cheap-filter ranking.
	if len(unsearchableDetectors(cfg.Strategy)) > 0 {
		return screener.Probe{}, false
	}

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
func (p *ScreenProbe) runSearch(ctx context.Context, symbol string, candles []market.Candle) (screener.Probe, bool) {
	if !TryAcquire() {
		// The shared run slot is busy with a real optimisation pass. A probe
		// is the least important user of it: skip rather than queue, and fall
		// back to whatever the backtest leg already produced.
		p.log.Debug("screener probe: search slot busy, skipping", "symbol", symbol)

		return screener.Probe{}, false
	}
	defer Release()

	// The screening pass's context: a shutdown stops the search instead of
	// holding the process for minutes (AT3-1).
	// The screener trusts a probe only over probe.min_trades closed trades.
	// The search gated on its own 3, and its Best can be the baseline trial,
	// gated on none: a 4-trade winner replaced a 7-trade backtest leg and was
	// then ignored as too thin, so the measured candidate lost its ranking
	// altogether — and a winner with fills but no closed trade counted as
	// scored (AT3-4). The search ranks on the screener's floor, and a winner
	// below it leaves the backtest leg standing.
	minTrades := max(DefaultMinTrades, screener.ProbeMinTrades(p.eng.ScreenerConfig().Probe))

	result := RunSymbolOptimize(
		ctx, p.eng, p.rm, p.pf, symbol, candles,
		minTrades, DefaultObjective, DefaultMinReturnPct,
	)

	best := result.Best
	if best.Result.ClosedTrades < minTrades {
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
		Source:     source,
		Trades:     r.ClosedTrades, // closed trades, what probe.min_trades means (BT2-3)
		ReturnPct:  r.TotalReturnPct,
		WinRatePct: r.WinRatePct,
		// backtest.Result.ExpectancyPct is a PERCENT (PnLPct = net/basis*100);
		// the screener, its min_expectancy_pct threshold and the verdict table
		// all read Probe.ExpectancyPct as a FRACTION. Passed through raw, a
		// 0.001 threshold (documented as 0.1%) meant 0.001%, "below -1 never
		// rejects" meant -1%/trade, and the UI showed 0.8%/trade as 80%
		// (SC2-2/AT2-2). Converted here, at the one boundary.
		ExpectancyPct: r.ExpectancyPct / 100,
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
