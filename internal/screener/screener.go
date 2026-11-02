// Package screener maintains the traded universe automatically.
//
// A fixed watchlist trades whatever was picked when it was written. Instruments
// go quiet, go illiquid, or get delisted; the strategy keeps voting on them
// anyway, and capital sits in symbols that no longer offer anything to trade.
// The screener re-derives the tradeable set from recent history on a schedule,
// applying the filters that decide whether an instrument is worth the risk of
// an order at all: price bounds, liquidity, and a volatility band — too quiet
// is untradeable after costs, too wild is unsurvivable.
package screener

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// History supplies the candles the screener judges instruments on. The engine's
// warmup history source satisfies it.
type History interface {
	// Candles fetches historical bars for symbol in [from, to] at res.
	Candles(
		ctx context.Context,
		symbol string,
		res market.Resolution,
		from, to time.Time,
	) ([]market.Candle, error)
}

// Verdict is one candidate's screening outcome, kept for both survivors and
// rejects so the UI can explain why a symbol is not being traded.
type Verdict struct {
	Symbol    string  `json:"symbol"`
	Passed    bool    `json:"passed"`
	Reason    string  `json:"reason"`
	Price     float64 `json:"price"`
	AvgVol    float64 `json:"avg_volume"`
	ATRPct    float64 `json:"atr_pct"`
	SpreadPct float64 `json:"spread_pct"` // estimated bid-ask spread (Corwin-Schultz), as a fraction of price
	Rank      float64 `json:"rank"`
	// Unmeasured is set when the candidate could not be judged at all — its
	// history fetch failed or returned nothing. That says something about the
	// provider, not the instrument; see engine.rescreen, which keeps such a
	// symbol on the watchlist rather than dropping it (SC2-3).
	Unmeasured bool `json:"unmeasured,omitempty"`
	// rankTier orders verdicts before Rank does: Rank is only comparable
	// within one tier. See rankTierOf.
	rankTier int
	Bars     int     `json:"bars"`
	Momentum float64 `json:"momentum_pct"`

	// AssetClass is what the candidate was judged AS — which profile's
	// resolution, filters and thresholds produced this verdict. Empty means
	// unclassified, screened against the base configuration.
	AssetClass string `json:"asset_class,omitempty"`
	// Resolution is the bar size it was judged on, spelled out because the
	// same symbol passes on daily bars and fails on minute bars.
	Resolution string `json:"resolution,omitempty"`
	// Sector is the label it was grouped under for max_symbols_per_sector.
	// Empty means unsectored, which exempts it — the same exemption the
	// portfolio guard's own sector caps make.
	Sector string `json:"sector,omitempty"`

	// TrendSlopePct/TrendR2 describe the fitted trend, MaxDrawdownPct the
	// candidate's own worst decline over the window, and CostMultiple how
	// many times its round-trip cost it typically moves in a bar.
	TrendSlopePct  float64 `json:"trend_slope_pct"`
	TrendR2        float64 `json:"trend_r2"`
	MaxDrawdownPct float64 `json:"max_drawdown_pct"`
	CostMultiple   float64 `json:"cost_multiple"`

	// Probe is the measured trading performance, when the probe stage ran for
	// this candidate. Nil means it was never probed — it ranked below the
	// cutoff, or probing is off — not that it scored badly.
	Probe *Probe `json:"probe,omitempty"`

	// candles are the bars this verdict was measured from, kept only for the
	// duration of the pass so the probe stage can backtest without re-fetching
	// history. Unexported: it is working state, not part of the API response,
	// and a few hundred candidates' bars have no business in a JSON payload.
	candles []market.Candle
}

// Result is one screening pass.
type Result struct {
	At       time.Time `json:"at"`
	Selected []string  `json:"selected"`
	Verdicts []Verdict `json:"verdicts"`
}

// atrPeriod is the lookback used for the volatility filter — the conventional
// 14 bars, matching the ATR the strategy's own detectors use.
const atrPeriod = 14

// momentumBars is the window the "momentum" ranking measures return over.
const momentumBars = 20

// Classifier reports an instrument's asset class ("etf", "stock", ...), so the
// screener can judge each candidate by the rules written for what it actually
// is. The engine satisfies it from the same classification cache the portfolio
// allocation caps use.
//
// An empty result means "unclassified", which is not an error: the candidate is
// screened against the base configuration instead of a class profile. Guessing
// a class would apply the wrong rules, which is worse than applying the general
// ones.
type Classifier interface {
	AssetClass(ctx context.Context, symbol string) string
}

// Sectorizer reports an instrument's sector label ("technology", "energy", ...),
// so the screener can stop filling every slot with one industry. The engine
// satisfies it from the same map the portfolio guard's sector caps read, which
// is the point: a screener that diversified on a different notion of "sector"
// than the guard enforces would just move the disagreement rather than fix it.
//
// An empty result means "unknown", which is not an error — an unsectored symbol
// (Yahoo reports none for funds) is simply not subject to the sector cap, the
// same exemption risk.PortfolioGuard.checkSector already makes.
type Sectorizer interface {
	Sector(ctx context.Context, symbol string) string
}

// Prober scores a shortlisted candidate by actually trading it — a backtest of
// the live strategy, or a bounded parameter search. It is the difference
// between "this instrument is tradeable" and "this instrument is worth
// trading", and it is expensive, so only the best few per class ever reach it.
//
// Implemented outside this package: a probe needs the backtester and the
// optimizer, and internal/autotune already imports the engine that imports
// this package. ok is false when the candidate could not be scored at all.
type Prober interface {
	Probe(ctx context.Context, symbol string, candles []market.Candle) (Probe, bool)
}

// Probe is one candidate's measured trading performance.
type Probe struct {
	// Source is "backtest" (the live strategy as configured) or "autotune"
	// (the best configuration a bounded search could find). They answer
	// different questions and must not be compared with each other: an
	// autotune score is what the symbol COULD do if tuned for, which is only
	// what you would actually earn if you then tune for it.
	Source string `json:"source"`

	Trades        int     `json:"trades"`
	ReturnPct     float64 `json:"return_pct"`
	WinRatePct    float64 `json:"win_rate_pct"`
	ExpectancyPct float64 `json:"expectancy_pct"` // a FRACTION per trade (0.008 = 0.8%)
	// Score is the probe's own ranking number, comparable only within one
	// Source.
	Score float64 `json:"score"`
}

// Screener evaluates a candidate universe against the configured filters.
type Screener struct {
	cfg      config.ScreenerConfig
	history  History
	res      market.Resolution
	classify Classifier
	sectors  Sectorizer
	prober   Prober
	costs    config.CostsConfig
	// perClassSectors counts MaxSymbolsPerSector within each asset class.
	perClassSectors bool
}

// Options carry the collaborators a screening pass can use but does not
// require. A Screener built with none of them behaves exactly as it did before
// per-class profiles existed: one set of rules, one universe, one cap.
type Options struct {
	// Classify assigns each candidate an asset class. Without it every
	// candidate is screened against the base configuration.
	Classify Classifier
	// Sectors assigns each candidate a sector label. Without it
	// max_symbols_per_sector does nothing, since there is nothing to group by.
	Sectors Sectorizer
	// Prober scores shortlisted candidates by backtesting them. Without it
	// screener.probe does nothing, whatever the config says.
	Prober Prober
	// Costs is the commission model the "cost" filter charges a round trip
	// at, alongside the estimated spread.
	Costs config.CostsConfig
	// SectorCapsPerAssetClass mirrors
	// config.PortfolioConfig.SectorCapsPerAssetClass so MaxSymbolsPerSector
	// is counted the same way the portfolio guard counts its own sector caps.
	// It is passed in rather than duplicated in ScreenerConfig because the
	// setting belongs to one limit with two enforcement points, and two copies
	// of it could disagree.
	SectorCapsPerAssetClass bool
}

// New builds a Screener. It returns nil when screening is disabled, so callers
// can treat "off" and "absent" the same way.
//
// res is the fallback bar size, used only for a class whose resolved profile
// names none — screener.resolution and each profile's own override take
// precedence, since the right granularity is a property of the instrument.
func New(cfg config.ScreenerConfig, h History, res market.Resolution, opt Options) *Screener {
	if !cfg.Enabled || h == nil {
		return nil
	}

	if cfg.Rank == "" {
		cfg.Rank = "volume"
	}

	if cfg.Interval <= 0 {
		cfg.Interval = 6 * time.Hour
	}

	return &Screener{
		cfg:      cfg,
		history:  h,
		res:      res,
		classify: opt.Classify,
		sectors:  opt.Sectors,
		prober:   opt.Prober,
		costs:    opt.Costs,

		perClassSectors: opt.SectorCapsPerAssetClass,
	}
}

// Interval is how often Screen should be re-run.
func (s *Screener) Interval() time.Duration {
	if s == nil {
		return 0
	}

	return s.cfg.Interval
}

// Inputs are the symbol sets one screening pass reconciles. They are a struct
// rather than three positional []string arguments because transposing two of
// them would compile cleanly and quietly change which symbols are protected.
type Inputs struct {
	// Current is the watchlist as it stands, and the candidate pool when no
	// universe is configured.
	Current []string
	// Held have an open position. Dropping one strands it.
	Held []string
	// Pinned were chosen by hand — the config watchlist or the API.
	Pinned []string
}

// Screen evaluates every candidate and returns the symbols that should be
// tracked, best-ranked first.
//
// Two kinds of symbol are protected from the filters and from the MaxSymbols
// cap, each behind its own switch.
//
// Held symbols (keep_held): dropping a symbol the bot still owns would leave the
// position untracked — no exits, no trailing stop, no way for the strategy to
// sell it. Whatever the filters say, you finish what you started.
//
// Pinned symbols (keep_manual): the operator chose these, and a ranking pass is
// not entitled to overrule that. With it set the screener only ever *extends*
// the watchlist, and MaxSymbols caps the discoveries alone.
//
// A protected symbol still gets a full verdict recorded, so the UI shows why it
// would otherwise have gone.
func (s *Screener) Screen(ctx context.Context, in Inputs) Result {
	if s == nil {
		return Result{}
	}

	out := Result{At: time.Now()}
	held := upperSet(in.Held)
	pinned := upperSet(in.Pinned)

	// Survivors are ranked and capped WITHIN their own class, never against
	// each other. That is the whole point of a per-class cap: ranking an ETF's
	// average volume against a megacap's decides the ETF never wins, and
	// "eight ETFs and fifteen stocks" silently becomes "twenty-three stocks".
	passed := make(map[string][]Verdict)

	for class, syms := range s.candidatesByClass(ctx, in.Current) {
		cfg := s.cfg.ForAssetClass(class)

		for _, sym := range syms {
			v := s.judge(ctx, cfg, class, sym)
			if !v.Passed {
				v = s.protect(v, held[sym], pinned[sym])
			}

			out.Verdicts = append(out.Verdicts, v)

			// A protected symbol is tracked but does not consume a MaxSymbols
			// slot, so it is appended after the cap rather than ranked against
			// the field.
			if v.Passed && !s.protected(held[sym], pinned[sym]) {
				passed[class] = append(passed[class], v)
			}
		}
	}

	// Probing is the expensive stage and runs only over what already survived,
	// best-first, so a large universe costs no more searches than a small one.
	s.probeAll(ctx, passed, out.Verdicts)

	out.Selected = s.selectPerClass(passed, s.heldSectors(out.Verdicts, held), held)

	// The bars were working state for the probe stage. publishVerdict
	// cleared them only on verdicts it probed, so every other passing
	// verdict — all of them with probing off — kept its whole series in the
	// Result the engine stores until the next pass: tens of MB held for six
	// hours for nothing (SC3-7).
	for i := range out.Verdicts {
		out.Verdicts[i].candles = nil
	}

	// Protected symbols are tracked whether or not they ranked, and whether or
	// not the cap had room.
	if s.cfg.KeepHeld {
		out.Selected = appendMissing(out.Selected, held)
	}

	if s.cfg.KeepManual {
		out.Selected = appendMissing(out.Selected, pinned)
	}

	return out
}

// heldSectors is the sector budget the held positions already spend, keyed
// like sectorBudgetKey. risk.PortfolioGuard's max_positions_per_sector counts
// held positions, and the screener's budget did not: with two tech names held
// and both caps at 2, the screener still picked two tech discoveries and the
// guard refused every buy of them — the blocked-trade stream
// max_symbols_per_sector exists to stop (SC3-6). Held symbols still never
// drop; they only count.
func (s *Screener) heldSectors(verdicts []Verdict, held map[string]bool) map[string]int {
	sectors := make(map[string]int)

	for _, v := range verdicts {
		if held[v.Symbol] && v.Sector != "" {
			sectors[s.sectorBudgetKey(v)]++
		}
	}

	return sectors
}

// selectPerClass ranks and caps each class independently, then concatenates
// them in a stable class order so two identical passes produce an identical
// list. sectors starts as what the held positions spend (see heldSectors).
func (s *Screener) selectPerClass(passed map[string][]Verdict, sectors map[string]int, held map[string]bool) []string {
	classes := make([]string, 0, len(passed))
	for class := range passed {
		classes = append(classes, class)
	}

	slices.Sort(classes)

	// One sector budget, keyed to match whatever the portfolio guard counts:
	// shared across classes by default, or per class when
	// portfolio.sector_caps_per_asset_class is on. The screener diversifying
	// on a different rule than the guard enforces would just move the
	// disagreement rather than fix it. Classes are walked in sorted order so
	// the same input always produces the same selection.
	out := make([]string, 0, len(classes))

	for _, class := range classes {
		cfg := s.cfg.ForAssetClass(class)
		out = append(out, s.rankAndCap(cfg, passed[class], sectors, held)...)
	}

	return out
}

// candidatesByClass resolves the pool to screen and groups it by asset class.
//
// Each class's pool is resolved separately (profile universe, then
// universe_by_asset_class, then the shared universe, then a suggested starter
// pool), because the whole point of screening by type is that the pools have
// nothing to do with each other. Anything that remains unresolved falls back to
// the current watchlist, which preserves the original filter-only behaviour for
// an installation that has never configured a universe.
func (s *Screener) candidatesByClass(ctx context.Context, current []string) map[string][]string {
	pool := s.pool(current)
	out := make(map[string][]string, len(pool))

	for _, sym := range pool {
		// Once per symbol. Both sides of the old one-liner called classOf,
		// which can be a fresh provider lookup each time; when the first
		// failed and the second succeeded, the unclassified bucket was
		// overwritten with the other class's symbols — those screened twice,
		// the unclassified ones never (SC2-4).
		c := s.classOf(ctx, sym)

		out[c] = append(out[c], sym)
	}

	return out
}

// seedableClasses lists every class a pass should assemble a pool for: the ones
// with a profile or a universe of their own, plus — when the shared switch is
// on, or when nothing is configured at all — every class that has a starter
// pool to offer.
//
// The second half is what serves an operator who wants the screener to find
// things but has no idea what to list: with suggest_universe on and no classes
// named, every class there is a pool for gets seeded.
func (s *Screener) seedableClasses() []string {
	out := s.cfg.AssetClassesConfigured()
	if !s.cfg.SuggestUniverse && len(out) > 0 {
		return out
	}

	seen := make(map[string]bool, len(out))
	for _, c := range out {
		seen[c] = true
	}

	for _, c := range SuggestedClasses() {
		if !seen[c] {
			out = append(out, c)
		}
	}

	return out
}

// classOf classifies one candidate, tolerating the absence of a classifier and
// an unclassifiable symbol alike — both mean "screen it against the base
// configuration".
func (s *Screener) classOf(ctx context.Context, symbol string) string {
	if s.classify == nil {
		return ""
	}

	return strings.ToLower(strings.TrimSpace(s.classify.AssetClass(ctx, symbol)))
}

// sectorOf labels one candidate, tolerating both the absence of a Sectorizer
// and an unsectored symbol — either way it is exempt from the sector cap.
//
// Resolved here, during screening, rather than read from a cache filled after
// adoption: a symbol the screener has never tracked has no sector yet, and a
// cap that only applies to symbols already on the watchlist would miss every
// candidate it exists to judge.
func (s *Screener) sectorOf(ctx context.Context, symbol string) string {
	if s.sectors == nil {
		return ""
	}

	return strings.ToLower(strings.TrimSpace(s.sectors.Sector(ctx, symbol)))
}

// pool assembles the full candidate set across every configured class, plus the
// shared universe, plus (when nothing at all is configured) the current
// watchlist.
func (s *Screener) pool(current []string) []string {
	seen := make(map[string]bool)

	var out []string

	add := func(syms []string) {
		for _, p := range syms {
			sym := strings.ToUpper(strings.TrimSpace(p))
			if sym == "" || seen[sym] {
				continue
			}

			seen[sym] = true

			out = append(out, sym)
		}
	}

	add(s.cfg.Universe)

	for _, class := range s.seedableClasses() {
		// A class with a pool written for it specifically contributes that
		// pool and nothing else: a curated list is a decision, and appending
		// suggestions to it would dilute rather than help.
		if s.cfg.HasOwnUniverse(class) {
			add(s.cfg.ForAssetClass(class).Universe)

			continue
		}

		// Otherwise seed it from the starter pool, if this class asked to be
		// seeded — per class, so you can take suggestions for the trackers
		// you know nothing about while screening exactly the five stocks you
		// already chose.
		if s.cfg.ForAssetClass(class).SuggestUniverse {
			add(SuggestedUniverse(class))
		}
	}

	// Nothing configured anywhere: screen what is already tracked. Filtering
	// only, never adding — the original behaviour.
	if len(out) == 0 {
		add(current)
	}

	slices.Sort(out)

	return out
}

// rankAndCap orders the survivors best-first and keeps at most MaxSymbols of
// them. Ties break on symbol so a pass is reproducible. Protected symbols are
// not in passed — they are appended afterwards, so they never spend a slot.
func (s *Screener) rankAndCap(
	cfg config.ScreenerConfig,
	passed []Verdict,
	sectors map[string]int,
	held map[string]bool,
) []string {
	sortByRank(passed)

	limit := len(passed)
	if cfg.MaxSymbols > 0 {
		limit = min(limit, cfg.MaxSymbols)
	}

	out := make([]string, 0, limit)

	for _, v := range passed {
		if len(out) >= limit {
			break
		}

		// A sector already at its cap is skipped rather than ending the walk:
		// the next candidate down may be in a different industry, and it is
		// exactly that substitution the cap exists to make. An unsectored
		// symbol is exempt, matching the guard.
		// A held symbol already counted in sectors (heldSectors) and is
		// never refused for its own sector.
		if cfg.MaxSymbolsPerSector > 0 && v.Sector != "" && !held[v.Symbol] {
			key := s.sectorBudgetKey(v)
			if sectors[key] >= cfg.MaxSymbolsPerSector {
				continue
			}

			sectors[key]++
		}

		out = append(out, v.Symbol)
	}

	return out
}

// appendMissing adds each symbol in add that is not already in out, in
// sorted order. Held and pinned overlap constantly — you tend to hold what
// you picked — so a symbol must not be listed twice.
//
// Sorted, not map iteration order: Go randomises map iteration, so ranging
// over add directly made the tail of Result.Selected (the protected
// held/pinned symbols rankAndCap never even sees) nondeterministic between
// otherwise-identical passes — contradicting rankAndCap's own "ties break on
// symbol so a pass is reproducible" guarantee one function up. See
// CODE_REVIEW_FINDINGS.md's Result.Selected-ordering finding.
func appendMissing(out []string, add map[string]bool) []string {
	missing := make([]string, 0, len(add))

	for sym := range add {
		if !slices.Contains(out, sym) {
			missing = append(missing, sym)
		}
	}

	slices.Sort(missing)

	return append(out, missing...)
}

// protected reports whether a symbol is exempt from the filters and the cap.
func (s *Screener) protected(held, pinned bool) bool {
	return (held && s.cfg.KeepHeld) || (pinned && s.cfg.KeepManual)
}

// protect passes a failed verdict that is protected, recording why it would
// otherwise have been dropped. The position takes precedence in the wording:
// it is the one with money at stake.
func (s *Screener) protect(v Verdict, held, pinned bool) Verdict {
	switch {
	case held && s.cfg.KeepHeld:
		v.Passed, v.Reason = true, "kept: position open ("+v.Reason+")"
	case pinned && s.cfg.KeepManual:
		v.Passed, v.Reason = true, "kept: on your watchlist ("+v.Reason+")"
	}

	return v
}

// upperSet indexes symbols case-insensitively, the form judge and candidates
// both work in.
func upperSet(symbols []string) map[string]bool {
	out := make(map[string]bool, len(symbols))
	for _, s := range symbols {
		out[strings.ToUpper(s)] = true
	}

	return out
}

// sectorBudgetKey is the slot a candidate spends from, matching how
// risk.PortfolioGuard.checkSector counts the same limit: the bare sector, or
// class+sector when the caps are counted per asset class.
func (s *Screener) sectorBudgetKey(v Verdict) string {
	if !s.perClassSectors {
		return v.Sector
	}

	return v.AssetClass + "/" + v.Sector
}

// sortByRank orders verdicts best-first: by tier, then by Rank within it. Ties
// break on symbol so a pass is reproducible.
func sortByRank(v []Verdict) {
	slices.SortStableFunc(v, func(a, b Verdict) int {
		if c := cmp.Compare(b.rankTier, a.rankTier); c != 0 {
			return c
		}

		if c := cmp.Compare(b.Rank, a.Rank); c != 0 {
			return c
		}

		return cmp.Compare(a.Symbol, b.Symbol)
	})
}

// judge screens one symbol through the configured filter chain, stopping at the
// first failure so the reason names the binding constraint rather than a list.
func (s *Screener) judge(
	ctx context.Context,
	cfg config.ScreenerConfig,
	class, symbol string,
) Verdict {
	res := s.resolutionFor(cfg)
	v := Verdict{
		Symbol: symbol, AssetClass: class,
		Resolution: string(res), Sector: s.sectorOf(ctx, symbol),
	}

	// The window comes from the class profile: daily bars over 30 days is two
	// dozen observations, which cannot support an ATR let alone a regression,
	// so an ETF profile asks for a year while a minute-bar profile asks for a
	// month.
	to := time.Now()
	from := to.Add(-time.Duration(cfg.LookbackDays) * 24 * time.Hour)

	candles, err := s.history.Candles(ctx, symbol, res, from, to)
	if err != nil {
		v.Reason = "history unavailable: " + err.Error()
		v.Unmeasured = true

		return v
	}

	if len(candles) == 0 {
		v.Reason = "history unavailable: the provider returned no bars"
		v.Unmeasured = true

		return v
	}

	// A history source may serve coarser bars than asked for — Yahoo falls
	// back to hourly or daily when it has too few at the interval, over the
	// fallback's whole range. Judged as the requested resolution, every
	// per-bar measure moved the way that flatters a thin listing: volume 4×
	// or ~26× per bar, a bar count that clears min_history_bars, ATR and
	// drawdown over a different window — so the very "too few intraday bars"
	// case the screener exists to refuse passed, labelled "15" (SC3-3,
	// MK3-4). Too few bars at the requested resolution IS the measurement.
	if gap, ok := market.FinestBarGap(candles); ok && gap > market.ResolutionDuration(res)*3/2 {
		v.Bars = len(candles)
		v.Reason = fmt.Sprintf("too few bars at resolution %s: the provider served %s bars instead",
			res, gap.Round(time.Minute))

		return v
	}

	cols := columns(candles)
	m := measure(cols)
	m.measureCost(cols, s.costs)

	v.Bars, v.Price, v.AvgVol = m.Bars, m.Price, m.AvgVol
	v.ATRPct, v.SpreadPct, v.Momentum = m.ATRPct, m.SpreadPct, m.Momentum
	v.TrendSlopePct, v.TrendR2 = m.TrendSlopePct, m.TrendR2
	v.MaxDrawdownPct, v.CostMultiple = m.MaxDrawdownPct, m.CostMultiple()

	for _, f := range chain(cfg) {
		if reason := f.Test(cfg, &m); reason != "" {
			v.Reason = reason
			return v
		}
	}

	v.Passed = true
	v.Reason = "passed"
	v.Rank = rankOf(cfg, v, m)

	// Kept so the probe stage can re-score without re-fetching history.
	v.candles = candles

	return v
}

// resolutionFor picks the bar size for a resolved profile, falling back to the
// screener's construction-time default when the profile names none.
func (s *Screener) resolutionFor(cfg config.ScreenerConfig) market.Resolution {
	if cfg.Resolution != "" {
		return market.Resolution(cfg.Resolution)
	}

	if s.res != "" {
		return s.res
	}

	return market.Res1Min
}

// rankOf scores a survivor for ordering under the configured ranking. m is
// the same Measures judge computed v from, passed alongside it so ranking
// can see the *Ok flags Verdict doesn't carry (they exist to distinguish
// "measured as low/flat" from "couldn't be measured", a distinction ranking
// needs but the API response doesn't).
func rankOf(cfg config.ScreenerConfig, v Verdict, m Measures) float64 {
	switch strings.ToLower(cfg.Rank) {
	case "volatility":
		return v.ATRPct
	case "trend":
		// Slope alone rewards a violent, unrepeatable spike; R² alone rewards
		// a clean line that goes nowhere. Their product is the thing actually
		// worth trading: a move that is both sizeable and orderly.
		if !m.TrendOK {
			return math.Inf(-1)
		}

		return m.TrendSlopePct * m.TrendR2
	case "cost":
		// How much room the instrument leaves above its own friction.
		return m.CostMultiple()
	case "momentum":
		// Undefined momentum (too little history — a freshly-listed symbol)
		// used to default to exactly 0, sorting ABOVE every symbol with
		// genuinely negative momentum instead of behind all of them, since
		// higher Rank sorts first. -Inf guarantees last place regardless of
		// how negative a real reading can be. See
		// CODE_REVIEW_FINDINGS.md's "undefined momentum ranks as exactly 0".
		if !m.MomentumOK {
			return math.Inf(-1)
		}

		return v.Momentum
	default: // "volume"
		return v.AvgVol
	}
}

// series holds the aligned OHLCV columns the indicators take.
type series struct {
	high, low, closes, volume []float64
}

// columns splits candles into the aligned series the indicators take, from one
// backing allocation.
func columns(candles []market.Candle) series {
	n := len(candles)
	buf := make([]float64, 4*n)
	s := series{
		high:   buf[0:n:n],
		low:    buf[n : 2*n : 2*n],
		closes: buf[2*n : 3*n : 3*n],
		volume: buf[3*n : 4*n : 4*n],
	}

	for i, c := range candles {
		s.high[i], s.low[i], s.closes[i], s.volume[i] = c.High, c.Low, c.Close, c.Volume
	}

	return s
}

// mean returns the arithmetic mean, or 0 for an empty series.
func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}

	var sum float64

	for _, x := range v {
		sum += x
	}

	return sum / float64(len(v))
}
