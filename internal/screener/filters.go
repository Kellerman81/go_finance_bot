package screener

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/strategy/indicators"
)

// minTrendBars is the fewest closes a regression fit is trusted on. Below this
// a line through the points says more about the sample than the instrument.
const minTrendBars = 20

// The filter chain.
//
// Each filter is a named test over one candidate's measurements. They run in a
// configured order and evaluation stops at the first failure, so the verdict
// names the binding constraint instead of listing everything wrong at once.
//
// Splitting them out this way is what makes the set composable: a filter can be
// dropped from screener.filters entirely rather than disabled by guessing which
// magic value means "no bound", and the cheap tests can be ordered ahead of the
// ones that cost an indicator pass.

// Filter is one named screening test. It returns an empty string when the
// candidate passes, or the reason it does not.
type Filter struct {
	// Name is the identifier used in screener.filters.
	Name string
	// Test judges a candidate. m carries every measurement taken from its bars.
	Test func(cfg config.ScreenerConfig, m *Measures) string
}

// Measures are the values computed once per candidate and shared by the filters,
// so ordering the chain differently never changes what anything measures.
type Measures struct {
	Bars      int
	Price     float64
	AvgVol    float64
	ATRPct    float64
	SpreadPct float64
	// SpreadOK is false when the spread estimator could not produce a value
	// (a broken feed, NaN inputs) — distinct from a genuinely tight spread,
	// which also happens to be near 0. The zero value of SpreadPct is
	// otherwise indistinguishable from that. See the "spread" filter below
	// and CODE_REVIEW_FINDINGS.md's "the spread filter fails open".
	SpreadOK bool
	Momentum float64
	// MomentumOK is false when there isn't enough history to measure
	// momentum (e.g. a freshly-listed symbol) — distinct from genuinely flat
	// (0%) momentum. See rankOf and CODE_REVIEW_FINDINGS.md's "undefined
	// momentum ranks as exactly 0".
	MomentumOK bool

	// TrendSlopePct is the least-squares regression slope over the window as
	// a percentage of price PER BAR, and TrendR2 how much of the variance
	// that line explains (0..1).
	//
	// The two answer different halves of one question. Slope says which way
	// and how fast; R² says whether "trend" is even the right word for it. A
	// sideways chop that happens to end higher than it started has a real
	// positive slope and an R² near zero — momentum alone cannot tell it
	// apart from a clean advance, and trading it as a trend is how a
	// trend-following config bleeds out in a range.
	TrendSlopePct float64
	TrendR2       float64
	// TrendOK is false when there were too few bars to fit a line at all —
	// distinct from a genuinely flat, genuinely noisy result.
	TrendOK bool

	// MaxDrawdownPct is the candidate's own worst peak-to-trough decline over
	// the window, as a fraction. A falling knife passes a volatility band
	// (it is certainly moving) and can pass momentum on a dead-cat bounce;
	// this is the measure that refuses it.
	MaxDrawdownPct float64

	// RoundTripCostPct is what one complete trade costs in this instrument —
	// its spread paid once (half on each fill), plus commission and slippage
	// on both legs — as a fraction of price (SC2-5; this comment still said
	// "paid twice" until SC3-8). TypicalMovePct is the median absolute
	// close-to-close move over the same window.
	//
	// Their ratio is the only screening number that decides whether an
	// instrument can be profitable AT ALL: something that moves 0.1% a bar
	// against a 0.15% round trip loses money at every win rate, and no
	// strategy setting fixes that. CostOK is false when the spread could not
	// be measured, so the ratio is unknown rather than flattering.
	RoundTripCostPct float64
	TypicalMovePct   float64
	CostOK           bool
}

// CostMultiple is how many times its own round-trip cost the instrument
// typically moves in a bar. Higher is more tradeable; below ~2 there is
// essentially no room for a strategy to be right often enough.
func (m *Measures) CostMultiple() float64 {
	if !m.CostOK || m.RoundTripCostPct <= 0 {
		return 0
	}

	return m.TypicalMovePct / m.RoundTripCostPct
}

// DefaultFilterOrder is the chain used when screener.filters is empty. History
// is first because a candidate without enough bars cannot be measured at all,
// and spread is last because it is the only one that costs a second pass over
// the series.
var DefaultFilterOrder = []string{"history", "price", "volume", "volatility", "spread"}

// AllFilterNames is every filter in the registry, in a sensible running order —
// the cheap structural tests first, the ones needing a full indicator pass
// last. Offered by the API as "everything available", distinct from
// DefaultFilterOrder which is what runs when nothing is configured.
var AllFilterNames = []string{
	"history", "price", "volume", "volatility", "trend", "drawdown", "spread", "cost",
}

// filters is the registry of every available screening test.
var filters = map[string]Filter{
	"history": {
		Name: "history",
		Test: func(cfg config.ScreenerConfig, m *Measures) string {
			need := max(cfg.MinHistoryBars, atrPeriod+2)
			if m.Bars < need {
				return fmt.Sprintf("only %d bars, need %d", m.Bars, need)
			}

			return ""
		},
	},
	"price": {
		Name: "price",
		Test: func(cfg config.ScreenerConfig, m *Measures) string {
			switch {
			case m.Price <= 0:
				return "no price"
			case cfg.MinPrice > 0 && m.Price < cfg.MinPrice:
				return fmt.Sprintf("price %.2f below min %.2f", m.Price, cfg.MinPrice)
			case cfg.MaxPrice > 0 && m.Price > cfg.MaxPrice:
				return fmt.Sprintf("price %.2f above max %.2f", m.Price, cfg.MaxPrice)
			}

			return ""
		},
	},
	"volume": {
		Name: "volume",
		Test: func(cfg config.ScreenerConfig, m *Measures) string {
			if cfg.MinAvgVolume > 0 && m.AvgVol < cfg.MinAvgVolume {
				return fmt.Sprintf("avg volume %.0f below min %.0f", m.AvgVol, cfg.MinAvgVolume)
			}

			return ""
		},
	},
	"volatility": {
		Name: "volatility",
		Test: func(cfg config.ScreenerConfig, m *Measures) string {
			switch {
			case cfg.MinVolatilityPct > 0 && m.ATRPct < cfg.MinVolatilityPct:
				return fmt.Sprintf("volatility %.2f%% below min %.2f%%",
					m.ATRPct, cfg.MinVolatilityPct)

			case cfg.MaxVolatilityPct > 0 && m.ATRPct > cfg.MaxVolatilityPct:
				return fmt.Sprintf("volatility %.2f%% above max %.2f%%",
					m.ATRPct, cfg.MaxVolatilityPct)
			}

			return ""
		},
	},
	"trend": {
		Name: "trend",
		Test: func(cfg config.ScreenerConfig, m *Measures) string {
			if cfg.MinTrendR2 <= 0 && cfg.MinTrendSlopePct <= 0 {
				return ""
			}

			// Same reasoning as the spread filter: an unmeasurable trend is
			// refused, not assumed good. This filter only runs when the
			// operator asked for a trend bound, and "we could not fit a line"
			// cannot satisfy a bound that was explicitly requested.
			if !m.TrendOK {
				return "trend unmeasurable (too few bars), refused rather than assumed trending"
			}

			if cfg.MinTrendR2 > 0 && m.TrendR2 < cfg.MinTrendR2 {
				return fmt.Sprintf(
					"trend R² %.2f below min %.2f (moves, but not in a straight line)",
					m.TrendR2, cfg.MinTrendR2)
			}

			if cfg.MinTrendSlopePct > 0 && m.TrendSlopePct < cfg.MinTrendSlopePct {
				return fmt.Sprintf("trend slope %.3f%%/bar below min %.3f%%/bar",
					m.TrendSlopePct*100, cfg.MinTrendSlopePct*100)
			}

			return ""
		},
	},
	"drawdown": {
		Name: "drawdown",
		Test: func(cfg config.ScreenerConfig, m *Measures) string {
			if cfg.MaxDrawdownPct <= 0 {
				return ""
			}

			if m.MaxDrawdownPct > cfg.MaxDrawdownPct {
				return fmt.Sprintf("own drawdown %.1f%% above max %.1f%%",
					m.MaxDrawdownPct*100, cfg.MaxDrawdownPct*100)
			}

			return ""
		},
	},
	"cost": {
		Name: "cost",
		Test: func(cfg config.ScreenerConfig, m *Measures) string {
			if cfg.MinCostMultiple <= 0 {
				return ""
			}

			// A genuinely measured zero move — a stale listing whose close
			// never changes (SC2-6) — is refused for the right reason: it
			// was reported as "unmeasurable", which sends an operator
			// looking for a broken feed (SC3-8).
			if m.SpreadOK && m.RoundTripCostPct > 0 && m.TypicalMovePct == 0 {
				return "no price movement measured (typical move 0%) — cannot clear any round-trip cost"
			}

			if !m.CostOK {
				return "round-trip cost unmeasurable, refused rather than assumed cheap"
			}

			if got := m.CostMultiple(); got < cfg.MinCostMultiple {
				return fmt.Sprintf(
					"typical move %.3f%% is only %.1fx the %.3f%% round-trip cost "+
						"(min %.1fx) — cannot clear its own costs",
					m.TypicalMovePct*100, got, m.RoundTripCostPct*100, cfg.MinCostMultiple)
			}

			return ""
		},
	},
	"spread": {
		Name: "spread",
		Test: func(cfg config.ScreenerConfig, m *Measures) string {
			if cfg.MaxSpreadPct <= 0 {
				return ""
			}

			// An unmeasurable spread (broken feed, NaN inputs) used to record
			// as exactly 0 — indistinguishable from a megacap's genuinely
			// tight spread, and so it silently passed the one filter meant
			// to catch untradeable instruments. Fail closed instead: this
			// filter is only active when the operator asked for a cap, and
			// "we can't tell" cannot mean "assume it's fine" for a cap that
			// was explicitly requested.
			if !m.SpreadOK {
				return "spread unmeasurable (broken data), refused rather than assumed tradeable"
			}

			if m.SpreadPct > cfg.MaxSpreadPct {
				return fmt.Sprintf("spread %.3f%% above max %.3f%% (paid once per round trip, half on each fill)",
					m.SpreadPct*100, cfg.MaxSpreadPct*100)
			}

			return ""
		},
	},
}

// FilterNames returns every available filter name, sorted — for the API, so the
// UI can offer the real set rather than a hard-coded copy of it.
func FilterNames() []string {
	out := make([]string, 0, len(filters))
	for name := range filters {
		out = append(out, name)
	}

	slices.Sort(out)

	return out
}

// chain resolves the configured filter order into the tests to run. Unknown
// names are skipped rather than fatal: a typo in a screening filter must not
// take a running bot down, and the omission is visible in the verdicts.
func chain(cfg config.ScreenerConfig) []Filter {
	names := cfg.Filters
	if len(names) == 0 {
		names = DefaultFilterOrder
	}

	out := make([]Filter, 0, len(names))

	for _, n := range names {
		if f, ok := filters[strings.ToLower(strings.TrimSpace(n))]; ok {
			out = append(out, f)
		}
	}

	return out
}

// measure computes every value the filters compare against, once per candidate.
func measure(cols series) Measures {
	m := Measures{Bars: len(cols.closes)}
	if m.Bars == 0 {
		return m
	}

	m.Price = cols.closes[m.Bars-1]
	m.AvgVol = mean(cols.volume)

	if m.Price > 0 {
		if atr, ok := indicators.ATRLast(cols.high, cols.low, cols.closes, atrPeriod); ok {
			m.ATRPct = atr / m.Price * 100
		}
	}

	if s, ok := indicators.HighLowSpreadAdjusted(cols.high, cols.low, cols.closes); ok {
		m.SpreadPct = s
		m.SpreadOK = true
	}

	if m.Bars > momentumBars && cols.closes[m.Bars-1-momentumBars] > 0 {
		m.Momentum = (m.Price/cols.closes[m.Bars-1-momentumBars] - 1) * 100
		m.MomentumOK = true
	}

	m.TrendSlopePct, m.TrendR2, m.TrendOK = trendFit(cols.closes)
	m.MaxDrawdownPct = maxDrawdown(cols.closes)

	return m
}

// measureCost fills the round-trip cost fields. It is separate from measure
// because it is the only measurement that needs to know the commission model,
// which is configured outside the screener.
func (m *Measures) measureCost(cols series, costs config.CostsConfig) {
	if m.Price <= 0 || !m.SpreadOK {
		return
	}

	// The cost of one complete trade, charged the way the backtester and the
	// simulated broker charge it: Corwin-Schultz S is the FULL spread, and
	// each fill pays half of it, so a round trip pays S once — "2×spread"
	// demanded twice the move it should have (SC2-5). Slippage and commission
	// are paid on both legs. The configured spread_pct stays a floor, as in
	// CostsFor — but the measured spread always counts, measure_spread or not:
	// judging each instrument's own spread is what this filter is for.
	// (Flat/minimum commission depends on order size, unknown here.)
	m.RoundTripCostPct = math.Max(
		m.SpreadPct,
		costs.SpreadPct,
	) + 2*(costs.CommissionPct+costs.SlippagePct)
	m.TypicalMovePct = medianAbsReturn(cols.closes)
	m.CostOK = m.RoundTripCostPct > 0 && m.TypicalMovePct > 0
}

// trendFit runs a least-squares fit of close against bar index and returns the
// slope as a fraction of mean price per bar, plus R².
//
// Normalising the slope by price is what makes it comparable across
// instruments: a 0.50 EUR/bar drift means something very different on a 12 EUR
// ETF than on a 900 EUR share, and an un-normalised bound would screen for
// share price rather than for trend.
func trendFit(closes []float64) (slopePct, r2 float64, ok bool) {
	n := len(closes)
	if n < minTrendBars {
		return 0, 0, false
	}

	var sumX, sumY float64

	for i, y := range closes {
		sumX += float64(i)
		sumY += y
	}

	meanX, meanY := sumX/float64(n), sumY/float64(n)
	if meanY <= 0 {
		return 0, 0, false
	}

	var sxy, sxx float64

	for i, y := range closes {
		dx := float64(i) - meanX

		sxy += dx * (y - meanY)
		sxx += dx * dx
	}

	if sxx <= 0 {
		return 0, 0, false
	}

	slope := sxy / sxx

	// R² as 1 - SSres/SStot. A perfectly flat series has no total variance to
	// explain; calling that "R² = 1" would let a dead instrument pass a trend
	// bound, so it is reported as unmeasurable instead.
	var ssRes, ssTot float64

	for i, y := range closes {
		fit := meanY + slope*(float64(i)-meanX)

		ssRes += (y - fit) * (y - fit)
		ssTot += (y - meanY) * (y - meanY)
	}

	if ssTot <= 0 {
		return 0, 0, false
	}

	r2 = 1 - ssRes/ssTot
	if math.IsNaN(r2) || math.IsInf(r2, 0) {
		return 0, 0, false
	}

	return slope / meanY, max(r2, 0), true
}

// maxDrawdown returns the worst peak-to-trough decline in the series, as a
// fraction of the peak.
func maxDrawdown(closes []float64) float64 {
	var peak, worst float64

	for _, c := range closes {
		if c > peak {
			peak = c
		}

		if peak > 0 {
			if dd := (peak - c) / peak; dd > worst {
				worst = dd
			}
		}
	}

	return worst
}

// medianAbsReturn is the median absolute bar-to-bar return — what the
// instrument typically moves between one close and the next.
//
// Close-to-close, deliberately NOT the high-low range. A trade captures the
// change in price, not the width of a bar; and the bar range is precisely what
// the Corwin-Schultz estimator derives the spread from, so dividing one by the
// other would compare a quantity against itself and produce roughly the same
// ratio for every instrument however much it really moves.
//
// Median, not mean: one gap or news bar dominates an average and would make an
// otherwise-dead instrument look tradeable.
func medianAbsReturn(closes []float64) float64 {
	if len(closes) < 2 {
		return 0
	}

	moves := make([]float64, 0, len(closes)-1)

	for i := 1; i < len(closes); i++ {
		if closes[i-1] <= 0 {
			continue
		}

		// Unchanged bars count: they are exactly what a stale, illiquid
		// listing produces, and dropping them inflated its "typical move" —
		// 80% unchanged bars and 20% at ±0.5% read as 0.5% instead of 0
		// (SC2-6). A median of 0 trips the cost filter's fail-closed path.
		moves = append(moves, math.Abs(closes[i]/closes[i-1]-1))
	}

	if len(moves) == 0 {
		return 0
	}

	slices.Sort(moves)

	mid := len(moves) / 2
	if len(moves)%2 == 1 {
		return moves[mid]
	}

	return (moves[mid-1] + moves[mid]) / 2
}
