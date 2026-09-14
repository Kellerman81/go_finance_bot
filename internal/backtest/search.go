package backtest

import (
	"context"
	"fmt"
	"math/rand"
	"slices"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// candidate is one fully-specified point in the search space: the strategy
// parameters plus the deployment and exit levels that are searched alongside
// them.
type candidate struct {
	sc         config.StrategyConfig
	frac       float64 // order size as a fraction of starting cash
	sl, ts, tp float64 // stop-loss / trailing-stop / take-profit
}

// searchSpace holds the candidate values every tunable parameter is drawn from.
// Keeping them in one place means random search, guided search and the
// sensitivity sweep all explore the same space, so their results are about the
// strategy rather than about which tool happened to try which values.
type searchSpace struct {
	grid ParamGrid

	maPairs     [][2]int
	rsiPeriods  []int
	rsiOB       []float64
	rsiOS       []float64
	macdSets    [][3]int
	bbPeriods   []int
	bbStd       []float64
	vwapPeriods []int
	atrPeriods  []int
	atrMults    []float64
	rvolPeriods []int
	rvolThr     []float64
	vpWindows   []int
	vpBuckets   []int
	adxPeriods  []int
	adxThr      []float64
	stochK      []int
	stochD      []int
	stochOB     []float64
	stochOS     []float64
	stPeriods   []int
	stMults     []float64
	dcPeriods   []int
}

// Pins fixes individual deployment axes to a single value instead of drawing
// them from the grid, for a caller that cannot APPLY a change to that axis and
// therefore has nothing to gain by exploring it.
//
// internal/autotune is that caller: it can only write StrategyConfig fields
// into a per-symbol strategy_overrides entry (see config.SymbolOverrideFields),
// so CapitalFrac and the three exit levels are unappliable for it. Leaving them
// free does active harm rather than merely wasting samples — the search happily
// wins on them (a bigger position scores better on every size-sensitive
// objective regardless of whether the signal improved), and the resulting trial
// is then rejected wholesale for depending on a setting that cannot be written
// back. See CODE_REVIEW_FINDINGS.md's AT-1 and AT-11 findings.
//
// A nil field explores the grid as usual, so the zero value is "search
// everything" — what cmd/optimize wants, since it applies its result by hand to
// the shared config where every one of these axes IS settable. Pointers rather
// than a "0 means unpinned" sentinel because 0 is a legitimate pinned VALUE for
// TrailingStop and TakeProfit: those are the grid's own "no trailing stop" and
// "ride the trend" settings.
type Pins struct {
	CapitalFrac  *float64
	StopLoss     *float64
	TrailingStop *float64
	TakeProfit   *float64
}

// defaultSpace returns the search space used by every sampler, with p's pinned
// axes collapsed to a single value (see Pins) and base's own detector set and
// min_strength added as candidates when the grid does not already offer them.
//
// Seeding the incumbent in matters because the grid is a fixed menu: ten
// detector sets and min_strength {0.10, 0.20, 0.30}. A config the operator
// actually runs — a hand-tuned min_strength of 0.25, or any detector set that
// isn't one of the ten — is otherwise UNREACHABLE, so the search cannot even
// consider "keep the detectors, tune the periods" and every proposal it makes
// necessarily changes the detector set. Adding the incumbent costs one extra
// value on two axes and makes the incumbent a candidate that has to be beaten
// on its own terms. See CODE_REVIEW_FINDINGS.md's AT-13 finding.
func defaultSpace(p Pins, base config.StrategyConfig) searchSpace {
	grid := DefaultGrid()

	if p.CapitalFrac != nil {
		grid.CapitalFrac = []float64{*p.CapitalFrac}
	}

	if p.StopLoss != nil {
		grid.StopLoss = []float64{*p.StopLoss}
	}

	if p.TrailingStop != nil {
		grid.TrailingStop = []float64{*p.TrailingStop}
	}

	if p.TakeProfit != nil {
		grid.TakeProfit = []float64{*p.TakeProfit}
	}

	if len(base.Detectors) > 0 && !slices.ContainsFunc(grid.DetectorSets, func(s []string) bool {
		return slices.Equal(s, base.Detectors)
	}) {
		grid.DetectorSets = append(grid.DetectorSets, slices.Clone(base.Detectors))
	}

	if base.MinStrength > 0 && !slices.Contains(grid.MinStrength, base.MinStrength) {
		grid.MinStrength = append(grid.MinStrength, base.MinStrength)
	}

	return searchSpace{
		grid:        grid,
		maPairs:     [][2]int{{8, 21}, {10, 30}, {12, 26}, {20, 50}},
		rsiPeriods:  []int{7, 9, 14, 21},
		rsiOB:       []float64{70, 75, 80},
		rsiOS:       []float64{20, 25, 30},
		macdSets:    [][3]int{{12, 26, 9}, {5, 35, 5}, {8, 21, 5}, {10, 20, 7}},
		bbPeriods:   []int{14, 20, 30},
		bbStd:       []float64{1.5, 2.0, 2.5},
		vwapPeriods: []int{14, 20, 50},
		atrPeriods:  []int{10, 14, 21},
		atrMults:    []float64{1.0, 1.5, 2.0, 2.5},
		rvolPeriods: []int{10, 20, 30},
		rvolThr:     []float64{1.2, 1.5, 2.0},
		vpWindows:   []int{60, 120, 200},
		vpBuckets:   []int{16, 24, 32},
		adxPeriods:  []int{10, 14, 20},
		adxThr:      []float64{20, 25, 30},
		stochK:      []int{9, 14, 21},
		stochD:      []int{3, 5},
		stochOB:     []float64{75, 80, 85},
		stochOS:     []float64{15, 20, 25},
		stPeriods:   []int{7, 10, 14},
		stMults:     []float64{2.0, 3.0, 4.0},
		dcPeriods:   []int{10, 20, 55},
	}
}

// sample draws a uniformly-random candidate from the whole space.
func (s searchSpace) sample(rng *rand.Rand, base config.StrategyConfig) candidate {
	c := candidate{sc: base}

	c.sc.Detectors = pick(rng, s.grid.DetectorSets)
	c.sc.Combine = pick(rng, s.grid.Combine)
	c.sc.MinStrength = pick(rng, s.grid.MinStrength)

	ma := pick(rng, s.maPairs)

	c.sc.FastMA, c.sc.SlowMA = ma[0], ma[1]

	md := pick(rng, s.macdSets)

	c.sc.MACDFast, c.sc.MACDSlow, c.sc.MACDSignal = md[0], md[1], md[2]

	c.sc.RSIPeriod = pick(rng, s.rsiPeriods)
	c.sc.RSIOverbought = pick(rng, s.rsiOB)
	c.sc.RSIOversold = pick(rng, s.rsiOS)
	c.sc.BBPeriod = pick(rng, s.bbPeriods)
	c.sc.BBStdDev = pick(rng, s.bbStd)
	c.sc.VWAPPeriod = pick(rng, s.vwapPeriods)
	c.sc.ATRPeriod = pick(rng, s.atrPeriods)
	c.sc.ATRMult = pick(rng, s.atrMults)
	c.sc.RVOLPeriod = pick(rng, s.rvolPeriods)
	c.sc.RVOLThreshold = pick(rng, s.rvolThr)
	c.sc.VPWindow = pick(rng, s.vpWindows)
	c.sc.VPBuckets = pick(rng, s.vpBuckets)
	c.sc.ADXPeriod = pick(rng, s.adxPeriods)
	c.sc.ADXThreshold = pick(rng, s.adxThr)
	c.sc.StochKPeriod = pick(rng, s.stochK)
	c.sc.StochDPeriod = pick(rng, s.stochD)
	c.sc.StochOverbought = pick(rng, s.stochOB)
	c.sc.StochOversold = pick(rng, s.stochOS)
	c.sc.SupertrendPeriod = pick(rng, s.stPeriods)
	c.sc.SupertrendMult = pick(rng, s.stMults)
	c.sc.DonchianPeriod = pick(rng, s.dcPeriods)

	c.frac = pick(rng, s.grid.CapitalFrac)
	c.sl = pick(rng, s.grid.StopLoss)
	c.ts = pick(rng, s.grid.TrailingStop)
	c.tp = pick(rng, s.grid.TakeProfit)

	return c
}

// mutate returns a neighbour of parent: each axis is independently resampled
// with probability rate, and otherwise inherited. It is the exploration step of
// the guided search — small rate means "look around this configuration", large
// rate means "start over from a hint".
//
//nolint:cyclop,funlen // one independent coin flip per tunable axis
func (s searchSpace) mutate(rng *rand.Rand, parent candidate, rate float64) candidate {
	c := parent

	flip := func() bool { return rng.Float64() < rate }

	if flip() {
		c.sc.Detectors = pick(rng, s.grid.DetectorSets)
	}

	if flip() {
		c.sc.Combine = pick(rng, s.grid.Combine)
	}

	if flip() {
		c.sc.MinStrength = pick(rng, s.grid.MinStrength)
	}

	if flip() {
		ma := pick(rng, s.maPairs)

		c.sc.FastMA, c.sc.SlowMA = ma[0], ma[1]
	}

	if flip() {
		md := pick(rng, s.macdSets)

		c.sc.MACDFast, c.sc.MACDSlow, c.sc.MACDSignal = md[0], md[1], md[2]
	}

	if flip() {
		c.sc.RSIPeriod = pick(rng, s.rsiPeriods)
	}

	if flip() {
		c.sc.RSIOverbought = pick(rng, s.rsiOB)
	}

	if flip() {
		c.sc.RSIOversold = pick(rng, s.rsiOS)
	}

	if flip() {
		c.sc.BBPeriod = pick(rng, s.bbPeriods)
	}

	if flip() {
		c.sc.BBStdDev = pick(rng, s.bbStd)
	}

	if flip() {
		c.sc.VWAPPeriod = pick(rng, s.vwapPeriods)
	}

	if flip() {
		c.sc.ATRPeriod = pick(rng, s.atrPeriods)
	}

	if flip() {
		c.sc.ATRMult = pick(rng, s.atrMults)
	}

	if flip() {
		c.sc.RVOLPeriod = pick(rng, s.rvolPeriods)
	}

	if flip() {
		c.sc.RVOLThreshold = pick(rng, s.rvolThr)
	}

	if flip() {
		c.sc.VPWindow = pick(rng, s.vpWindows)
	}

	if flip() {
		c.sc.VPBuckets = pick(rng, s.vpBuckets)
	}

	if flip() {
		c.sc.ADXPeriod = pick(rng, s.adxPeriods)
	}

	if flip() {
		c.sc.ADXThreshold = pick(rng, s.adxThr)
	}

	if flip() {
		c.sc.StochKPeriod = pick(rng, s.stochK)
	}

	if flip() {
		c.sc.StochDPeriod = pick(rng, s.stochD)
	}

	if flip() {
		c.sc.StochOverbought = pick(rng, s.stochOB)
	}

	if flip() {
		c.sc.StochOversold = pick(rng, s.stochOS)
	}

	if flip() {
		c.sc.SupertrendPeriod = pick(rng, s.stPeriods)
	}

	if flip() {
		c.sc.SupertrendMult = pick(rng, s.stMults)
	}

	if flip() {
		c.sc.DonchianPeriod = pick(rng, s.dcPeriods)
	}

	if flip() {
		c.frac = pick(rng, s.grid.CapitalFrac)
	}

	if flip() {
		c.sl = pick(rng, s.grid.StopLoss)
	}

	if flip() {
		c.ts = pick(rng, s.grid.TrailingStop)
	}

	if flip() {
		c.tp = pick(rng, s.grid.TakeProfit)
	}

	return c
}

// signature uniquely identifies a candidate, so the same configuration is never
// backtested twice within a run.
func (c candidate) signature() string {
	sc := c.sc

	return fmt.Sprintf(
		"%v|%s|%.2f|%d-%d|%d/%.0f/%.0f|%d-%d-%d|%d/%.1f|%d|%d/%.1f|%d/%.1f|%d/%d|%d/%.0f|%d/%d/%.0f/%.0f|%d/%.1f|%d|%.2f|%.2f-%.2f-%.2f",
		sc.Detectors,
		sc.Combine,
		sc.MinStrength,
		sc.FastMA,
		sc.SlowMA,
		sc.RSIPeriod,
		sc.RSIOverbought,
		sc.RSIOversold,
		sc.MACDFast,
		sc.MACDSlow,
		sc.MACDSignal,
		sc.BBPeriod,
		sc.BBStdDev,
		sc.VWAPPeriod,
		sc.ATRPeriod,
		sc.ATRMult,
		sc.RVOLPeriod,
		sc.RVOLThreshold,
		sc.VPWindow,
		sc.VPBuckets,
		sc.ADXPeriod,
		sc.ADXThreshold,
		sc.StochKPeriod,
		sc.StochDPeriod,
		sc.StochOverbought,
		sc.StochOversold,
		sc.SupertrendPeriod,
		sc.SupertrendMult,
		sc.DonchianPeriod,
		c.frac,
		c.sl,
		c.ts,
		c.tp,
	)
}

// toCandidate reconstructs the search point a completed trial represents, so a
// good trial can be used as the parent of the next generation.
func toCandidate(t Trial) candidate {
	return candidate{
		sc:   t.Params,
		frac: t.CapitalFrac,
		sl:   t.StopLoss,
		ts:   t.TrailingStop,
		tp:   t.TakeProfit,
	}
}

// runner evaluates candidates, skipping duplicates and collecting the trials.
type runner struct {
	ctx           context.Context //nolint:containedctx // eval is called from a tight sampling loop the caller doesn't otherwise thread ctx through; see newRunner
	base          config.Config
	data          map[string][]market.Candle
	cash          float64
	minTrades     int
	score         Scorer
	respectLimits bool

	seen   map[string]bool
	trials []Trial
	prog   *progressBar
	done   int
}

// newRunner creates a runner that reports progress over an expected total.
//
// ctx is checked on every eval call; once cancelled, eval becomes a cheap
// no-op instead of running another backtest, so a caller's sampling loop
// (which does not otherwise know about ctx) drains out quickly rather than
// running to completion after the operator has asked to stop. A nil ctx
// runs to completion exactly as before. See CODE_REVIEW_FINDINGS.md's
// "cmd/optimize: no cancellation, no incremental result persistence".
func newRunner(ctx context.Context, base config.Config, data map[string][]market.Candle, cash float64,
	minTrades int, score Scorer, respectLimits bool, total int,
) *runner {
	return &runner{
		ctx:  ctx,
		base: base, data: data, cash: cash, minTrades: minTrades,
		score: score, respectLimits: respectLimits,
		seen: make(map[string]bool), prog: newProgress(total),
	}
}

// eval backtests one candidate unless an identical one has already been run,
// or ctx has been cancelled.
func (r *runner) eval(c candidate) {
	if r.ctx != nil && r.ctx.Err() != nil {
		return
	}

	r.done++

	r.prog.tick(r.done)

	sig := c.signature()
	if r.seen[sig] {
		return
	}

	r.seen[sig] = true

	if t, ok := runTrial(r.base, r.data, c.sc, c.frac, c.sl, c.ts, c.tp,
		r.cash, r.minTrades, r.score, r.respectLimits); ok {
		r.trials = append(r.trials, t)
	}
}

// results returns the trials collected so far, best-first.
func (r *runner) results() []Trial {
	sortTrials(r.trials)

	return r.trials
}
