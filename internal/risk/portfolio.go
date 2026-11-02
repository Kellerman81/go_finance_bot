package risk

import (
	"fmt"
	"math"
	"strings"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
)

// defaultCorrelationLookback is the window correlations are measured over when
// none is configured. Short enough to describe the current regime, long enough
// that the estimate is not noise.
const defaultCorrelationLookback = 200

// minCorrelationSamples is the fewest overlapping return observations needed
// before a correlation is trusted. Below this, two symbols can correlate at 0.95
// by coincidence, and blocking a trade on that is worse than not checking.
const minCorrelationSamples = 30

// Returns supplies recent per-bar returns for a symbol, most recent last. Both
// the live engine (from its bar store) and the backtester (from its candle map)
// can provide this.
type Returns interface {
	// Returns gives up to n recent per-bar returns for symbol. ok is false when
	// there is not enough history to be useful.
	Returns(symbol string, n int) ([]float64, bool)
}

// PortfolioGuard caps concentration across the book: asset-class allocation,
// sector exposure and correlated positions.
//
// It answers two questions the per-order money limits cannot: not "is this
// order too big?" but "is this order the same bet I already have on?" and "does
// the book still look like the allocation I chose?". A nil guard allows
// everything, so callers need no special case.
type PortfolioGuard struct {
	cfg     config.PortfolioConfig
	sectors map[string]string
	classes map[string]string  // symbol -> canonical asset class
	caps    map[string]float64 // asset class -> max fraction of equity
	slots   map[string]int     // asset class -> positions its budget must split into

	// definedSlots is the total of every named class's slot count, and
	// cappedPct the total fraction of the book the named budgets claim.
	// Both are fixed by the config, so they are summed once here rather
	// than on every order. See MaxPositionValue for what they divide.
	definedSlots int
	cappedPct    float64

	// uncappedSlots is the divisor for classes that have slots but NO
	// budget of their own: they share whatever fraction the named budgets
	// left unclaimed. The unnamed-class remainder is added at call time,
	// since it depends on the global min_positions.
	uncappedSlots int

	rets Returns
}

// NewPortfolioGuard builds a guard. It returns nil when portfolio limits are
// disabled or nothing is actually configured to enforce — an enabled block with
// every cap at zero would cost work on every order and block nothing.
//
// classes maps symbol -> asset class ("stock", "etf", ...) for the allocation
// caps; it is the engine's merged view of what the data provider/broker
// classified plus whatever config.yaml's asset_classes: block overrode. A
// symbol missing from it counts against config.UnclassifiedAssetClass, which is
// capped only if the operator wrote that key — so an unnamed bucket stays
// exempt, and a named one binds like any other class.
//
//nolint:cyclop // one "anything to enforce?" term per cap family
func NewPortfolioGuard(
	cfg config.PortfolioConfig,
	sectors map[string]string,
	classes map[string]string,
	rets Returns,
) *PortfolioGuard {
	if !cfg.Enabled {
		return nil
	}

	caps := cfg.NormalizedAssetClassCaps()

	sectorCaps := cfg.MaxPerSector > 0 || cfg.MaxPositionsPerSector > 0
	corrCaps := cfg.MaxCorrelatedPositions > 0 && cfg.CorrelationThreshold > 0
	classCaps := len(caps) > 0
	slots := cfg.NormalizedAssetClassSlots()

	// Slots alone are something to enforce too: config.Validate calls slots
	// without a class budget a supported way to split the book, and without
	// this the guard came back nil and min_positions_per_asset_class was
	// silently ignored (RK2-3).
	if !sectorCaps && !corrCaps && !classCaps && len(slots) == 0 {
		return nil
	}

	var (
		defined   int
		uncapped  int
		cappedPct float64
	)

	for class, n := range slots {
		defined += n

		// A class with slots but no budget of its own does not divide a
		// share of the book — it takes its share OUT of whatever the named
		// budgets left over. Counted separately for that reason.
		if caps[class] <= 0 {
			uncapped += n
		}
	}

	for _, pct := range caps {
		cappedPct += pct
	}

	return &PortfolioGuard{
		cfg:           cfg,
		sectors:       NormalizeLabels(sectors),
		classes:       NormalizeClassLabels(classes),
		caps:          caps,
		slots:         slots,
		definedSlots:  defined,
		cappedPct:     cappedPct,
		uncappedSlots: uncapped,
		rets:          rets,
	}
}

// NormalizeLabels canonicalises a hand-written symbol -> label map once, at
// construction: symbols upper-cased (matching portfolio.Normalize and every
// other symbol key in the bot), labels lower-cased, blanks dropped. Without it
// a cap on "etf" would silently miss a symbol the provider classified as "ETF".
//
// Exported because the guard is not the only thing that has to bucket a
// symbol: internal/api's allocation panel displays the very caps this guard
// enforces, and it grew its own ad-hoc lookup (a bare strings.ToUpper on the
// symbol, the label used verbatim). That drifted — config.yaml's
// asset_classes: is hand-written, so "spy: ETF" bucketed as "etf" here and as
// class "ETF" there, where no cap is keyed, and the panel reported "no limit"
// on a class that was in fact capped. One exported function, called by both,
// is what keeps the panel and the limit it displays telling the same story.
func NormalizeLabels(in map[string]string) map[string]string {
	return normalizeLabelsWith(
		in,
		func(l string) string { return strings.ToLower(strings.TrimSpace(l)) },
	)
}

// NormalizeClassLabels is NormalizeLabels for an asset-class map: each label
// in canonical class form (config.CanonicalAssetClass), so a hand-written
// "ETFs" buckets with the classifier's "etf" rather than founding a class of
// its own that no cap binds (SC3-5).
func NormalizeClassLabels(in map[string]string) map[string]string {
	return normalizeLabelsWith(in, config.CanonicalAssetClass)
}

func normalizeLabelsWith(in map[string]string, canon func(string) string) map[string]string {
	out := make(map[string]string, len(in))

	for sym, label := range in {
		if l := canon(label); l != "" {
			out[strings.ToUpper(strings.TrimSpace(sym))] = l
		}
	}

	return out
}

// AllowEntry reports whether buying symbol is allowed, given what is already
// held and the market value the order would add. The reason explains a refusal
// and is recorded on the blocked trade.
//
// The two families of cap treat an existing position differently, deliberately:
//
//   - The asset-class allocation cap applies to EVERY buy, including topping up
//     a position already held. It is a budget, and a top-up spends the same
//     budget as a new name — "no more than 30% in ETFs" that stops applying the
//     moment you already own the ETF is not a limit at all.
//   - The sector and correlation caps skip a symbol already held. They ask "is
//     this the same bet I already have on?", and that decision was made when the
//     position was opened; refusing to top up while allowing the original is
//     incoherent.
//
// Exits are never gated at all — this is only ever consulted on the buy path.
func (g *PortfolioGuard) AllowEntry(
	symbol string,
	snap portfolio.Snapshot,
	addValue float64,
) (bool, string) {
	if g == nil {
		return true, ""
	}

	symbol = strings.ToUpper(strings.TrimSpace(symbol))

	if ok, reason := g.checkAssetClass(symbol, snap, addValue); !ok {
		return false, reason
	}

	if h, held := snap.Holding(symbol); held && h.Held() {
		return true, ""
	}

	if ok, reason := g.checkSector(symbol, snap, addValue); !ok {
		return false, reason
	}

	return g.checkCorrelation(symbol, snap)
}

// AssetClass returns the canonical class the guard has for symbol, or "" when
// it has none. Exposed so the API/UI can show which bucket a symbol counts
// against — an allocation cap that blocks an order is much easier to understand
// when you can see how the bot classified the position.
func (g *PortfolioGuard) AssetClass(symbol string) string {
	if g == nil {
		return ""
	}

	return g.classes[strings.ToUpper(strings.TrimSpace(symbol))]
}

// bucketOf is the allocation bucket a symbol counts against: its class, or the
// unclassified sentinel when nothing could name one.
//
// Both sides of the cap go through here — the candidate being judged and each
// holding being summed — because the comparison is only meaningful if they
// agree on what to call "no class". AssetClass above deliberately does not:
// callers asking what the bot knows about a symbol must still be able to tell
// "unclassified" from a class literally named that.
func (g *PortfolioGuard) bucketOf(symbol string) string {
	if class := g.classes[strings.ToUpper(strings.TrimSpace(symbol))]; class != "" {
		return class
	}

	return config.UnclassifiedAssetClass
}

// equityOf is the denominator every equity-relative cap in this file divides
// by. Some callers (and the backtester's early bars) fill Cash/Invested without
// deriving Equity; reconstructing it here rather than skipping the cap is what
// keeps the allocation enforced exactly while the book is being built.
func equityOf(snap portfolio.Snapshot) float64 {
	if snap.Equity > 0 {
		return snap.Equity
	}

	return snap.Cash + snap.Invested
}

// MaxPositionValue is the most a single position in symbol's class may be
// worth: the share of the investable book that one of its slots is entitled
// to. It reports false when nothing splits that class, in which case the
// position is bounded only by max_per_position and the other money limits.
//
// investable is what the book can actually deploy — equity less cash_reserve,
// capped at max_total_invested (see Manager.investableBase). Equity alone
// would promise slots the account cannot fund: dividing 1000 of equity into
// four 250 slots when only 950 may ever be invested leaves the fourth
// position permanently short. Dividing the investable pot instead means N
// slots really do fit. A slot is a CEILING, not a reservation — the
// cash_reserve rule still refuses any individual order the account has no
// cash for right now.
//
// minPositions is the global min_positions. Slot counts resolve in three
// tiers, finest first:
//
//   - the class's own min_positions_per_asset_class entry;
//   - otherwise the REMAINDER of min_positions once the named classes have
//     taken theirs (global 5 with {stock: 2, etf: 2} leaves 1 for anything
//     unnamed; global 4 leaves 0, i.e. no ceiling for unnamed classes);
//   - otherwise, when no per-class split is configured at all, the original
//     flat rule: one equal share of the investable pot per position,
//     class-agnostic. That tier is also what a nil guard answers, since
//     min_positions works whether or not portfolio limits are enabled.
//
// What a slot divides depends on whether the class has a budget:
//
//   - With max_pct_per_asset_class: its own budget, split n ways. n == 1 is
//     the whole budget, which checkAssetClass already enforces, so it adds
//     nothing and reports false.
//   - Without one: the leftover the named budgets did not claim
//     (1 - Σ caps), split across every slot that has to come out of it. Here
//     n == 1 IS a real ceiling — one position's share of the leftover — so
//     unlike above it is not waved through.
//
// Both forms sum to the investable pot rather than oversubscribing it: with
// caps 0.7/0.3 and 2 slots each, 2(0.7·I/2) + 2(0.3·I/2) = I exactly, and
// with no caps at all, 4(I/4) = I.
//
// It is a POSITION ceiling, not an order ceiling: the caller subtracts what is
// already held, so a scale-in ladder cannot walk one name up to the whole class
// budget one add at a time. That is the failure this setting exists for — a
// percentage cap alone is spent first-come-first-served, and the first buy large
// enough to fill it shuts the class for everything else.
//
// risk.Manager applies it (see buyCapRules), which is deliberate: it clamps the
// order down like max_order_value instead of refusing it like the caps in
// AllowEntry, and one enforcement point means the live engine, the scale-in
// ladder and the backtester cannot drift apart on it.
//
// rule names the tier that produced the slot, so a clamped or refused order
// says which setting to go and look at rather than naming a mechanism the
// operator cannot find in their config file.
func (g *PortfolioGuard) MaxPositionValue(
	symbol string,
	investable float64,
	minPositions int,
) (value float64, rule string, ok bool) {
	if investable <= 0 {
		return 0, "", false
	}

	// No per-class split configured (or no guard at all): the original flat
	// rule, one share of the pot per position regardless of class.
	if g == nil || len(g.slots) == 0 {
		if minPositions <= 1 {
			return 0, "", false
		}

		return investable / float64(minPositions), ruleMinPositions, true
	}

	class := g.bucketOf(symbol)

	n, named := g.slots[class]
	if !named {
		n = g.unnamedSlots(minPositions)
	}

	if n <= 0 {
		return 0, "", false
	}

	// A class with no entry of its own is bounded by what min_positions had
	// left over, so that is the setting to name for it.
	rule = ruleMinPositionsPerClass
	if !named {
		rule = ruleMinPositions
	}

	if limit, capped := g.caps[class]; capped {
		if n <= 1 {
			return 0, "", false // the whole budget; checkAssetClass already has this
		}

		return limit * investable / float64(n), rule, true
	}

	v, ok := g.leftoverSlot(investable, minPositions)

	return v, rule, ok
}

// The settings a position slot can come from, as they are spelled in the
// config file — these reach the operator in a blocked/clamped trade reason.
const (
	ruleMinPositions         = "min_positions"
	ruleMinPositionsPerClass = "min_positions_per_asset_class"
)

// unnamedSlots is the slot count for a class with no entry of its own: what
// the named classes left of the global min_positions. Caller holds nothing;
// this is pure.
func (g *PortfolioGuard) unnamedSlots(minPositions int) int {
	if minPositions <= g.definedSlots {
		return 0
	}

	return minPositions - g.definedSlots
}

// leftoverSlot is one position's share of the book the named budgets did not
// claim, for a class carrying slots but no max_pct_per_asset_class of its own.
//
// The unnamed remainder is added to the divisor so an unnamed class has a
// share reserved for it rather than taking one out of the named classes'.
// When that unnamed class turns out to have a budget after all, this
// over-reserves slightly — every slot comes out a little smaller than it
// strictly had to. That direction is the safe one, and the alternative is
// enumerating classes that by definition have not been named.
func (g *PortfolioGuard) leftoverSlot(investable float64, minPositions int) (float64, bool) {
	divisor := g.uncappedSlots + g.unnamedSlots(minPositions)
	if divisor <= 0 {
		return 0, false
	}

	leftover := 1 - g.cappedPct
	if leftover <= 0 {
		// The named budgets claim the whole book. An unbudgeted class has no
		// room at all — reported as a slot worth nothing rather than as "no
		// rule", so Authorize refuses the buy by name instead of silently
		// letting it through under some other cap.
		return 0, true
	}

	return leftover * investable / float64(divisor), true
}

// checkAssetClass enforces the per-asset-class share of the book. A symbol
// nothing could classify is judged against config.UnclassifiedAssetClass rather
// than waved through: whether that bucket binds is then the operator's choice
// (write the key or don't) instead of an accident of what a provider happened to
// know about a ticker.
//
// The denominator is total equity (cash plus the marked-to-market value of
// every holding), not invested capital. Equity is what "30% of the portfolio"
// means to the person writing it, and it is the only denominator that does not
// move as a result of the order being judged: buying converts cash into a
// holding, leaving equity unchanged, so the same order cannot be allowed and
// then retroactively over the line. Measured against invested capital instead,
// the very first buy would always be 100% of a book of one.
func (g *PortfolioGuard) checkAssetClass(
	symbol string,
	snap portfolio.Snapshot,
	addValue float64,
) (bool, string) {
	if len(g.caps) == 0 {
		return true, ""
	}

	class := g.bucketOf(symbol)

	limit, capped := g.caps[class]
	if !capped {
		return true, ""
	}

	equity := equityOf(snap)
	if equity <= 0 {
		// No book to take a percentage of. Nothing sensible to enforce, and
		// refusing every order on an empty account would be worse.
		return true, ""
	}

	var value float64

	for sym, h := range snap.Holdings {
		if !h.Held() { // dust is not a position (RK3-2)
			continue
		}

		if g.bucketOf(sym) == class {
			value += h.MarketValue
		}
	}

	after := value + addValue
	if after <= limit*equity {
		return true, ""
	}

	return false, fmt.Sprintf(
		"portfolio: %s would be %.1f%% of equity (%.2f of %.2f) after this order; "+
			"max_pct_per_asset_class[%s] is %.1f%%",
		class, after/equity*100, after, equity, class, limit*100)
}

// checkSector enforces the per-sector value and position-count caps.
func (g *PortfolioGuard) checkSector(
	symbol string,
	snap portfolio.Snapshot,
	addValue float64,
) (bool, string) {
	sector, known := g.sectors[symbol]
	if !known {
		// An unmapped symbol is not silently exempt from *everything* — the
		// correlation check still applies — but it has no sector to compare
		// against, so the sector caps cannot say anything about it.
		return true, ""
	}

	// With SectorCapsPerAssetClass the cap is counted within the candidate's
	// own class, so a tech ETF and a tech stock hold separate slots. Off (the
	// default) counts the sector across the whole book.
	perClass := g.cfg.SectorCapsPerAssetClass
	class := ""

	if perClass {
		class = g.classes[symbol]
	}

	var (
		value float64
		count int
	)

	for sym, h := range snap.Holdings {
		if !h.Held() { // dust is not a position (RK3-2)
			continue
		}

		key := strings.ToUpper(sym)
		if g.sectors[key] != sector {
			continue
		}

		if perClass && g.classes[key] != class {
			continue
		}

		value += h.MarketValue
		count++
	}

	bucket := sector
	if perClass {
		bucket = describeBucket(class, sector)
	}

	if g.cfg.MaxPerSector > 0 && value+addValue > g.cfg.MaxPerSector {
		return false, fmt.Sprintf(
			"portfolio: %s at %.2f + %.2f exceeds max_per_sector %.2f",
			bucket, value, addValue, g.cfg.MaxPerSector)
	}

	if g.cfg.MaxPositionsPerSector > 0 && count >= g.cfg.MaxPositionsPerSector {
		return false, fmt.Sprintf(
			"portfolio: already holding %d %s positions (max_positions_per_sector %d)",
			count, bucket, g.cfg.MaxPositionsPerSector)
	}

	return true, ""
}

// describeBucket names the class+sector slot a refusal was counted in, so a
// blocked order says "etf tech" rather than just "tech" and the operator can
// tell which of the two buckets filled up. An unclassified candidate keeps the
// bare sector name — there is no class to name.
func describeBucket(class, sector string) string {
	if class == "" {
		return sector + " (unclassified)"
	}

	return class + " " + sector
}

// checkCorrelation counts how many held symbols move with the candidate and
// blocks the entry when too many already do.
//
//nolint:cyclop // guard chain, then one pass over the holdings
func (g *PortfolioGuard) checkCorrelation(
	symbol string,
	snap portfolio.Snapshot,
) (bool, string) {
	if g.cfg.MaxCorrelatedPositions <= 0 || g.cfg.CorrelationThreshold <= 0 || g.rets == nil {
		return true, ""
	}

	lookback := g.cfg.CorrelationLookback
	if lookback <= 0 {
		lookback = defaultCorrelationLookback
	}

	candidate, ok := g.rets.Returns(symbol, lookback)
	if !ok {
		// Unknown correlation is not a reason to block: a brand-new symbol with
		// no history would be refused forever, which is a worse failure than
		// occasionally allowing a correlated entry.
		return true, ""
	}

	var (
		correlated []string
		worst      float64
	)

	for sym, h := range snap.Holdings {
		if !h.Held() || strings.EqualFold(sym, symbol) { // dust is not a position (RK3-2)
			continue
		}

		held, ok := g.rets.Returns(sym, lookback)
		if !ok {
			continue
		}

		c, ok := correlation(candidate, held)
		if !ok || c < g.cfg.CorrelationThreshold {
			continue
		}

		correlated = append(correlated, sym)

		if c > worst {
			worst = c
		}
	}

	if len(correlated) >= g.cfg.MaxCorrelatedPositions {
		return false, fmt.Sprintf(
			"portfolio: %s correlates >= %.2f with %d held positions (%s, peak %.2f); "+
				"max_correlated_positions %d",
			symbol, g.cfg.CorrelationThreshold, len(correlated),
			strings.Join(correlated, ","), worst, g.cfg.MaxCorrelatedPositions)
	}

	return true, ""
}

// correlation returns the Pearson correlation of two return series, aligned on
// their most recent overlapping observations. ok is false when there is too
// little overlap or either series is flat (a constant series has no correlation,
// and dividing by its zero deviation would produce a confident NaN).
func correlation(a, b []float64) (float64, bool) {
	n := min(len(a), len(b))
	if n < minCorrelationSamples {
		return 0, false
	}

	// Align on the most recent n observations.
	a, b = a[len(a)-n:], b[len(b)-n:]

	var meanA, meanB float64

	for i := range n {
		meanA += a[i]
		meanB += b[i]
	}

	meanA /= float64(n)
	meanB /= float64(n)

	var cov, varA, varB float64

	for i := range n {
		da, db := a[i]-meanA, b[i]-meanB

		cov += da * db
		varA += da * da
		varB += db * db
	}

	if varA <= 0 || varB <= 0 {
		return 0, false
	}

	c := cov / math.Sqrt(varA*varB)
	if math.IsNaN(c) || math.IsInf(c, 0) {
		return 0, false
	}

	return c, true
}

// BarReturns converts a close series into per-bar fractional returns, the input
// the correlation check expects.
func BarReturns(closes []float64) []float64 {
	if len(closes) < 2 {
		return nil
	}

	out := make([]float64, 0, len(closes)-1)

	for i := 1; i < len(closes); i++ {
		if closes[i-1] > 0 {
			out = append(out, closes[i]/closes[i-1]-1)
		}
	}

	return out
}
