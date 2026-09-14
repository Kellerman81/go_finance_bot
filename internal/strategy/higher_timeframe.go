package strategy

import (
	"fmt"
	"strings"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// htfMode selects how the higher timeframe is applied to a signal.
type htfMode string

const (
	// htfGate vetoes signals that oppose the higher timeframe.
	htfGate htfMode = "gate"
	// htfVote folds the higher timeframe into the tally as one more opinion.
	htfVote htfMode = "vote"
)

// higherTimeframe evaluates a detector set on bars aggregated up from the base
// timeframe, giving the strategy a view of the larger trend it is trading
// inside.
//
// A buy signal on 1-minute bars during an hourly downtrend is usually noise
// dressed up as an entry. Rather than guess, this re-runs real detectors on
// genuinely coarser bars and either vetoes the counter-trend side (gate) or
// lets it argue its case alongside the others (vote).
type higherTimeframe struct {
	multiple  int
	mode      htfMode
	detectors []Detector
	warmup    int // in *higher-timeframe* bars: the floor vote() waits for
	// maxLookback is, like Combined.MaxLookback, the most *higher-timeframe*
	// bars any HTF detector can use — at least warmup, but more for a
	// long-lookback detector such as the default "trend" (whose WarmupBars
	// is only trendMinBars=20, far short of its configured TrendPeriod).
	// BaseBars sizes off this, not warmup, so a backtest gives the HTF
	// detectors as much aggregated history as they'd see live instead of
	// silently capping them to their bare readiness floor. See
	// CODE_REVIEW_FINDINGS.md's "higherTimeframe.BaseBars() under-reports".
	maxLookback int
}

// newHigherTimeframe builds the confirmation timeframe from config, or nil when
// it is not enabled.
func newHigherTimeframe(cfg config.StrategyConfig) *higherTimeframe {
	if cfg.HTFMultiple < 2 {
		return nil
	}

	names := cfg.HTFDetectors
	if len(names) == 0 {
		// A confirmation timeframe is asking one question — "which way is the
		// larger move going?" — and the regression trend answers exactly that.
		names = []string{"trend"}
	}

	dets, warmup := buildDetectorList(names, cfg)
	if len(dets) == 0 {
		return nil
	}

	mode := htfGate
	if strings.EqualFold(strings.TrimSpace(cfg.HTFMode), string(htfVote)) {
		mode = htfVote
	}

	maxLookback := warmup
	for _, d := range dets {
		if n := detectorMaxLookback(d); n > maxLookback {
			maxLookback = n
		}
	}

	return &higherTimeframe{
		multiple: cfg.HTFMultiple, mode: mode, detectors: dets, warmup: warmup,
		maxLookback: maxLookback,
	}
}

// BaseBars is how many base-timeframe bars the backtester should feed so the
// higher timeframe sees as much aggregated history as it would live — sized
// off maxLookback, not the (potentially far smaller) warmup readiness floor.
func (h *higherTimeframe) BaseBars() int {
	if h == nil {
		return 0
	}

	return (h.maxLookback + 2) * h.multiple
}

// htfRead is the higher timeframe's opinion. ok is false when there is not
// enough history to aggregate, in which case the caller must not gate on it — an
// unknown higher timeframe should leave the base strategy alone, not silently
// block every trade.
type htfRead struct {
	action   Action
	strength float64
	reason   string
	ok       bool
}

// vote evaluates the higher-timeframe detectors and returns their merged
// direction.
func (h *higherTimeframe) vote(candles []market.Candle) htfRead {
	if h == nil {
		return htfRead{action: Hold}
	}

	agg := market.ResampleEvery(candles, h.multiple)
	if len(agg) < h.warmup {
		return htfRead{action: Hold, reason: fmt.Sprintf("htf: warming up (%d/%d bars of %dx)",
			len(agg), h.warmup, h.multiple)}
	}

	s := acquireSeries(agg)
	defer releaseSeries(s)

	// The confirmation timeframe has no symbol-aware detectors by design: an LLM
	// verdict is cached per symbol, not per timeframe, so routing it here would
	// return the base-timeframe answer and pretend it was a higher one.
	votes, buyN, sellN, buyStr, sellStr := tallyDetectors(h.detectors, "", s, nil)
	reason := fmt.Sprintf("htf(%dx): %s", h.multiple, summarise(votes))

	releaseVotes(votes)

	switch {
	case buyN > sellN:
		return htfRead{Buy, buyStr / float64(max(buyN, 1)), reason, true}
	case sellN > buyN:
		return htfRead{Sell, sellStr / float64(max(sellN, 1)), reason, true}
	}

	return htfRead{Hold, 0, fmt.Sprintf("htf(%dx): undecided", h.multiple), true}
}

// tally is a detector set's merged votes: who voted which way, and how strongly.
type tally struct {
	votes           []detectorVote
	buyN, sellN     int
	buyStr, sellStr float64
}

// addHTFVote folds the higher-timeframe opinion into a tally, as though it were
// one more detector. An undecided higher timeframe abstains rather than counting
// as a HOLD vote, so it cannot dilute a majority.
func addHTFVote(t tally, htf detectorVote) tally {
	t.votes = append(t.votes, htf)

	switch htf.action {
	case Buy:
		t.buyN++

		t.buyStr += htf.strength

	case Sell:
		t.sellN++

		t.sellStr += htf.strength

	case Hold:
	}

	return t
}

// summarise renders the higher-timeframe votes compactly for the reason string.
func summarise(votes []detectorVote) string {
	parts := make([]string, len(votes))
	for i, v := range votes {
		parts[i] = fmt.Sprintf("%s:%s(%.2f)", v.name, v.action, v.strength)
	}

	return strings.Join(parts, ", ")
}
