// Package autotune runs the same bounded per-symbol backtest search two
// callers share: the Strategy tab's manual "optimize this symbol" button
// (internal/api, propose-only) and the scheduled auto-optimizer (Schedule,
// in schedule.go — optional, and unlike the button, writes its result
// straight into strategy_overrides and the live engine, unsupervised).
//
// The core logic moved here from internal/api/symbol_optimize.go so a
// background job started from cmd/bot can share it without pulling in
// gin/html-template/i18n, none of which internal/engine's world should ever
// need to import.
//
// Two baselines, not one. RunSymbolOptimize's isolated what-if backtest
// hands a symbol the FULL evaluated cash for the whole run in isolation;
// real trading never does that, since capital is shared across the whole
// watchlist and a signal the strategy fired on can simply not have been
// actable because money was tied up elsewhere. Judging a proposal only
// against that isolated backtest would let it "beat" a number the account
// never actually earned or lost. Judging it only against the real
// (capital-constrained) track record has the opposite problem: a symbol
// that's simply never had capital free would show a tiny or empty real
// history, and almost anything would look like an improvement over that —
// the exact thin-sample trap MinTrades exists to guard against. So a
// proposal must beat BOTH: the isolated backtest (proves the
// detector/combine/min_strength combination itself is sound) and the real
// track record when one exists (proves it's not just an artifact of the
// isolated backtest's unrealistic full-capital assumption). A symbol with
// no real trades yet — including every symbol right after a fresh install,
// with nothing to compare against — simply has nothing to beat on that
// side, and is judged on the isolated backtest alone.
package autotune

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/backtest"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/engine"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// init silences backtest's CLI progress bar for this process: cmd/bot (the
// only binary that reaches this package, whether via internal/api's manual
// button or Schedule) has no reader for a `\r`-redrawn stderr line, only a
// log-spam problem every time a search runs. See backtest.ProgressOutput's
// doc comment — cmd/optimize's own separate process is unaffected, since
// package-level state is never shared across OS processes.
func init() {
	backtest.ProgressOutput = io.Discard
}

// MinBars mirrors optimizeMinBars in internal/api's field sweep: below this,
// warmup eats most of the series and every candidate looks the same for
// lack of trades. Shared by the manual button's own floor check and
// Schedule's per-symbol skip.
const MinBars = 150

// DefaultMinTrades is the manual button's thin-sample floor: a handful of
// trades is still not a lot, but it is enough to see more than one outcome,
// an acceptable compromise for a result a human is about to look at before
// doing anything with it. Schedule uses its own, stricter, configurable
// floor (config.AutotuneConfig.MinTrades) instead — unsupervised,
// every-symbol, real-money writes deserve less tolerance for a thin sample
// than a click someone is standing in front of.
const DefaultMinTrades = 3

// Samples/Generations/Seed/Timeout bound the guided search's cost: a few
// hundred backtests over one symbol's cached history (typically a few
// thousand bars at most, nowhere near a full CSV backtest's scale) completes
// in a few seconds, not the minutes a real offline grid takes.
const (
	Samples     = 120
	Generations = 3
	Seed        = 1
	Timeout     = 20 * time.Second
)

// sem bounds this to one run at a time, process-wide, shared between the
// manual HTTP button and the scheduler — a full guided search is real CPU
// work, and nothing should let a refreshing tab, a second operator, or a
// scheduled pass overlap another one and stack up concurrent runs against
// the live trading loop.
var sem = make(chan struct{}, 1)

// TryAcquire claims the shared run slot, returning false (and claiming
// nothing) if a search is already in progress. Callers must call Release
// when done, and only when TryAcquire returned true.
func TryAcquire() bool {
	select {
	case sem <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release frees the shared run slot.
func Release() { <-sem }

// Result is what both the manual UI and the scheduler get back: two
// baselines for today's resolved config (WhatIf, the isolated full-capital
// backtest; Real, the actual capital-constrained track record when one
// exists) against the best config the search found, plus why.
type Result struct {
	Symbol string
	Bars   int
	// WhatIf is today's config backtested in isolation with the full
	// evaluated cash — what this symbol would have done with no competition
	// for capital from the rest of the watchlist.
	WhatIf backtest.Result
	// HasReal/Real are the symbol's realized track record over the SAME time
	// window WhatIf just backtested — engine.StatsSince(candles[0].Time),
	// not engine.Stats("all"). A symbol's all-time history can span weeks
	// under settings that have since changed, so comparing "the last ~2000
	// cached bars" against "everything that ever happened" is not the same
	// question as "did today's config actually do better than reality, over
	// the period we're both looking at". HasReal is false when there are no
	// closed trades in that window (freshly added symbol, fresh install, or
	// just a quiet window), in which case Real is the zero value and plays
	// no part in Improved.
	HasReal bool
	Real    engine.SymbolStats
	// HasTotal/Total are the same symbol's ALL-TIME realized track record —
	// context only, never fed into Improved. Comparing a same-window
	// proposal against an unbounded historical total would reintroduce the
	// exact mismatch Real/HasReal exists to avoid.
	HasTotal bool
	Total    engine.SymbolStats
	Best     backtest.Trial
	// Improved requires beating BOTH baselines, clearing an absolute
	// minimum-return floor, beating a recent real win when one exists (see
	// beatsBothBaselines and the package doc above) — AND NotOverridable
	// being empty. That last condition matters as much as the others: a
	// trial that only "wins" through a setting Proposed/Fields cannot
	// express is not a trial this package can actually apply, so it must
	// not be reported as an improvement — see CODE_REVIEW_FINDINGS.md's
	// AT-2/AT-5 findings.
	Improved bool
	// Reasoning renders Refine's own axis-by-axis log as plain sentences —
	// the same information cmd/optimize prints to stderr, reshaped for a
	// page (or a notification) instead of a terminal.
	Reasoning []string
	// Proposed is the strategy_overrides entry ready to paste into
	// config.yaml, in the same shape as a hand-written override.
	Proposed []string
	// NotOverridable names engine/exits-level settings the winning trial
	// also depends on (confirm_buy, confirm_sell, keep_interval,
	// risk_per_trade_pct, atr_stop_mult, atr_trail_mult, capital_frac,
	// exits_enabled, stop_loss_pct, trailing_stop_pct, take_profit_pct)
	// that a strategy_overrides entry cannot express at all — only
	// StrategyConfig fields are per-symbol overridable (see
	// internal/config/overrides.go). Empty when the win came entirely from
	// fields Proposed already covers. Non-empty means the numbers above
	// won't be fully reproduced by pasting Proposed (or Schedule applying
	// OverrideFields) alone — and, per Improved's own doc comment, means
	// Improved is false regardless of what beatsBothBaselines found.
	NotOverridable []string
	// Fields is the structured equivalent of Proposed — the same decision
	// of "which StrategyConfig fields differ from base", ready to hand to
	// config.PatchSymbolOverride. Only meaningful when Improved.
	Fields config.SymbolOverrideFields
	// Base is the strategy the search started from — the symbol's live
	// config when it began. Fields is a diff against it, so applying them
	// onto anything else applies a config no trial ran (AT3-2).
	Base config.StrategyConfig
	// Objective is the backtest.Scorer name that actually ranked
	// candidates this run (see backtest.ScorerNames) — echoed back so a
	// caller can display what "Best"/Improved were judged by, since an
	// unrecognized name silently falls back to "blend" (see
	// backtest.ScorerByName) rather than erroring.
	Objective string
	// MinReturnPct echoes the floor this run was judged against (see
	// config.AutotuneConfig.MinReturnPct) — alongside BelowReturnFloor,
	// lets a caller explain SPECIFICALLY why a proposal was rejected,
	// since Improved alone doesn't say which of the (up to three)
	// conditions in beatsBothBaselines failed.
	MinReturnPct float64
	// BelowReturnFloor is true when Best's own backtested return fails to
	// clear MinReturnPct. Surfaced separately from Improved because
	// raising or lowering MinReturnPct never changes WHICH candidate the
	// search settles on — the search ranks purely by Objective, unaware of
	// this floor — it only changes whether that same candidate is
	// accepted or rejected. Without this, retrying a search after
	// adjusting the floor looks like nothing happened: the displayed
	// "proposed" numbers are identical either way, because they are the
	// same candidate.
	BelowReturnFloor bool
	// HasRecentWin/RecentWin are the symbol's REAL track record since
	// internal/engine's Engine.LastAutotuneApply — i.e. since autotune
	// itself last changed this symbol's config, not since the backtest
	// window (that's Real/HasReal) or all time (Total/HasTotal). This is
	// autotune judging its OWN last decision by what actually happened
	// afterward. HasRecentWin is true only when there's a recorded apply,
	// AND real trades since it, AND they net out positive (ExpectancyPct
	// > 0) — a losing or too-thin recent record does not set this, so it
	// adds no extra hurdle (see BelowRecentWin).
	HasRecentWin bool
	RecentWin    engine.SymbolStats
	// BelowRecentWin is true when HasRecentWin is true but Best fails to
	// beat RecentWin's own win rate AND expectancy — the reason a
	// proposal can be rejected even though it clears every other gate:
	// the symbol's OWN last autotune-applied config is already winning in
	// real trading, so replacing it needs a real edge over that, not just
	// a better-looking backtest. False whenever HasRecentWin is false
	// (nothing recent to protect), regardless of Best.
	BelowRecentWin bool
}

// DefaultObjective matches config.AutotuneConfig's own documented default
// and backtest.ScoreBlend's role as "the historical default" scorer (see
// backtest.ScorerByName's doc comment) — used by the manual button, which
// has no per-run objective control of its own.
const DefaultObjective = "blend"

// DefaultMinReturnPct matches config.AutotuneConfig.MinReturnPct's own
// documented default (reject any proposal with a negative backtested
// return) — used by the manual button, which has no per-run floor of its
// own.
const DefaultMinReturnPct = 0.0

// RunSymbolOptimize builds a backtest.Config from the live settings resolved
// for symbol (its strategy_overrides entry if it has one, otherwise the
// shared strategy), runs a bounded guided search plus a refine pass against
// its cached history, and reduces the result to Result. minTrades is the
// thin-sample floor to use — DefaultMinTrades for the manual button,
// config.AutotuneConfig.MinTrades for a scheduled pass. objective selects
// which backtest.Scorer ranks candidates (see backtest.ScorerNames); an
// unrecognized or empty name falls back to "blend" — DefaultObjective for
// the manual button, config.AutotuneConfig.Objective for a scheduled pass.
//
// objective only decides which CANDIDATE wins the search: the separate gate
// against a symbol's real track record (beatsBothBaselines) always compares
// win rate and expectancy specifically, whatever objective is — see
// config.AutotuneConfig.Objective's own doc comment for why those two
// metrics are the only ones genuinely comparable between an isolated
// backtest and a capital-constrained real history. minReturnPct is an
// absolute floor the winning candidate's own backtested return must clear
// regardless of objective or either baseline — see
// config.AutotuneConfig.MinReturnPct's doc comment for why beating a weak
// or empty baseline is not by itself proof of profitability.
//
// ctx parents the search's own Timeout, so a shutdown stops it; the refine
// pass, which takes no context, is skipped once ctx is done (AT2-8).
func RunSymbolOptimize(
	parent context.Context, eng *engine.Engine, rm *risk.Manager, pf *portfolio.Portfolio,
	symbol string, candles []market.Candle, minTrades int, objective string, minReturnPct float64,
) Result {
	port, sectors, assetClasses := eng.PortfolioConfig()

	cfg := config.Config{
		Strategy:     eng.StrategyConfigFor(symbol),
		Exits:        eng.ExitsConfig(),
		Costs:        eng.CostsConfig(),
		Engine:       eng.EngineConfig(),
		Limits:       rm.Limits(),
		Portfolio:    port,
		Sectors:      sectors,
		AssetClasses: assetClasses,
		Protections:  eng.ProtectionsConfig(),
	}

	// Refuse before backtesting anything, not after: running the baseline at
	// all is what makes an "ai" detector consult the LIVE per-symbol verdict
	// cache (and schedule a background refresh from historical bars). See
	// unsearchableDetectors.
	if unsearchable := unsearchableDetectors(cfg.Strategy); len(unsearchable) > 0 {
		return Result{
			Symbol: symbol, Bars: len(candles),
			Objective: objective, MinReturnPct: minReturnPct,
			NotOverridable: unsearchable,
			Improved:       false,
			Reasoning: []string{
				"not tuned: this symbol's detector set contains " + strings.Join(unsearchable, ", ") +
					", which the optimizer's search space cannot produce — every candidate it " +
					"generated would silently drop them, so any proposal would change the " +
					"strategy rather than tune it",
			},
		}
	}

	cash := pf.Snapshot().Equity
	if cash <= 0 {
		cash = 10000
	}

	// The per-trade deployment the search centres on. There is no
	// order_size_usd any more — sizing starts at the money-limit headroom —
	// so the natural basis is the tightest per-order ceiling the limits
	// impose. 0.25 when neither is set, matching the old default.
	baseFrac := 0.25

	if perTrade := cfg.Limits.TightestPerOrderCap(cash); perTrade > 0 && cash > 0 {
		baseFrac = perTrade / cash
	}

	data := map[string][]market.Candle{symbol: candles}

	score, ok := backtest.ScorerByName(objective)
	if !ok {
		objective = DefaultObjective // report what was actually used, not what was asked for
	}

	baseline := runBaseline(cfg, data, cash, baseFrac)
	baseTrial := baselineTrial(cfg, baseFrac, cash, baseline, score)

	ctx, cancel := context.WithTimeout(parent, Timeout)
	defer cancel()

	// Pin every deployment axis to what this symbol already runs. None of the
	// four is expressible in a strategy_overrides entry, so exploring them
	// cannot produce a proposal this package can apply — it can only produce
	// one it must then throw away (see nonOverridableChanges and Result.Improved).
	// Pinning them means a win can only come from the StrategyConfig fields
	// that ARE overridable. See CODE_REVIEW_FINDINGS.md's AT-1/AT-11 findings.
	pins := backtest.Pins{
		CapitalFrac:  &baseFrac,
		StopLoss:     &cfg.Exits.StopLossPct,
		TrailingStop: &cfg.Exits.TrailingStopPct,
		TakeProfit:   &cfg.Exits.TakeProfitPct,
	}

	guided := backtest.OptimizeGuided(ctx, cfg, data, cash,
		Samples, Generations, minTrades, score, false, Seed, pins)

	cancelled := parent.Err() != nil
	best, changes := refineWinner(cfg, data, guided, baseTrial, cash, minTrades, score, cancelled)

	// The chosen Objective can legitimately rank a candidate that loses
	// money above one that doesn't — blend, for instance, weighs a
	// near-100%-win-rate streak so heavily that a thin, small LOSS can
	// outscore a genuinely profitable but merely-decent-win-rate config
	// (see ScoreBlend's formula). MinReturnPct exists to catch that, but
	// simply rejecting the top scorer and stopping there leaves a
	// profitable alternative sitting unused in the same search — guided
	// already evaluated (and scored, and sorted) every candidate it
	// tried, so if the winner doesn't clear the floor, fall back to the
	// best-scoring one among those that DO, rather than proposing
	// nothing when something better was right there.
	//
	// Deliberately not re-run through Refine: Refine's own contract only
	// ever adopts a strictly higher-scoring variant and has no awareness
	// of this floor, so refining a floor-clearing candidate could walk it
	// back below the floor again.
	if best.Result.TotalReturnPct < minReturnPct {
		if rescue, ok := bestClearingReturnFloor(guided, baseTrial, minReturnPct); ok {
			best, changes = rescue, nil
		}
	}

	// candles is chronological (SeriesResampled/the backtester both assume
	// this), so candles[0].Time is exactly the start of the window WhatIf
	// just backtested — the matching window for a fair real-vs-proposed
	// comparison.
	real, hasReal := realStatsFor(eng, symbol, candles[0].Time)
	total, hasTotal := realStatsFor(eng, symbol, time.Time{})

	recentWin, hasRecentWin := recentWinFor(eng, symbol)

	fields := overrideFields(cfg.Strategy, best)
	notOverridable := nonOverridableChanges(cfg, best, baseFrac)

	return Result{
		Symbol: symbol, Bars: len(candles),
		WhatIf: baseline, HasReal: hasReal, Real: real,
		HasTotal: hasTotal, Total: total, Best: best,
		// len(notOverridable) == 0: a win this package cannot actually apply
		// must not be reported as an improvement — see CODE_REVIEW_FINDINGS.md's
		// AT-2/AT-5 findings and Result.Improved's own doc comment.
		Improved: !cancelled && len(notOverridable) == 0 && beatsBothBaselines(
			best, baseTrial, hasReal, real, minReturnPct, hasRecentWin, recentWin),
		Reasoning:        reasoningLines(baseline, best.Result, changes, hasReal, real, hasTotal, total),
		Proposed:         formatOverride(fields),
		Base:             cfg.Strategy,
		NotOverridable:   notOverridable,
		Fields:           fields,
		Objective:        objective,
		MinReturnPct:     minReturnPct,
		BelowReturnFloor: best.Result.TotalReturnPct < minReturnPct,
		HasRecentWin:     hasRecentWin,
		RecentWin:        recentWin,
		BelowRecentWin:   hasRecentWin && !outperforms(best.Result, recentWin),
	}
}

// refineWinner takes the guided search's best trial (or the baseline, if
// nothing beat it) and runs the refine pass over it — unless the caller was
// cancelled: Refine takes no context, and a shutdown must not sit through a
// ~22-backtest sweep whose result it will discard anyway (AT2-8).
func refineWinner(
	cfg config.Config, data map[string][]market.Candle, guided []backtest.Trial, baseTrial backtest.Trial,
	cash float64, minTrades int, score backtest.Scorer, cancelled bool,
) (backtest.Trial, []backtest.Refinement) {
	best := baseTrial
	if len(guided) > 0 && guided[0].Score > best.Score {
		best = guided[0]
	}

	if cancelled || (best.Score <= baseTrial.Score && len(guided) == 0) {
		return best, nil
	}

	// strategyOnly: same reasoning as the pins in RunSymbolOptimize —
	// Refine's behaviour/adaptiveExits/riskSizing axes tune engine:/exits:
	// settings this package cannot write per-symbol.
	return backtest.Refine(cfg, data, best, cash, minTrades, score, false, io.Discard, true)
}

// recentWinFor reports the symbol's real track record since autotune's own
// last applied change to it (Engine.LastAutotuneApply), and whether that
// record amounts to a WIN — real trades exist since that apply AND they
// net out positive (ExpectancyPct > 0). No recorded apply, no real trades
// since it, or a net loss/breakeven since it all report false: a losing or
// too-thin recent record adds no extra hurdle of its own (see
// beatsBothBaselines) — it's a config with nothing worth protecting, not
// one to protect from being replaced.
func recentWinFor(eng *engine.Engine, symbol string) (engine.SymbolStats, bool) {
	lastApply, ok := eng.LastAutotuneApply(symbol)
	if !ok {
		return engine.SymbolStats{}, false
	}

	since, hasSince := realStatsFor(eng, symbol, lastApply)
	if !hasSince || since.ExpectancyPct <= 0 {
		return engine.SymbolStats{}, false
	}

	return since, true
}

// bestClearingReturnFloor returns the highest-scoring trial in guided (see
// backtest.OptimizeGuided — already sorted best-first by Score) that beats
// baseTrial's score AND clears minReturnPct, for RunSymbolOptimize's
// return-floor fallback. guided being sorted lets this stop at the first
// trial whose score no longer beats baseTrial: nothing after that point
// could pass either check.
func bestClearingReturnFloor(
	guided []backtest.Trial, baseTrial backtest.Trial, minReturnPct float64,
) (backtest.Trial, bool) {
	for _, t := range guided {
		if t.Score <= baseTrial.Score {
			break
		}

		if t.Result.TotalReturnPct >= minReturnPct {
			return t, true
		}
	}

	return backtest.Trial{}, false
}

// beatsBothBaselines is the package doc's "must beat both" rule: the winning
// trial must score higher than the isolated what-if backtest (baseTrial —
// proves the config itself is sound), its own backtested return must clear
// minReturnPct outright (see config.AutotuneConfig.MinReturnPct — beating a
// weak or empty baseline's SCORE is not by itself proof of profitability:
// a symbol with no what-if trades and no real trades in the window has a
// baseline score of ~0, which a lossmaking candidate can still "beat" on
// blend's win-rate/drawdown terms alone), AND — only when the symbol has a
// real track record to judge against — its win rate and expectancy must
// both exceed the real, capital-constrained numbers too (proves the win
// isn't just an artifact of the what-if backtest's unrealistic full-capital
// assumption). A symbol with no real trades yet has nothing on that side to
// beat, so hasReal=false makes this the what-if-plus-floor check alone —
// exactly the case a fresh install or a freshly-added symbol needs.
//
// hasRecentWin/recentWin add a THIRD, independent bar: when autotune's own
// last applied change to this symbol has been winning in real trading
// since (see recentWinFor), a replacement must ALSO beat THAT specific
// record on win rate and expectancy — autotune should not readily replace
// a config that is demonstrably working. When there's no recorded apply,
// no real trades since it, or that record is a loss, hasRecentWin is
// false and this adds no hurdle at all: a symbol with nothing recent
// worth protecting (or one that's LOSING since its last change) is easy
// to move away from, on purpose — see RunSymbolOptimize's own doc comment.
func beatsBothBaselines(
	best, baseTrial backtest.Trial, hasReal bool, real engine.SymbolStats, minReturnPct float64,
	hasRecentWin bool, recentWin engine.SymbolStats,
) bool {
	if best.Score <= baseTrial.Score {
		return false
	}

	if best.Result.TotalReturnPct < minReturnPct {
		return false
	}

	if hasRecentWin && !outperforms(best.Result, recentWin) {
		return false
	}

	if !hasReal {
		return true
	}

	return outperforms(best.Result, real)
}

// outperforms is the real-side comparison: a candidate must beat the record
// on both expectancy and win rate, the win rate read SMOOTHED by the size of
// the record — (wins+1)/(trades+2), Laplace's rule of succession.
//
// Raw, one winning close since the last apply was a 100% win rate: the AT2-6
// fix let a saturated record be matched, but matching still took a candidate
// that won every one of its 10+ backtested trades, which almost none does —
// a 20-trade, 90%-win candidate at five times the expectancy was refused,
// and the symbol stayed un-tunable until a loss arrived (AT3-5). Smoothed,
// one win reads 67%, ten in a row 92%: a thin record is a low hurdle and a
// long one a high hurdle, and neither is impassable.
//
// No minimum trade count is imposed on the real side, deliberately: a thin
// record makes the hurdle low, never impassable, and the what-if score,
// min_return_pct and min_trades gates still apply in full. Discarding a thin
// record instead would remove the one check that a win is not an artifact of
// the backtest's full-capital assumption for every symbol with few trades.
func outperforms(res backtest.Result, rec engine.SymbolStats) bool {
	if res.ExpectancyPct <= rec.ExpectancyPct {
		return false
	}

	return res.WinRatePct > smoothedWinRatePct(rec)
}

// smoothedWinRatePct is rec's win rate by the rule of succession, in percent.
func smoothedWinRatePct(rec engine.SymbolStats) float64 {
	if rec.Trades <= 0 {
		return rec.WinRatePct
	}

	wins := float64(rec.Wins)
	if rec.Wins == 0 && rec.WinRatePct > 0 {
		wins = rec.WinRatePct / 100 * float64(rec.Trades)
	}

	return (wins + 1) / float64(rec.Trades+2) * 100
}

// realStatsFor returns symbol's realized track record from since to now (a
// zero since means "all of history"), and whether it has any closed trades
// in that window at all. A symbol with none — freshly added to the
// watchlist, right after a fresh install, or just a quiet window — has
// nothing real to compare a proposal against there, so the caller must fall
// back to judging on the isolated backtest alone rather than treating a
// missing real baseline as "beaten" or "not beaten".
func realStatsFor(eng *engine.Engine, symbol string, since time.Time) (engine.SymbolStats, bool) {
	stats, err := eng.StatsSince(since)
	if err != nil {
		return engine.SymbolStats{}, false
	}

	for _, sym := range stats.BySymbol {
		if sym.Symbol == symbol {
			return sym, sym.Trades > 0
		}
	}

	return engine.SymbolStats{}, false
}

// runBaseline backtests today's resolved config exactly as it stands, so the
// search has something concrete to beat rather than an abstract score.
func runBaseline(cfg config.Config, data map[string][]market.Candle, cash, frac float64) backtest.Result {
	orderSize := cash * frac

	bt := &backtest.Backtester{
		Strategy:        strategy.New(cfg.Strategy),
		Limits:          relaxedSymbolLimits(orderSize, cash),
		Exits:           cfg.Exits,
		Costs:           cfg.Costs,
		Cash:            cash,
		OrderSize:       orderSize,
		AllowPyramiding: cfg.Engine.AllowPyramiding,
		ScaleIn:         cfg.Engine.ScaleIn,
		ConfirmBuy:      cfg.Engine.ConfirmBuy,
		ConfirmSell:     cfg.Engine.ConfirmSell,
		KeepInterval:    cfg.Engine.KeepInterval,
		MaxRiskSizeUSD:  cfg.Engine.MaxRiskSizeUSD,
		MinOrderSizeUSD: cfg.Engine.MinOrderSizeUSD,
		// MinPositions stays 0 on purpose: this is one symbol on a synthetic
		// budget, and a min_positions slot of THAT budget is not the live
		// slot. The candidates run the same way, so the comparison is fair.
		Portfolio:    cfg.Portfolio,
		Sectors:      cfg.Sectors,
		AssetClasses: cfg.AssetClasses,
		Protections:  cfg.Protections,
	}

	return bt.Run(data)
}

// relaxedSymbolLimits mirrors backtest's own (unexported) relaxedLimits: wide
// enough that money limits don't cap the deployment being explored, same as
// every other search entry point in this codebase.
func relaxedSymbolLimits(orderSize, cash float64) config.Limits {
	return config.Limits{
		MaxTotalInvested: cash,
		MaxPerPosition:   orderSize * 1.05,
		MaxOrderValue:    orderSize * 1.05,
		MaxDailySpend:    cash * 100,
		CashReserve:      0,
		MaxOpenPositions: 10000,
	}
}

// baselineTrial packages the baseline run as a Trial so it can compete with
// the search's own candidates on equal footing (Refine only ever adopts a
// variant that scores higher than what it started from).
func baselineTrial(
	cfg config.Config, frac, cash float64, res backtest.Result, score backtest.Scorer,
) backtest.Trial {
	return backtest.Trial{
		Detectors: cfg.Strategy.Detectors, Combine: cfg.Strategy.Combine,
		MinStrength: cfg.Strategy.MinStrength,
		CapitalFrac: frac, OrderSize: cash * frac,
		StopLoss: cfg.Exits.StopLossPct, TrailingStop: cfg.Exits.TrailingStopPct,
		TakeProfit: cfg.Exits.TakeProfitPct,
		Params:     cfg.Strategy, Result: res, Score: score(res),
		Exits:        cfg.Exits,
		ConfirmBuy:   cfg.Engine.ConfirmBuy,
		ConfirmSell:  cfg.Engine.ConfirmSell,
		KeepInterval: cfg.Engine.KeepInterval,
	}
}

// reasoningLines renders why the proposed config is proposed: both current
// baselines against the winner's headline numbers — real windowed to match
// what-if's own span, plus the all-time total for context only — then
// Refine's own axis-by-axis log (empty when nothing it tried improved on the
// search's own winner).
func reasoningLines(
	whatIf, best backtest.Result, changes []backtest.Refinement,
	hasReal bool, real engine.SymbolStats,
	hasTotal bool, total engine.SymbolStats,
) []string {
	var lines []string

	if hasReal {
		lines = append(lines, fmt.Sprintf(
			"current, real (%d actual trades over the same window, capital-constrained): "+
				"%.1f%% win rate, %.2f%% expectancy per trade",
			real.Trades, real.WinRatePct, real.ExpectancyPct,
		))
	} else {
		lines = append(lines,
			"current, real: no closed trades for this symbol in this window — judged on the what-if baseline alone",
		)
	}

	lines = append(lines,
		fmt.Sprintf(
			"current, what if fully capitalized: %.2f%% return, %.1f%% win rate, %d trades, %.2f%% max drawdown",
			whatIf.TotalReturnPct, whatIf.WinRatePct, whatIf.NumTrades, whatIf.MaxDrawdownPct,
		),
		fmt.Sprintf(
			"proposed: %.2f%% return, %.1f%% win rate, %d trades, %.2f%% max drawdown",
			best.TotalReturnPct, best.WinRatePct, best.NumTrades, best.MaxDrawdownPct,
		),
	)

	if hasTotal {
		lines = append(lines, fmt.Sprintf(
			"for context only, not compared — all-time real track record (%d trades): "+
				"%.1f%% win rate, %.2f%% expectancy per trade",
			total.Trades, total.WinRatePct, total.ExpectancyPct,
		))
	}

	for _, ch := range changes {
		lines = append(lines, fmt.Sprintf("%s: %s -> %s (score %+.2f)",
			ch.Axis, ch.From, ch.To, ch.Improved))
	}

	return lines
}

// quoteJoin renders a string slice as a quoted, comma-separated YAML list —
// mirrors cmd/optimize/main.go's identically-named helper.
func quoteJoin(ss []string) string {
	q := make([]string, len(ss))
	for i, sv := range ss {
		q[i] = fmt.Sprintf("%q", sv)
	}

	return strings.Join(q, ", ")
}

// overrideFields reduces the winning trial to exactly the StrategyConfig
// fields that differ from base — the fields a strategy_overrides entry
// should actually name. Shared by formatOverride (the UI's/notification's
// display strings) and Schedule's structured write via
// config.PatchSymbolOverride, so "which fields differ" is one decision, not
// two that could drift.
//
// detectors/combine/min_strength are always set (the shape every override
// this session applied by hand used); every other field — sell-side,
// trend-gate, and every indicator parameter the search can vary — is set
// ONLY when it actually differs from base.
//
// TrendGate matters here specifically because Refine has a dedicated axis
// for it (see internal/backtest/refine.go's trendGate method) that can flip
// it on the winning trial — omitting it would silently propose a config
// that doesn't actually reproduce the backtested numbers shown alongside
// it, since a strategy_overrides entry that doesn't name trend_gate
// INHERITS whatever the shared config has it set to, not whatever the
// search found best.
//
// The indicator-period fields (rsi_period, macd_fast, adx_period, ...) ARE
// now diffed, having previously been deliberately excluded on the reasoning
// that the guided search randomizes every one of them on every candidate
// regardless of which detectors are active, making most of them differing
// from base "sampling noise". That reasoning missed that noise or not, they
// are still inputs to the very Result a proposal is judged and displayed
// by — dropping them meant the applied config did not reproduce the
// backtested one. See CODE_REVIEW_FINDINGS.md's AT-2 finding. The
// trade-off accepted here: a proposal now often also names periods for
// detectors that aren't even in the winning set (inert once applied, since
// StrategyConfig/PerSymbolStrategy simply ignores a period whose detector
// isn't listed) — harmless noise in the file, not a correctness problem.
func overrideFields(base config.StrategyConfig, t backtest.Trial) config.SymbolOverrideFields {
	var f config.SymbolOverrideFields

	f.Detectors = t.Detectors
	f.Combine = t.Combine
	minStrength := t.MinStrength
	f.MinStrength = &minStrength

	p := t.Params

	if !slices.Equal(p.DetectorsSell, base.DetectorsSell) {
		ds := append([]string(nil), p.DetectorsSell...)
		f.DetectorsSell = &ds
	}

	if p.CombineSell != base.CombineSell {
		cs := p.CombineSell
		f.CombineSell = &cs
	}

	if p.MinStrengthSell != base.MinStrengthSell {
		ms := p.MinStrengthSell
		f.MinStrengthSell = &ms
	}

	if p.TrendGate != base.TrendGate {
		tg := p.TrendGate
		f.TrendGate = &tg
	}

	f.FastMA = diffInt(base.FastMA, p.FastMA)
	f.SlowMA = diffInt(base.SlowMA, p.SlowMA)
	f.RSIPeriod = diffInt(base.RSIPeriod, p.RSIPeriod)
	f.RSIOverbought = diffFloat(base.RSIOverbought, p.RSIOverbought)
	f.RSIOversold = diffFloat(base.RSIOversold, p.RSIOversold)
	f.MACDFast = diffInt(base.MACDFast, p.MACDFast)
	f.MACDSlow = diffInt(base.MACDSlow, p.MACDSlow)
	f.MACDSignal = diffInt(base.MACDSignal, p.MACDSignal)
	f.BBPeriod = diffInt(base.BBPeriod, p.BBPeriod)
	f.BBStdDev = diffFloat(base.BBStdDev, p.BBStdDev)
	f.VWAPPeriod = diffInt(base.VWAPPeriod, p.VWAPPeriod)
	f.ATRPeriod = diffInt(base.ATRPeriod, p.ATRPeriod)
	f.ATRMult = diffFloat(base.ATRMult, p.ATRMult)
	f.RVOLPeriod = diffInt(base.RVOLPeriod, p.RVOLPeriod)
	f.RVOLThreshold = diffFloat(base.RVOLThreshold, p.RVOLThreshold)
	f.VPWindow = diffInt(base.VPWindow, p.VPWindow)
	f.VPBuckets = diffInt(base.VPBuckets, p.VPBuckets)
	f.ADXPeriod = diffInt(base.ADXPeriod, p.ADXPeriod)
	f.ADXThreshold = diffFloat(base.ADXThreshold, p.ADXThreshold)
	f.StochKPeriod = diffInt(base.StochKPeriod, p.StochKPeriod)
	f.StochDPeriod = diffInt(base.StochDPeriod, p.StochDPeriod)
	f.StochOverbought = diffFloat(base.StochOverbought, p.StochOverbought)
	f.StochOversold = diffFloat(base.StochOversold, p.StochOversold)
	f.SupertrendPeriod = diffInt(base.SupertrendPeriod, p.SupertrendPeriod)
	f.SupertrendMult = diffFloat(base.SupertrendMult, p.SupertrendMult)
	f.DonchianPeriod = diffInt(base.DonchianPeriod, p.DonchianPeriod)

	return f
}

// diffInt returns a pointer to val when it differs from base, nil otherwise
// — the shared "did the search change this axis" test overrideFields uses
// for every int-valued indicator parameter.
func diffInt(base, val int) *int {
	if val == base {
		return nil
	}

	v := val

	return &v
}

// diffFloat is diffInt for float64-valued indicator parameters.
func diffFloat(base, val float64) *float64 {
	if val == base {
		return nil
	}

	v := val

	return &v
}

// formatOverride renders fields as strategy_overrides YAML lines for
// display — detectors/combine/min_strength always shown, the rest only
// when set.
func formatOverride(f config.SymbolOverrideFields) []string {
	lines := []string{
		fmt.Sprintf("detectors: [%s]", quoteJoin(f.Detectors)),
		fmt.Sprintf("combine: %q", f.Combine),
	}

	if f.MinStrength != nil {
		lines = append(lines, fmt.Sprintf("min_strength: %.2f", *f.MinStrength))
	}

	if f.DetectorsSell != nil {
		lines = append(lines, fmt.Sprintf("detectors_sell: [%s]", quoteJoin(*f.DetectorsSell)))
	}

	if f.CombineSell != nil {
		lines = append(lines, fmt.Sprintf("combine_sell: %q", *f.CombineSell))
	}

	if f.MinStrengthSell != nil {
		lines = append(lines, fmt.Sprintf("min_strength_sell: %.2f", *f.MinStrengthSell))
	}

	if f.TrendGate != nil {
		lines = append(lines, fmt.Sprintf("trend_gate: %v", *f.TrendGate))
	}

	lines = appendIntLine(lines, "fast_ma", f.FastMA)
	lines = appendIntLine(lines, "slow_ma", f.SlowMA)
	lines = appendIntLine(lines, "rsi_period", f.RSIPeriod)
	lines = appendFloatLine(lines, "rsi_overbought", f.RSIOverbought)
	lines = appendFloatLine(lines, "rsi_oversold", f.RSIOversold)
	lines = appendIntLine(lines, "macd_fast", f.MACDFast)
	lines = appendIntLine(lines, "macd_slow", f.MACDSlow)
	lines = appendIntLine(lines, "macd_signal", f.MACDSignal)
	lines = appendIntLine(lines, "bb_period", f.BBPeriod)
	lines = appendFloatLine(lines, "bb_stddev", f.BBStdDev)
	lines = appendIntLine(lines, "vwap_period", f.VWAPPeriod)
	lines = appendIntLine(lines, "atr_period", f.ATRPeriod)
	lines = appendFloatLine(lines, "atr_mult", f.ATRMult)
	lines = appendIntLine(lines, "rvol_period", f.RVOLPeriod)
	lines = appendFloatLine(lines, "rvol_threshold", f.RVOLThreshold)
	lines = appendIntLine(lines, "vp_window", f.VPWindow)
	lines = appendIntLine(lines, "vp_buckets", f.VPBuckets)
	lines = appendIntLine(lines, "adx_period", f.ADXPeriod)
	lines = appendFloatLine(lines, "adx_threshold", f.ADXThreshold)
	lines = appendIntLine(lines, "stoch_k_period", f.StochKPeriod)
	lines = appendIntLine(lines, "stoch_d_period", f.StochDPeriod)
	lines = appendFloatLine(lines, "stoch_overbought", f.StochOverbought)
	lines = appendFloatLine(lines, "stoch_oversold", f.StochOversold)
	lines = appendIntLine(lines, "supertrend_period", f.SupertrendPeriod)
	lines = appendFloatLine(lines, "supertrend_mult", f.SupertrendMult)
	lines = appendIntLine(lines, "donchian_period", f.DonchianPeriod)

	return lines
}

// appendIntLine appends "key: v" when v is set, for one int-valued field in
// formatOverride's rendering.
func appendIntLine(lines []string, key string, v *int) []string {
	if v == nil {
		return lines
	}

	return append(lines, fmt.Sprintf("%s: %d", key, *v))
}

// appendFloatLine is appendIntLine for a float64-valued field.
func appendFloatLine(lines []string, key string, v *float64) []string {
	if v == nil {
		return lines
	}

	return append(lines, fmt.Sprintf("%s: %.2f", key, *v))
}

// proposedOverride is overrideFields followed by formatOverride — the
// winning trial rendered straight to strategy_overrides YAML lines, for
// callers (and tests) that don't need the structured form in between.
func proposedOverride(base config.StrategyConfig, t backtest.Trial) []string {
	return formatOverride(overrideFields(base, t))
}

// unsearchableDetectors names detectors in the symbol's CURRENT resolved
// config that the optimizer's search space can never produce, as
// "detectors:<name>" entries for Result.NotOverridable.
//
// backtest.DefaultGrid's ten detector sets between them cover every registered
// detector EXCEPT "ai" (the LLM advisor) and "ml" (the learned classifier). So
// for a symbol configured to use either, three things go wrong at once, and
// none of them announce themselves:
//
//   - Every candidate the search generates omits it, while the baseline keeps
//     it — the comparison is between different detector families, and the whole
//     ai/ml contribution gets attributed to whichever grid axis happened to move.
//   - overrideFields always writes f.Detectors, so applying any proposal
//     silently STRIPS the detector from that symbol's config.
//   - Running the baseline at all calls ai.Advisor.Verdict, whose cache is keyed
//     by symbol alone. Inside cmd/bot that cache holds a verdict computed from
//     LIVE data, so every historical bar of the backtest reads the same
//     present-day opinion (lookahead), and in the default non-blocking mode the
//     call also schedules a background refresh from those historical features —
//     overwriting the verdict the live trading loop then reads.
//
// Derived from DefaultGrid rather than hardcoded, so adding ai/ml to the grid
// one day automatically makes them searchable here. Names are canonicalised
// through strategy.CanonicalDetector so a config written with an alias
// ("llm", "ema", "bb") is compared like for like; a name no detector is
// registered under at all is reported too, since a typo'd detector is silently
// dropped from the strategy that actually runs (see buildDetectorList) and is
// no more tunable than an unsearchable one.
func unsearchableDetectors(sc config.StrategyConfig) []string {
	searchable := make(map[string]bool)

	for _, set := range backtest.DefaultGrid().DetectorSets {
		for _, name := range set {
			if c, ok := strategy.CanonicalDetector(name); ok {
				searchable[c] = true
			}
		}
	}

	var (
		out  []string
		seen = make(map[string]bool)
	)

	for _, name := range slices.Concat(sc.Detectors, sc.DetectorsSell) {
		c, known := strategy.CanonicalDetector(name)
		if !known {
			c = strings.ToLower(strings.TrimSpace(name))
		}

		if c == "" || searchable[c] || seen[c] {
			continue
		}

		seen[c] = true

		out = append(out, "detectors:"+c)
	}

	slices.Sort(out) // deterministic ordering for the UI and the notification

	return out
}

// nonOverridableChanges names engine/exits-level settings the winning trial
// also depends on that a strategy_overrides entry cannot express at all —
// only StrategyConfig fields are per-symbol overridable (see
// internal/config/overrides.go's PerSymbolStrategy). Refine's behaviour(),
// adaptiveExits() and riskSizing() axes can all improve a trial's score via
// exactly these fields, and unlike the StrategyConfig ones overrideFields
// carries, there is nowhere to put them in a per-symbol override — changing
// them means changing the shared engine:/exits: config for every symbol,
// not just this one.
//
// capital_frac, exits_enabled, stop_loss_pct, trailing_stop_pct and
// take_profit_pct close CODE_REVIEW_FINDINGS.md's AT-3/AT-6 findings:
// runTrial (internal/backtest/optimize.go) draws CapitalFrac and the three
// exit levels from the grid, and forces Exits.Enabled true, on EVERY
// candidate — not just refined ones — so these were previously invisible
// here despite differing on essentially every trial. baseFrac is the
// caller's own baseline CapitalFrac (see RunSymbolOptimize); after the AT-1
// fix pins the search to it, a mismatch here would mean something upstream
// changed CapitalFrac unexpectedly, so it is still checked defensively.
func nonOverridableChanges(cfg config.Config, t backtest.Trial, baseFrac float64) []string {
	var names []string

	if t.ConfirmBuy != cfg.Engine.ConfirmBuy {
		names = append(names, "confirm_buy")
	}

	if t.ConfirmSell != cfg.Engine.ConfirmSell {
		names = append(names, "confirm_sell")
	}

	if t.KeepInterval != cfg.Engine.KeepInterval {
		names = append(names, "keep_interval")
	}

	if t.Exits.ATRStopMult != cfg.Exits.ATRStopMult {
		names = append(names, "atr_stop_mult")
	}

	if t.Exits.ATRTrailMult != cfg.Exits.ATRTrailMult {
		names = append(names, "atr_trail_mult")
	}

	if t.CapitalFrac != baseFrac {
		names = append(names, "capital_frac")
	}

	if !cfg.Exits.Enabled {
		names = append(names, "exits_enabled")
	}

	if t.Exits.StopLossPct != cfg.Exits.StopLossPct {
		names = append(names, "stop_loss_pct")
	}

	if t.Exits.TrailingStopPct != cfg.Exits.TrailingStopPct {
		names = append(names, "trailing_stop_pct")
	}

	if t.Exits.TakeProfitPct != cfg.Exits.TakeProfitPct {
		names = append(names, "take_profit_pct")
	}

	return names
}
