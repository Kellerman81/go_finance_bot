// Package strategy turns price history into buy/sell/hold signals. Individual
// Detectors each produce a Signal from one indicator; a Combined strategy runs
// the configured detectors and merges (or compares) their results.
package strategy

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/errlog"
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
	// Name describes the strategy configuration.
	Name() string
	// WarmupBars is the minimum number of candles required before Evaluate
	// can return a non-Hold signal.
	WarmupBars() int
	// Evaluate runs the strategy over candles and returns the signal for symbol.
	Evaluate(symbol string, candles []market.Candle) Signal
}

// Detector is a single-indicator signal generator. Combined runs one or more.
type Detector interface {
	// Name returns the canonical detector name (e.g. "rsi").
	Name() string
	// WarmupBars is the minimum bars needed for a stable read.
	WarmupBars() int
	// Detect returns an Action with a 0..1 strength and a human reason. The
	// closes/highs/lows/volumes are aligned series of equal length.
	Detect(c series) (Action, float64, string)
}

// symbolDetector is the optional interface for detectors whose read depends on
// which instrument is being evaluated, not just on its candles — the LLM
// advisor, whose verdicts are cached per symbol. Detectors that don't implement
// it are called through the plain Detector interface.
type symbolDetector interface {
	// DetectFor is Detect with the symbol the series belongs to.
	DetectFor(symbol string, c series) (Action, float64, string)
}

// detect runs a detector, preferring its symbol-aware form when it has one.
func detect(d Detector, symbol string, s series) (Action, float64, string) {
	if sd, ok := d.(symbolDetector); ok && symbol != "" {
		return sd.DetectFor(symbol, s)
	}

	return d.Detect(s)
}

// series holds the aligned OHLCV columns extracted from candles, shared by all
// detectors so each does not re-extract them.
type series struct {
	open, high, low, close, volume []float64
	// buf is the backing array the five columns are carved from. It is kept so a
	// pooled buffer can be returned whole; nil for an unpooled series.
	buf []float64
}

// seriesPool recycles the column buffer between evaluations.
//
// A single backing allocation carved into five columns was already better than
// five separate makes, but it is still one allocation of 5·lookback float64s per
// evaluation — and an evaluation happens once per bar, per symbol. On a backtest
// of any size that dominated everything else the strategy did, purely as
// short-lived garbage: the buffer is dead the moment Evaluate returns.
//
// Pooling is safe because the columns never outlive the call. Detectors return
// (Action, float64, string) and the two that need more — the LLM advisor and the
// learned detector — copy what they keep. If a detector is ever added that
// retains a column, this pool has to go, so releaseSeries is called from exactly
// one place to make that easy to find.
var seriesPool sync.Pool

// acquireSeries extracts the OHLCV columns into a pooled buffer.
func acquireSeries(candles []market.Candle) series {
	n := len(candles)
	need := 5 * n

	var buf []float64

	if held, ok := seriesPool.Get().(*[]float64); ok && cap(*held) >= need {
		buf = (*held)[:need]
	} else {
		// Round up so a series that grows by a bar at a time (the live engine's
		// ring filling up) does not reallocate on almost every evaluation.
		buf = make([]float64, need, need+need/4+64)
	}

	s := series{
		open:   buf[0:n:n],
		high:   buf[n : 2*n : 2*n],
		low:    buf[2*n : 3*n : 3*n],
		close:  buf[3*n : 4*n : 4*n],
		volume: buf[4*n : 5*n : 5*n],
		buf:    buf,
	}

	for i, c := range candles {
		s.open[i], s.high[i], s.low[i], s.close[i], s.volume[i] = c.Open, c.High, c.Low, c.Close, c.Volume
	}

	return s
}

// releaseSeries returns a buffer acquired by acquireSeries to the pool. It is a
// no-op for an unpooled series, so callers need not know which they hold.
func releaseSeries(s series) {
	if s.buf == nil {
		return
	}

	seriesPool.Put(&s.buf)
}

// votesPool recycles the per-side vote tally. Each is small, but one is built
// per detector set per evaluation, which on a backtest is millions of them.
//
// Like the column buffer, the tally never outlives Evaluate: it is read by
// formatVotes and the trend gate, both of which return strings.
var votesPool sync.Pool

// acquireVotes returns an empty tally with room for n detectors.
func acquireVotes(n int) []detectorVote {
	if held, ok := votesPool.Get().(*[]detectorVote); ok && cap(*held) >= n {
		return (*held)[:0]
	}

	return make([]detectorVote, 0, max(n, 8))
}

// releaseVotes returns a tally to the pool. Passing a slice that did not come
// from the pool is harmless — an appended-to tally may have been reallocated.
func releaseVotes(v []detectorVote) {
	if cap(v) == 0 {
		return
	}

	// Drop the reason strings so the pool does not pin them until reuse.
	clear(v[:cap(v)])

	votesPool.Put(&v)
}

// toSeries extracts the OHLCV columns from candles in one allocation. It is the
// unpooled form, for callers that keep the columns.
func toSeries(candles []market.Candle) series {
	n := len(candles)
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
	// trendFilterSell, when non-nil, replaces trendFilter for the SELL-veto
	// side only (built with TrendPeriodSell/TrendThresholdSell); the BUY-veto
	// side always uses trendFilter.
	trendGate       bool
	trendFilter     *trendDetector
	trendFilterSell *trendDetector

	// denyGate, when set, adds a short-term BUY-only veto checked after
	// everything else (trend gate included) has already let the buy through.
	// See config.StrategyConfig.DenyGate's doc comment for the two checks it
	// runs. SELL is never denied. <=0 on either period disables that specific
	// check while leaving the other active.
	denyGate                bool
	denyBreakoutPeriod      int
	denyBreakoutRVOLPeriod  int
	denyBreakoutRVOLThresh  float64
	denyReversalStochPeriod int
	denyReversalOverbought  float64

	// htf, when set, evaluates a detector set on coarser aggregated bars and
	// either vetoes counter-trend signals or votes alongside the base set.
	htf *higherTimeframe
}

// Name describes the combined strategy: detector sets and merge modes.
func (c *Combined) Name() string {
	modes := string(c.mode)
	if c.sellMode != c.mode {
		modes = fmt.Sprintf("buy:%s/sell:%s", c.mode, c.sellMode)
	}

	sets := detectorNames(c.detectors)
	if !c.sellSame {
		sets = fmt.Sprintf(
			"buy:%s/sell:%s",
			detectorNames(c.detectors),
			detectorNames(c.sellDetectors),
		)
	}

	return fmt.Sprintf("combined[%s|%s]", sets, modes)
}

// detectorNames joins detector names with commas.
func detectorNames(ds []Detector) string {
	names := make([]string, len(ds))
	for i, d := range ds {
		names[i] = d.Name()
	}

	return strings.Join(names, ",")
}

// WarmupBars reports the bars needed before the strategy gives stable signals.
func (c *Combined) WarmupBars() int { return c.warmup }

// detectorMaxLookback reports the most bars d can make use of in one
// evaluation: its own MaxLookback() when it implements that optional
// interface (a detector like trend or ema_cross that benefits from more
// history than the bare minimum to produce a value), otherwise just
// WarmupBars().
func detectorMaxLookback(d Detector) int {
	n := d.WarmupBars()
	if lb, ok := d.(interface{ MaxLookback() int }); ok {
		if m := lb.MaxLookback(); m > n {
			n = m
		}
	}

	return n
}

// MaxLookback reports the most bars any of the strategy's detectors (or the
// trend gate) can make use of in one evaluation — at least WarmupBars, but more
// for long-lookback detectors like trend. The backtester uses it to size its
// per-bar window so those detectors see as much history as they would live,
// instead of being silently capped to the (much smaller) warmup window.
func (c *Combined) MaxLookback() int {
	most := c.warmup
	consider := func(d Detector) {
		if n := detectorMaxLookback(d); n > most {
			most = n
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

	if c.trendFilterSell != nil {
		consider(c.trendFilterSell)
	}

	if c.denyGate {
		if n := c.denyBreakoutPeriod + 2; n > most {
			most = n
		}

		if n := c.denyReversalStochPeriod + 2; n > most {
			most = n
		}
	}

	// The confirmation timeframe reads its own detectors on aggregated bars, so
	// it needs its multiple times as much base history.
	if n := c.htf.BaseBars(); n > most {
		most = n
	}

	return most
}

// Evaluate runs the detector sets on the candles and merges their votes into
// a single BUY/SELL/HOLD signal for symbol.
//
//nolint:cyclop,funlen // the merge pipeline (warmup, tally, gate, format) is one flow
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

	// The columns live only for this call — every detector reads them and returns
	// scalars — so the buffer goes back to the pool on the way out.
	s := acquireSeries(candles)
	defer releaseSeries(s)

	// Higher-timeframe reading, computed once and used by whichever mode is
	// configured. An unavailable reading (still aggregating) gates nothing.
	htf := c.htf.vote(candles)

	// BUY side: its own detector set + combine mode.
	buyVotes, bBuyN, bSellN, bBuyStr, bSellStr := tallyDetectors(c.detectors, symbol, s, c.weights)

	// The tallies are read only by formatVotes and the trend gate, both of which
	// return strings, so they can go back to the pool when Evaluate returns.
	// addHTFVote may append and reallocate; releasing whatever buyVotes ends up
	// pointing at is correct either way.
	defer func() { releaseVotes(buyVotes) }()

	// In "vote" mode the higher timeframe is simply one more opinion in the tally.
	if htf.ok && c.htf != nil && c.htf.mode == htfVote {
		t := addHTFVote(
			tally{buyVotes, bBuyN, bSellN, bBuyStr, bSellStr},
			detectorVote{"htf", htf.action, htf.strength, htf.reason},
		)

		buyVotes, bBuyN, bSellN, bBuyStr, bSellStr = t.votes, t.buyN, t.sellN, t.buyStr, t.sellStr
	}

	buyOK, buyStrength := sideQualifies(c.mode, bBuyN, bSellN, bBuyStr, bSellStr)

	// SELL side: may use a different detector set (reuse the buy tally if same).
	sellVotes := buyVotes
	sBuyN, sSellN, sBuyStr, sSellStr := bBuyN, bSellN, bBuyStr, bSellStr

	if !c.sellSame && len(c.sellDetectors) > 0 {
		sellVotes, sBuyN, sSellN, sBuyStr, sSellStr = tallyDetectors(
			c.sellDetectors,
			symbol,
			s,
			c.weights,
		)

		// Only when the sell side has its own tally: otherwise sellVotes aliases
		// buyVotes and returning it twice would hand the same buffer to two
		// callers.
		defer func() { releaseVotes(sellVotes) }()

		// The buy side's tally above already picked up the HTF vote when it
		// aliases into sellVotes (c.sellSame, the common case) — but a split
		// detector set builds its own fresh tally that never saw it. Without
		// this, htf_mode: vote silently only ever influenced entries, never
		// exits, for any config with a distinct sell detector set (one line
		// away from any profile — e.g. "conservative" already uses a split
		// buy/sell mode). See CODE_REVIEW_FINDINGS.md's HTF-vote-dropped-
		// from-sell finding.
		if htf.ok && c.htf != nil && c.htf.mode == htfVote {
			t := addHTFVote(
				tally{sellVotes, sBuyN, sSellN, sBuyStr, sSellStr},
				detectorVote{"htf", htf.action, htf.strength, htf.reason},
			)

			sellVotes, sBuyN, sSellN, sBuyStr, sSellStr = t.votes, t.buyN, t.sellN, t.buyStr, t.sellStr
		}
	}

	sellOK, sellStrength := sideQualifies(
		c.sellMode,
		sSellN,
		sBuyN,
		sSellStr,
		sBuyStr,
	)

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

	// Higher-timeframe gate: don't trade against the larger move. Like the trend
	// gate, this only blocks — protective exits run in the engine before the
	// strategy, so a vetoed SELL can never trap a losing position.
	if htf.ok && c.htf != nil && c.htf.mode == htfGate && action != Hold {
		if (htf.action == Buy && action == Sell) || (htf.action == Sell && action == Buy) {
			sig.Action = Hold
			sig.Strength = 0
			sig.Reason = fmt.Sprintf("%s blocked by %s", action, htf.reason)

			return sig
		}
	}

	// Weekly-trend gate: veto counter-trend actions. An up-trend blocks SELLs, a
	// down-trend blocks BUYs; a flat trend gates nothing. Protective exits run in
	// the engine before the strategy, so a vetoed SELL never traps a losing
	// position. A SELL action uses trendFilterSell when configured (its own
	// period/threshold); everything else uses the shared trendFilter. Kept
	// inside the c.trendGate check (rather than computed unconditionally above
	// it) so a disabled gate costs nothing extra per evaluation.
	if c.trendGate && action != Hold {
		gateFilter, reuseVote := c.trendFilter, true
		if action == Sell && c.trendFilterSell != nil {
			gateFilter, reuseVote = c.trendFilterSell, false
		}

		if gateFilter != nil {
			if tdir, treason := c.trendVote(
				s,
				usedVotes,
				gateFilter,
				reuseVote,
			); (tdir == Buy && action == Sell) ||
				(tdir == Sell && action == Buy) {
				sig.Action = Hold
				sig.Strength = 0
				sig.Reason = fmt.Sprintf("%s blocked by %s", action, treason)
				return sig
			}
		}
	}

	// Short-term deny gate: BUY-only veto for a false breakout or an
	// already-overextended move, checked last so it only has to run on a
	// buy that survived every other gate. See config.StrategyConfig.DenyGate.
	if c.denyGate && action == Buy {
		if reason, deny := c.denyVote(s); deny {
			sig.Action = Hold
			sig.Strength = 0
			sig.Reason = fmt.Sprintf("%s denied: %s", action, reason)

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

// trendVote returns the trend direction (and reason) for the gate, reading
// filter. When reuse is true and the acting side's detector set already
// includes "trend", its tallied vote is reused instead — that vote was built
// from the same config as trendFilter, so re-running the long-lookback
// regression (up to a week of bars) would repeat identical work every
// evaluation. reuse must be false for trendFilterSell: its parameters can
// differ from the tallied "trend" detector's, so its vote must always be
// computed fresh.
func (c *Combined) trendVote(
	s series,
	votes []detectorVote,
	filter *trendDetector,
	reuse bool,
) (Action, string) {
	if reuse {
		for _, v := range votes {
			if v.name == "trend" {
				return v.action, v.reason
			}
		}
	}

	a, _, reason := filter.Detect(s)

	return a, reason
}

// tallyDetectors runs a detector set over the series and tallies its votes. Each
// detector's strength is scaled by its configured weight (default 1.0) before
// being summed; vote counts are unaffected by weight.
//
//nolint:revive // five results (votes + per-side counts/strengths) beat an ad-hoc struct
func tallyDetectors(
	dets []Detector,
	symbol string,
	s series,
	weights map[string]float64,
) (votes []detectorVote, buyN, sellN int, buyStr, sellStr float64) {
	votes = acquireVotes(len(dets))
	for _, d := range dets {
		a, str, reason := detect(d, symbol, s)

		votes = append(votes, detectorVote{d.Name(), a, str, reason})

		w := 1.0

		if wv, ok := weights[d.Name()]; ok {
			// config.StrategyConfig.Weights documents "a zero or negative
			// weight mutes a detector's strength contribution without
			// removing its vote" — a negative wv multiplying through str
			// would instead INVERT the contribution (a BUY detector's
			// positive strength becomes a negative addend to buyStr),
			// which can flip a combine decision rather than merely muting
			// one detector's voice in it. Clamping to 0 here is what
			// actually delivers the documented "mute" contract. See
			// CODE_REVIEW_FINDINGS.md's "negative detector weights invert
			// a vote instead of muting it".
			w = max(0, wv)
		}

		switch a {
		case Buy:
			buyN++

			buyStr += str * w

		case Sell:
			sellN++

			sellStr += str * w

		case Hold:
			// holds don't vote
		}
	}

	return votes, buyN, sellN, buyStr, sellStr
}

// sideQualifies reports whether a side (with sideN votes / sideStr summed
// strength) wins under the given mode against the opposing side, and the
// resulting 0..1-ish strength.
//
// Every mode normalizes its returned strength the same way — an average over
// the WINNING side's own contributing detectors, max(sideN, 1) — so a
// strength is comparable across modes: min_strength/min_strength_sell gate
// buy and sell strengths with the same numbers regardless of which mode
// either side uses, and Evaluate's own SELL-vs-BUY tie-break compares them
// directly. Weighted previously divided by total (every configured
// detector, including ones that abstained), which shrank its reported
// strength as more detectors were added regardless of decision quality, and
// made it read on a different scale than majority/unanimous for the same
// underlying conviction — see CODE_REVIEW_FINDINGS.md's AT-8 finding. The
// WIN/LOSE decision itself (net > 0 for weighted) is unchanged; only the
// reported magnitude is renormalized.
func sideQualifies(
	mode CombineMode,
	sideN, otherN int,
	sideStr, otherStr float64,
) (bool, float64) {
	switch mode {
	case Unanimous:
		return sideN > 0 && otherN == 0, sideStr / float64(max(sideN, 1))
	case Weighted:
		net := sideStr - otherStr

		return net > 0, net / float64(max(sideN, 1))

	case Majority: // falls out to the shared majority return; unknown modes too
	}

	return sideN > otherN, sideStr / float64(max(sideN, 1))
}

// detectorVote captures one detector's result for tallying and reporting.
type detectorVote struct {
	name     string
	action   Action
	strength float64
	reason   string
}

// formatVotes renders every detector's vote plus the merged outcome (a single
// detector keeps its own reason).
// It builds into one Builder rather than Sprintf-ing each vote into a []string
// and joining: this runs on every evaluation of every symbol, whatever the
// outcome, and the per-vote Sprintf plus the slice plus the Join made it the
// single largest source of allocation *count* in a backtest once the column
// buffers were pooled. One grown buffer replaces about a dozen small ones.
func formatVotes(votes []detectorVote, result Action, mode CombineMode) string {
	if len(votes) == 1 {
		return votes[0].reason
	}

	var (
		b       strings.Builder
		scratch [32]byte // stack space for the strength; never escapes
	)

	// name:ACTION(0.00) is ~22 bytes; sizing up front avoids regrowth.
	b.Grow(len(votes)*24 + 24)

	for i, v := range votes {
		if i > 0 {
			b.WriteString(", ")
		}

		b.WriteString(v.name)
		b.WriteByte(':')
		b.WriteString(string(v.action))
		b.WriteByte('(')

		b.Write(strconv.AppendFloat(scratch[:0], v.strength, 'f', 2, 64))
		b.WriteByte(')')
	}

	b.WriteString(" => ")
	b.WriteString(string(result))
	b.WriteString(" by ")
	b.WriteString(string(mode))

	return b.String()
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
		errlog.Recordf("strategy", "none of the configured detectors %v were recognised; falling back to a plain RSI strategy", names)

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
	var trendFilter, trendFilterSell *trendDetector

	if cfg.TrendGate {
		trendFilter = newTrendDetector(cfg)

		if cfg.TrendPeriodSell > 0 || cfg.TrendThresholdSell > 0 {
			period := cfg.TrendPeriodSell
			if period <= 0 {
				period = cfg.TrendPeriod
			}

			threshold := cfg.TrendThresholdSell
			if threshold <= 0 {
				threshold = cfg.TrendThreshold
			}

			trendFilterSell = newTrendDetectorParams(period, threshold)
		}
	}

	// Multi-timeframe confirmation. It needs its multiple times as many base
	// bars before it can say anything, so it raises the strategy's warmup —
	// gating on a higher timeframe that has not formed yet would either block
	// everything or confirm nothing.
	htf := newHigherTimeframe(cfg)
	if n := htf.BaseBars(); n > warmup {
		warmup = n
	}

	return &Combined{
		detectors: detectors, sellDetectors: sellDetectors, sellSame: sellSame,
		mode: mode, sellMode: sellMode, warmup: warmup + 2,
		minStrength: cfg.MinStrength, minStrengthSell: sellMin,
		trendGate: cfg.TrendGate, trendFilter: trendFilter, trendFilterSell: trendFilterSell,
		weights:                 normalizeWeights(cfg.Weights, cfg),
		htf:                     htf,
		denyGate:                cfg.DenyGate,
		denyBreakoutPeriod:      cfg.DenyBreakoutPeriod,
		denyBreakoutRVOLPeriod:  cfg.DenyBreakoutRVOLPeriod,
		denyBreakoutRVOLThresh:  cfg.DenyBreakoutRVOLThreshold,
		denyReversalStochPeriod: cfg.DenyReversalStochPeriod,
		denyReversalOverbought:  cfg.DenyReversalOverbought,
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
// warmup among them. A name that matches no registered detector (a typo, or
// a name from a newer/older build) is silently dropped rather than erroring
// — without this record, the strategy that actually runs can differ from
// what's configured with nothing in the backtest report or the live log to
// reveal it happened. See CODE_REVIEW_FINDINGS.md's typo'd-detector-names
// finding.
func buildDetectorList(names []string, cfg config.StrategyConfig) (dets []Detector, warmup int) {
	for _, n := range names {
		d := buildDetector(strings.ToLower(strings.TrimSpace(n)), cfg)
		if d == nil {
			errlog.Recordf("strategy", "unknown detector %q ignored (check spelling against the registered names)", n)

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
	trimmed := strings.ToLower(strings.TrimSpace(s))

	switch m := CombineMode(trimmed); m {
	case Majority, Unanimous, Weighted:
		return m
	default:
		if trimmed != "" {
			errlog.Recordf("strategy", "unknown combine mode %q ignored; using %q", s, def)
		}

		return def
	}
}

// clamp01 clamps v into [0, 1]. NaN maps to 0 (the safe "no conviction"
// value), not NaN itself: a NaN strength falls through both the < 0 and > 1
// comparisons below (NaN compares false against everything), and a NaN
// Signal.Strength then also fails NaN < gate in the conviction-gate check
// that's supposed to filter it out — a detector producing NaN (e.g. atr's
// 0/0 with atr_mult: 0, or a NaN volume reaching rvol) bypassed the gate
// entirely and reached order sizing. See CODE_REVIEW_FINDINGS.md's
// NaN-strength-bypasses-gate finding.
func clamp01(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}

	if v < 0 {
		return 0
	}

	if v > 1 {
		return 1
	}

	return v
}
