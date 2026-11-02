package screener

import (
	"context"
	"fmt"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// The probe stage.
//
// Every filter before this one answers "is this instrument tradeable?" — enough
// history, enough liquidity, moves enough to clear its costs, trends cleanly
// enough to have a direction. None of them answers the question the operator
// actually has: would MY strategy have made money on it?
//
// A probe answers that by running the real backtester over the candidate's own
// bars. It is the last stage and a bounded one: a search per candidate over a
// few hundred symbols is minutes of CPU, so only the best few per class — the
// ones that already survived every cheap filter and ranked at the top — are
// ever probed.
//
// A probe can only ever DEMOTE or REJECT a candidate, never promote one that
// failed the filters. An instrument that cannot clear its own spread does not
// become tradeable because a backtest of it happened to look good.

// defaultProbeCandidates is how many survivors per class are probed when none
// is configured.
const defaultProbeCandidates = 10

// defaultProbeMinTrades is the fewest trades a probe result is trusted on. A
// score built on three lucky fills says nothing about the instrument.
const defaultProbeMinTrades = 5

// ProbeMinTrades resolves probe.min_trades as the probe stage applies it, for
// the prober (autotune.ScreenProbe), whose search must gate its winner on the
// same floor (AT3-4).
func ProbeMinTrades(c config.ScreenerProbeConfig) int {
	if c.MinTrades > 0 {
		return c.MinTrades
	}

	return defaultProbeMinTrades
}

// probeAll scores the top candidates of each class and folds the outcome back
// into both the per-class survivor lists (which decide what gets adopted) and
// the verdict list (which the UI renders).
func (s *Screener) probeAll(ctx context.Context, passed map[string][]Verdict, verdicts []Verdict) {
	if s.prober == nil || !s.cfg.Probe.Enabled {
		return
	}

	limit := s.cfg.Probe.MaxCandidates
	if limit <= 0 {
		limit = defaultProbeCandidates
	}

	for class, survivors := range passed {
		// Probe the best first: a cutoff is only defensible if it falls on the
		// candidates least likely to be adopted anyway.
		sortByRank(survivors)

		kept := make([]Verdict, 0, len(survivors))

		for i, v := range survivors {
			if i >= limit {
				// Beyond the cutoff. Keep the candidate on its cheap-filter
				// ranking — not probed is not the same as probed badly.
				kept = append(kept, v)

				continue
			}

			judged, ok := s.probeOne(ctx, v)

			// Publish the JUDGED verdict, not the one we started with, so the
			// UI shows the probe and the reason it produced. A rejected
			// candidate keeps a verdict too: it was measured and found
			// wanting, which is worth seeing.
			publishVerdict(verdicts, judged)

			if ok {
				kept = append(kept, judged)
			}
		}

		passed[class] = kept
	}
}

// probeOne scores a single candidate and decides whether it survives. The
// returned verdict carries the probe and, when the result is trustworthy, a
// rank derived from it; ok is false when the probe measured a loser.
func (s *Screener) probeOne(ctx context.Context, v Verdict) (Verdict, bool) {
	p, ok := s.prober.Probe(ctx, v.Symbol, v.candles)
	if !ok {
		// Could not be scored at all — too little history, or a strategy that
		// never fired on it. Keep the cheap-filter verdict rather than
		// inventing a judgement, same as every other unmeasurable quantity
		// here.
		return v, true
	}

	v.Probe = &p

	minTrades := ProbeMinTrades(s.cfg.Probe)

	if p.Trades < minTrades {
		// Too thin to mean anything. The candidate keeps its cheap-filter
		// ranking instead of being judged on a handful of fills — and the
		// verdict still carries the probe, so an operator can see exactly why
		// it was not used.
		v.Reason = fmt.Sprintf("passed (%s ignored: only %d trades, need %d)",
			p.Source, p.Trades, minTrades)

		return v, true
	}

	if p.ExpectancyPct < s.cfg.Probe.MinExpectancyPct {
		v.Passed = false
		v.Reason = fmt.Sprintf(
			"%s: expectancy %.3f%%/trade below min %.3f%% over %d trades",
			p.Source, p.ExpectancyPct*100, s.cfg.Probe.MinExpectancyPct*100, p.Trades)

		return v, false
	}

	// A measured result outranks an indicator proxy: every cheap rank is a
	// guess at "would this have traded well", and this is the answer.
	v.Rank = p.Score
	v.rankTier = rankTierOf(p.Source)
	v.Reason = fmt.Sprintf("passed (%s: %.2f%% return, %.0f%% win, %.3f%%/trade over %d)",
		p.Source, p.ReturnPct, p.WinRatePct, p.ExpectancyPct*100, p.Trades)

	return v, true
}

// publishVerdict replaces a symbol's entry in the rendered verdict list.
//
// Matched on symbol rather than index: the per-class survivor slices were
// re-sorted by rank and no longer align with the verdict list they came from.
func publishVerdict(verdicts []Verdict, v Verdict) {
	for i := range verdicts {
		if verdicts[i].Symbol == v.Symbol {
			// Keep the bars out of the rendered list; they were working state
			// for this pass only.
			v.candles = nil
			verdicts[i] = v

			return
		}
	}
}

// rankTierOf is the tier a trusted probe result ranks in. Scores are only
// comparable within one source, and a cheap-filter rank not at all: sorted on
// Rank alone, a probe Score (a return %, single digits to tens) lost to every
// unprobed survivor's rank (AvgVol under the default rank: volume, thousands
// to millions), so the stage dropped exactly the candidates it had measured as
// winners and kept the ones past the cutoff, the thin and the unscorable
// (SC2-1/AT2-1). So: every probed winner above every unprobed candidate, and
// the two sources apart.
//
// The two sources only ever meet with BOTH probe legs on — each alone
// produces one source — and there the search's result replaces the
// backtest's, so a candidate carries a "backtest" probe only when its
// search could not run: the run slot was busy, or the winner was too thin.
// Ranking that fallback first, as this did, let the candidates that
// happened to collide with an autotune pass outrank every candidate that
// was actually searched (AT3-3). The searched ones rank first.
func rankTierOf(source string) int {
	if source == "autotune" {
		return 2
	}

	return 1
}
