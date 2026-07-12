// Package strategy turns price history into buy/sell/hold signals. Individual
// Detectors each produce a Signal from one indicator; a Combined strategy runs
// the configured detectors and merges (or compares) their results.
package strategy

import (
	"fmt"
	"strings"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// Action is the recommended trade direction.
type Action string

const (
	Hold Action = "HOLD"
	Buy  Action = "BUY"
	Sell Action = "SELL"
)

// Signal is the strategy's recommendation for one symbol at one point in time.
type Signal struct {
	Symbol   string  `json:"symbol"`
	Action   Action  `json:"action"`
	Strength float64 `json:"strength"` // 0..1 confidence
	Price    float64 `json:"price"`
	Reason   string  `json:"reason"`
}

// Strategy evaluates candle history and returns a Signal.
type Strategy interface {
	Name() string
	// WarmupBars is the minimum number of candles required before Evaluate
	// can return a non-Hold signal.
	WarmupBars() int
	Evaluate(symbol string, candles []market.Candle) Signal
}

// Detector is a single-indicator signal generator. Combined runs one or more.
type Detector interface {
	Name() string
	WarmupBars() int
	// Detect returns an Action with a 0..1 strength and a human reason. The
	// closes/highs/lows/volumes are aligned series of equal length.
	Detect(c series) (Action, float64, string)
}

// series holds the aligned OHLCV columns extracted from candles, shared by all
// detectors so each does not re-extract them.
type series struct {
	open, high, low, close, volume []float64
}

func toSeries(candles []market.Candle) series {
	n := len(candles)
	// One backing allocation carved into five columns: fewer allocations and
	// better locality than five separate makes, which matters in the backtest
	// hot loop where toSeries runs once per bar.
	buf := make([]float64, 5*n)
	s := series{
		open:   buf[0:n:n],
		high:   buf[n : 2*n : 2*n],
		low:    buf[2*n : 3*n : 3*n],
		close:  buf[3*n : 4*n : 4*n],
		volume: buf[4*n : 5*n : 5*n],
	}
	for i, c := range candles {
		s.open[i], s.high[i], s.low[i], s.close[i], s.volume[i] = c.Open, c.High, c.Low, c.Close, c.Volume
	}
	return s
}

// CombineMode selects how multiple detectors' results are merged.
type CombineMode string

const (
	// Majority: act on the side with strictly more votes.
	Majority CombineMode = "majority"
	// Unanimous: act only when all opinionated detectors agree (none opposes).
	Unanimous CombineMode = "unanimous"
	// Weighted: act on the sign of the net strength across detectors.
	Weighted CombineMode = "weighted"
)

// Combined runs a set of detectors and merges their signals according to mode.
// When more than one detector is configured, the reason string compares each
// detector's individual result.
type Combined struct {
	detectors       []Detector  // BUY detector set
	sellDetectors   []Detector  // SELL detector set (== detectors when sellSame)
	sellSame        bool        // sell uses the same detectors as buy
	mode            CombineMode // BUY decisions
	sellMode        CombineMode // SELL decisions (may differ — e.g. quick to exit)
	warmup          int
	minStrength     float64 // conviction gate for BUYs
	minStrengthSell float64 // conviction gate for SELLs (may be lower)

	// weights scales each detector's strength contribution by canonical name
	// (default 1.0). nil/empty means every detector is weighted equally.
	weights map[string]float64

	// trendGate, when set, makes the weekly trend a directional filter: an
	// up-trend vetoes SELLs and a down-trend vetoes BUYs. trendFilter computes
	// that direction independently of whether "trend" is in the detector sets.
	trendGate   bool
	trendFilter *trendDetector
}

func (c *Combined) Name() string {
	modes := string(c.mode)
	if c.sellMode != c.mode {
		modes = fmt.Sprintf("buy:%s/sell:%s", c.mode, c.sellMode)
	}
	sets := detectorNames(c.detectors)
	if !c.sellSame {
		sets = fmt.Sprintf("buy:%s/sell:%s", detectorNames(c.detectors), detectorNames(c.sellDetectors))
	}
	return fmt.Sprintf("combined[%s|%s]", sets, modes)
}

func detectorNames(ds []Detector) string {
	names := make([]string, len(ds))
	for i, d := range ds {
		names[i] = d.Name()
	}
	return strings.Join(names, ",")
}

func (c *Combined) WarmupBars() int { return c.warmup }

// MaxLookback reports the most bars any of the strategy's detectors (or the
// trend gate) can make use of in one evaluation — at least WarmupBars, but more
// for long-lookback detectors like trend. The backtester uses it to size its
// per-bar window so those detectors see as much history as they would live,
// instead of being silently capped to the (much smaller) warmup window.
func (c *Combined) MaxLookback() int {
	max := c.warmup
	consider := func(d Detector) {
		n := d.WarmupBars()
		if lb, ok := d.(interface{ MaxLookback() int }); ok {
			if m := lb.MaxLookback(); m > n {
				n = m
			}
		}
		if n > max {
			max = n
		}
	}
	for _, d := range c.detectors {
		consider(d)
	}
	for _, d := range c.sellDetectors {
		consider(d)
	}
	if c.trendFilter != nil {
		consider(c.trendFilter)
	}
	return max
}

func (c *Combined) Evaluate(symbol string, candles []market.Candle) Signal {
	price := 0.0
	if len(candles) > 0 {
		price = candles[len(candles)-1].Close
	}
	sig := Signal{Symbol: symbol, Action: Hold, Price: price}

	if len(candles) < c.warmup {
		sig.Reason = fmt.Sprintf("warming up (%d/%d bars)", len(candles), c.warmup)
		return sig
	}

	s := toSeries(candles)

	// BUY side: its own detector set + combine mode.
	buyVotes, bBuyN, bSellN, bBuyStr, bSellStr := tallyDetectors(c.detectors, s, c.weights)
	buyOK, buyStrength := sideQualifies(c.mode, bBuyN, bSellN, bBuyStr, bSellStr, len(buyVotes))

	// SELL side: may use a different detector set (reuse the buy tally if same).
	sellVotes := buyVotes
	sBuyN, sSellN, sBuyStr, sSellStr := bBuyN, bSellN, bBuyStr, bSellStr
	if !c.sellSame && len(c.sellDetectors) > 0 {
		sellVotes, sBuyN, sSellN, sBuyStr, sSellStr = tallyDetectors(c.sellDetectors, s, c.weights)
	}
	sellOK, sellStrength := sideQualifies(c.sellMode, sSellN, sBuyN, sSellStr, sBuyStr, len(sellVotes))

	action := Hold
	strength := 0.0
	usedMode := c.mode
	usedVotes := buyVotes
	// Prefer SELL on a (rare) tie — de-risking an open position is the safer side.
	switch {
	case sellOK && (!buyOK || sellStrength >= buyStrength):
		action, strength, usedMode, usedVotes = Sell, sellStrength, c.sellMode, sellVotes
	case buyOK:
		action, strength, usedMode, usedVotes = Buy, buyStrength, c.mode, buyVotes
	}

	// Weekly-trend gate: veto counter-trend actions. An up-trend blocks SELLs, a
	// down-trend blocks BUYs; a flat trend gates nothing. Protective exits run in
	// the engine before the strategy, so a vetoed SELL never traps a losing
	// position.
	if c.trendGate && c.trendFilter != nil && action != Hold {
		if tdir, treason := c.trendVote(s, usedVotes); (tdir == Buy && action == Sell) || (tdir == Sell && action == Buy) {
			sig.Action = Hold
			sig.Strength = 0
			sig.Reason = fmt.Sprintf("%s blocked by %s", action, treason)
			return sig
		}
	}

	sig.Strength = clamp01(strength)
	// Conviction gate (separate bars for buy vs sell, so exits can be readier).
	gate := c.minStrength
	if action == Sell {
		gate = c.minStrengthSell
	}
	if action != Hold && sig.Strength < gate {
		sig.Action = Hold
		sig.Reason = fmt.Sprintf("%s => %s %.2f<min %.2f, hold",
			formatVotes(usedVotes, action, usedMode), action, sig.Strength, gate)
		sig.Strength = 0
		return sig
	}
	sig.Action = action
	sig.Reason = formatVotes(usedVotes, action, usedMode)
	return sig
}

// trendVote returns the trend direction (and reason) for the gate. When the
// acting side's detector set already includes "trend", its tallied vote is
// reused — the gate's trendFilter is built from the same config, so re-running
// the long-lookback regression (up to a week of bars) would repeat identical
// work every evaluation.
func (c *Combined) trendVote(s series, votes []detectorVote) (Action, string) {
	for _, v := range votes {
		if v.name == "trend" {
			return v.action, v.reason
		}
	}
	a, _, reason := c.trendFilter.Detect(s)
	return a, reason
}

// tallyDetectors runs a detector set over the series and tallies its votes. Each
// detector's strength is scaled by its configured weight (default 1.0) before
// being summed; vote counts are unaffected by weight.
func tallyDetectors(dets []Detector, s series, weights map[string]float64) (votes []detectorVote, buyN, sellN int, buyStr, sellStr float64) {
	votes = make([]detectorVote, 0, len(dets))
	for _, d := range dets {
		a, str, reason := d.Detect(s)
		votes = append(votes, detectorVote{d.Name(), a, str, reason})
		w := 1.0
		if wv, ok := weights[d.Name()]; ok {
			w = wv
		}
		switch a {
		case Buy:
			buyN++
			buyStr += str * w
		case Sell:
			sellN++
			sellStr += str * w
		}
	}
	return votes, buyN, sellN, buyStr, sellStr
}

// sideQualifies reports whether a side (with sideN votes / sideStr summed
// strength) wins under the given mode against the opposing side, and the
// resulting 0..1 strength.
func sideQualifies(mode CombineMode, sideN, otherN int, sideStr, otherStr float64, total int) (bool, float64) {
	switch mode {
	case Unanimous:
		return sideN > 0 && otherN == 0, sideStr / float64(maxi(sideN, 1))
	case Weighted:
		if total == 0 {
			return false, 0
		}
		net := sideStr - otherStr
		return net > 0, net / float64(total)
	default: // Majority
		return sideN > otherN, sideStr / float64(maxi(sideN, 1))
	}
}

// detectorVote captures one detector's result for tallying and reporting.
type detectorVote struct {
	name     string
	action   Action
	strength float64
	reason   string
}

func formatVotes(votes []detectorVote, result Action, mode CombineMode) string {
	parts := make([]string, len(votes))
	for i, v := range votes {
		parts[i] = fmt.Sprintf("%s:%s(%.2f)", v.name, v.action, v.strength)
	}
	if len(votes) == 1 {
		return votes[0].reason
	}
	return fmt.Sprintf("%s => %s by %s", strings.Join(parts, ", "), result, mode)
}

// New builds a Strategy from config. If no detectors are listed it defaults to a
// classic RSI + MACD + EMA-cross set with majority voting.
func New(cfg config.StrategyConfig) Strategy {
	names := cfg.Detectors
	if len(names) == 0 {
		names = []string{"ema_cross", "rsi", "macd"}
	}
	mode := normalizeCombine(cfg.Combine, Majority)
	sellMode := mode
	if strings.TrimSpace(cfg.CombineSell) != "" {
		sellMode = normalizeCombine(cfg.CombineSell, mode)
	}

	detectors, warmup := buildDetectorList(names, cfg)
	if len(detectors) == 0 {
		detectors = []Detector{newRSIDetector(cfg)}
		warmup = detectors[0].WarmupBars()
	}

	// SELL detector set: empty => reuse the buy set.
	sellDetectors, sellSame := detectors, true
	if len(cfg.DetectorsSell) > 0 {
		if sd, sw := buildDetectorList(cfg.DetectorsSell, cfg); len(sd) > 0 {
			sellDetectors, sellSame = sd, false
			if sw > warmup {
				warmup = sw
			}
		}
	}

	sellMin := cfg.MinStrength
	if cfg.MinStrengthSell >= 0 {
		sellMin = cfg.MinStrengthSell // explicit (possibly lower) sell bar
	}

	// Weekly-trend gate: a directional filter, independent of the detector sets.
	var trendFilter *trendDetector
	if cfg.TrendGate {
		trendFilter = newTrendDetector(cfg)
	}
	return &Combined{
		detectors: detectors, sellDetectors: sellDetectors, sellSame: sellSame,
		mode: mode, sellMode: sellMode, warmup: warmup + 2,
		minStrength: cfg.MinStrength, minStrengthSell: sellMin,
		trendGate: cfg.TrendGate, trendFilter: trendFilter,
		weights: normalizeWeights(cfg.Weights, cfg),
	}
}

// normalizeWeights re-keys the configured detector weights by canonical detector
// name, so an alias (e.g. "ema" or "bb") set in config still matches the name the
// detector reports. Returns nil for an empty map.
func normalizeWeights(in map[string]float64, cfg config.StrategyConfig) map[string]float64 {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]float64, len(in))
	for k, v := range in {
		if d := buildDetector(k, cfg); d != nil {
			out[d.Name()] = v
		} else {
			out[strings.ToLower(strings.TrimSpace(k))] = v
		}
	}
	return out
}

// buildDetectorList constructs detectors from names, returning the longest
// warmup among them.
func buildDetectorList(names []string, cfg config.StrategyConfig) (dets []Detector, warmup int) {
	for _, n := range names {
		d := buildDetector(strings.ToLower(strings.TrimSpace(n)), cfg)
		if d == nil {
			continue
		}
		dets = append(dets, d)
		if w := d.WarmupBars(); w > warmup {
			warmup = w
		}
	}
	return dets, warmup
}

// normalizeCombine parses a combine mode, falling back to def when invalid.
func normalizeCombine(s string, def CombineMode) CombineMode {
	switch m := CombineMode(strings.ToLower(strings.TrimSpace(s))); m {
	case Majority, Unanimous, Weighted:
		return m
	default:
		return def
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}
