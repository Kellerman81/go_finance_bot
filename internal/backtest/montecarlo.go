package backtest

import (
	"math"
	"math/rand"
	"slices"
)

// MonteCarlo is a bootstrap over the realised round trips: the same trades,
// dealt in a different order, many times over.
//
// A backtest reports one number because it saw one sequence of events. Reorder
// the same trades and the endpoint barely moves, but the *path* changes a great
// deal — and the path is what decides whether you would still have been trading
// at the end. Two strategies with an identical 20% return are not equivalent if
// one of them reaches a 45% drawdown in a tenth of its orderings.
//
// What this cannot tell you: the bootstrap resamples with replacement, so it
// assumes the trades are independent and identically distributed. They are not.
// It cannot see serial correlation (a losing streak clustered in one regime), it
// cannot invent outcomes the strategy never produced, and it says nothing about
// a market that behaves unlike the sample. Treat the spread as a lower bound on
// uncertainty, never an upper one.
type MonteCarlo struct {
	Runs   int `json:"runs"`
	Trades int `json:"trades"`

	// Return percentiles over the resampled orderings.
	ReturnP5   float64 `json:"return_p5_pct"`
	ReturnP25  float64 `json:"return_p25_pct"`
	ReturnP50  float64 `json:"return_p50_pct"`
	ReturnP75  float64 `json:"return_p75_pct"`
	ReturnP95  float64 `json:"return_p95_pct"`
	ReturnMean float64 `json:"return_mean_pct"`

	// Drawdown percentiles. P95 is the one to look at: the bad-but-not-absurd
	// case you should be sized to survive.
	MaxDDP50   float64 `json:"max_dd_p50_pct"`
	MaxDDP95   float64 `json:"max_dd_p95_pct"`
	MaxDDP99   float64 `json:"max_dd_p99_pct"`
	MaxDDWorst float64 `json:"max_dd_worst_pct"`

	// ProbProfit is the share of orderings that finished above the starting
	// cash. A strategy with a good return but a coin-flip probability of profit
	// earned it from a couple of trades.
	ProbProfit float64 `json:"prob_profit"`
	// ProbRuin is the share of orderings whose drawdown exceeded RuinPct.
	ProbRuin float64 `json:"prob_ruin"`
	RuinPct  float64 `json:"ruin_pct"`
}

// defaultRuinPct is the drawdown treated as "ruin" when none is configured —
// the point at which most people stop the bot whether or not the maths says to.
const defaultRuinPct = 0.30

// Bootstrap resamples the round trips `runs` times, rebuilding an equity path
// from startCash for each ordering, and returns the distribution of outcomes.
//
// Deterministic for a given seed. Returns nil when there are too few trades for
// the exercise to mean anything: bootstrapping three trades produces a
// confident-looking distribution of three numbers.
func Bootstrap(
	trips []RoundTrip,
	startCash float64,
	runs int,
	ruinPct float64,
	seed int64,
) *MonteCarlo {
	const minTrades = 10

	if len(trips) < minTrades || startCash <= 0 || runs < 1 {
		return nil
	}

	if ruinPct <= 0 {
		ruinPct = defaultRuinPct
	}

	pnl := pnlSeries(trips)
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic resampling

	returns := make([]float64, runs)
	drawdowns := make([]float64, runs)

	var (
		profitable int
		ruined     int
		sumReturn  float64
	)

	for r := range runs {
		equity, worstDD := resamplePath(rng, pnl, startCash)
		ret := (equity/startCash - 1) * 100

		returns[r] = ret
		drawdowns[r] = worstDD * 100

		sumReturn += ret

		if equity > startCash {
			profitable++
		}

		if worstDD >= ruinPct {
			ruined++
		}
	}

	slices.Sort(returns)
	slices.Sort(drawdowns)

	return &MonteCarlo{
		Runs: runs, Trades: len(trips),
		ReturnP5: percentile(returns, 5), ReturnP25: percentile(returns, 25),
		ReturnP50: percentile(returns, 50), ReturnP75: percentile(returns, 75),
		ReturnP95:  percentile(returns, 95),
		ReturnMean: sumReturn / float64(runs),
		MaxDDP50:   percentile(drawdowns, 50),
		MaxDDP95:   percentile(drawdowns, 95),
		MaxDDP99:   percentile(drawdowns, 99),
		MaxDDWorst: drawdowns[len(drawdowns)-1],
		ProbProfit: float64(profitable) / float64(runs),
		ProbRuin:   float64(ruined) / float64(runs),
		RuinPct:    ruinPct * 100,
	}
}

// resamplePath draws len(pnl) trades with replacement and walks the equity path
// they produce, returning the final equity and the worst drawdown along the way.
//
// With replacement, not a shuffle: the question is "what else could this strategy
// plausibly have produced", and shuffling would fix the endpoint and vary only
// the path.
func resamplePath(rng *rand.Rand, pnl []float64, startCash float64) (equity, worstDD float64) {
	equity = startCash

	peak := startCash

	for range pnl {
		equity += pnl[rng.Intn(len(pnl))]

		if equity > peak {
			peak = equity
		}

		if peak <= 0 {
			continue
		}

		if dd := (peak - equity) / peak; dd > worstDD {
			worstDD = dd
		}
	}

	return equity, worstDD
}

// percentile returns the p-th percentile (0..100) of an already-sorted series,
// interpolating between the two neighbouring ranks.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	switch n {
	case 0:
		return 0
	case 1:
		return sorted[0]
	}

	pos := p / 100 * float64(n-1)

	lo := int(math.Floor(pos))
	if lo < 0 {
		lo = 0
	}

	if lo >= n-1 {
		return sorted[n-1]
	}

	// lo < n-1 is guaranteed by the early return above, so lo and lo+1 are both
	// in range; gosec cannot follow that bound.
	below, above := sorted[lo], sorted[lo+1] //nolint:gosec // bounded by the check above

	return below + (above-below)*(pos-float64(lo))
}
