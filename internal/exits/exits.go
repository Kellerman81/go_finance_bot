// Package exits computes protective exit levels for an open position.
//
// It exists so the live engine and the backtester cannot disagree. Both used to
// carry their own copy of "is this position stopped out?", which is exactly the
// kind of duplication that makes a backtest quietly stop describing the bot: one
// side gains an ATR-based stop or a profit ladder and the other does not, and
// the reported edge becomes fiction. Both now call Compute and only differ in
// what they compare the levels against — a live tick, or a bar's high and low.
package exits

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// defaultATRPeriod matches the conventional 14-bar ATR the detectors use.
const defaultATRPeriod = 14

// Levels are the absolute prices at which a position exits. A zero level means
// that rule is disabled for this position.
type Levels struct {
	// Stop is the stop-loss price (below entry).
	Stop float64
	// Trail is the trailing-stop price (below the peak since entry).
	Trail float64
	// Target is the take-profit price (above entry), including any ROI-ladder
	// target that is currently in force.
	Target float64
	// TargetReason names which rule set Target, since the ROI ladder and the
	// fixed take-profit produce very different-looking exits.
	TargetReason string
	// TimeExit is non-empty when the position has been held past max_hold. It is
	// a barrier in time rather than in price, so it has no level to compare
	// against — once set, the position leaves at whatever the market offers.
	TimeExit string
}

// ATRPeriod returns the lookback to compute ATR with for these settings.
func ATRPeriod(cfg config.ExitsConfig) int {
	if cfg.ATRPeriod > 0 {
		return cfg.ATRPeriod
	}

	return defaultATRPeriod
}

// UsesATR reports whether any volatility-adaptive stop is configured, so callers
// can skip computing an ATR they will not use.
func UsesATR(cfg config.ExitsConfig) bool {
	return cfg.ATRStopMult > 0 || cfg.ATRTrailMult > 0
}

// Compute returns the exit levels for a position with the given entry price and
// peak-since-entry, held for the given duration. atr is the current Average
// True Range in price units (pass 0 when unknown — the ATR rules then fall back
// to the percentage ones).
//
// Where both a percentage and an ATR multiple are configured, the *wider* stop
// wins: the percentage acts as a floor so a momentarily tiny ATR cannot place
// the stop on top of the current price and guarantee an immediate exit.
func Compute(cfg config.ExitsConfig, entry, peak, atr float64, held time.Duration) Levels {
	var lv Levels

	// max_hold is a time barrier with no price level to compare against, so it
	// is the one rule that needs no entry price — it must not be disabled by
	// the same guard that protects the price-based rules below, or a position
	// whose entry price is momentarily unknown (0) never ages out. See
	// CODE_REVIEW_FINDINGS.md's "entry <= 0 disables every exit rule
	// including max_hold".
	if cfg.MaxHold > 0 && held >= cfg.MaxHold {
		lv.TimeExit = fmt.Sprintf("max hold %s reached", cfg.MaxHold)
	}

	if entry <= 0 {
		return lv
	}

	if peak < entry {
		peak = entry
	}

	lv.Stop = stopLevel(cfg, entry, atr)
	lv.Trail = trailLevel(cfg, entry, peak, atr)
	lv.Target, lv.TargetReason = targetLevel(cfg, entry, held)

	return lv
}

// WithTrailFloor raises the trailing stop to floor when floor is above it. See
// RebaseOnAdd for where a floor comes from; 0 leaves the levels unchanged.
//
// The floor is part of the trailing stop, so it lapses with it: an operator
// who sets every trailing distance to 0 has switched the trail off, and a
// floor left over from an earlier add must not keep firing for exactly the
// positions that had one.
func (lv Levels) WithTrailFloor(cfg config.ExitsConfig, floor float64) Levels {
	if !TrailConfigured(cfg) {
		return lv
	}

	if floor > lv.Trail {
		lv.Trail = floor
	}

	return lv
}

// TrailKey fingerprints the settings a trail level depends on, so a floor can
// tell whether it was computed under the settings now in force.
func TrailKey(cfg config.ExitsConfig) string {
	return fmt.Sprintf("%g/%g/%g/%t/%g", cfg.TrailingStopPct, cfg.TrailingTightPct,
		cfg.TrailingOffsetPct, cfg.TrailingOnlyAfterOffset, cfg.ATRTrailMult)
}

// FloorFor is floor while the trailing settings are the ones it was set under
// (key, from TrailKey), and 0 once they have changed. The floor is the
// pre-add trail at the settings OF THE ADD; kept after a change it froze them
// for exactly the positions that were ever scaled into — a trail widened from
// 3% to 10% applied to every position but those (EX3-5). An empty key (a
// floor recorded before keys existed) keeps the floor.
func FloorFor(cfg config.ExitsConfig, floor float64, key string) float64 {
	if key != "" && key != TrailKey(cfg) {
		return 0
	}

	return floor
}

// TrailConfigured reports whether any trailing-stop distance is set.
func TrailConfigured(cfg config.ExitsConfig) bool {
	return cfg.TrailingStopPct > 0 || cfg.TrailingTightPct > 0 || cfg.ATRTrailMult > 0
}

// RebaseOnAdd returns the peak and trailing-stop floor a position carries
// after it is added to — a scale-in on the way down or a pyramid on the way up
// — so the live engine and the backtester treat an add identically.
//
// The trail judges its stage from peak/entry, and an add moves the entry (the
// average cost) under a peak that was reached against the old one. Left alone
// that misreads the position (EX2-1): averaging down from 100 to 97.5 under a
// 104.5 peak turned a 4.5% gain into a 7.2% one, armed the tight stage, and put
// the trail at 102.41 — above the 95 price, so the whole enlarged position was
// sold at a loss on the very next check. Averaging UP did the opposite and
// loosened a tight trail from 107.80 to 99, or removed it outright under
// trailing_only_after_offset: a trailing stop that moves down.
//
// So an add re-bases the peak to where the new position actually stands —
// the higher of the new average and the add price — and keeps the trail level
// in force just before the add as a floor. The trail then ratchets up from the
// new basis and never sits below where it already was. floor is the
// position's existing floor (0 if none).
func RebaseOnAdd(
	cfg config.ExitsConfig,
	oldEntry, peak, floor, atr, newEntry, addPrice float64,
) (newPeak, newFloor float64) {
	before := Compute(cfg, oldEntry, peak, atr, 0).WithTrailFloor(cfg, floor).Trail

	return max(newEntry, addPrice), max(floor, before)
}

// stopLevel returns the stop-loss price: the wider of the percentage and ATR
// stops, so the percentage acts as a floor.
func stopLevel(cfg config.ExitsConfig, entry, atr float64) float64 {
	var stop float64

	if cfg.StopLossPct > 0 {
		stop = entry * (1 - cfg.StopLossPct)
	}

	return widerATRLevel(stop, entry, cfg.ATRStopMult, atr)
}

// minLevelFrac is where an ATR-only stop or trail goes when entry − mult·ATR
// is not positive: it used to be dropped, leaving an ATR-only position with
// no stop at all (EX3-4). With a percentage set, that one stays in force
// instead — deliberately NOT the 1% clamp: exits read the CURRENT ATR, so a
// crash bar on a HELD position would otherwise move its stop to 1% of entry,
// which is no stop. The entry side of EX3-4 (a volatile entry sized as if
// the narrow percentage stop applied) is closed in risk.SizeBuy, which
// refuses such an entry (ATROutOfRange).
const minLevelFrac = 0.01

// ATROutOfRange reports whether mult·ATR reaches the entry price, so the ATR
// stop cannot be placed above zero and the position's risk cannot be measured
// against it.
func ATROutOfRange(cfg config.ExitsConfig, entry, atr float64) bool {
	return cfg.ATRStopMult > 0 && atr > 0 && entry-cfg.ATRStopMult*atr <= 0
}

// trailLevel returns the trailing-stop price, or 0 when the position has not
// risen above its entry — until then a trailing stop would only duplicate or
// undercut the stop-loss.
//
// The trail has two stages. Below trailing_offset_pct it uses the ordinary
// (loose) distance, or nothing at all when trailing_only_after_offset is set;
// at or above the offset it switches to trailing_tight_pct. One distance cannot
// be both loose enough early that ordinary noise doesn't end a trade before it
// works and tight enough late that a winner isn't handed back — so it is two.
func trailLevel(cfg config.ExitsConfig, entry, peak, atr float64) float64 {
	if peak <= entry {
		return 0
	}

	pct, armed := trailPct(cfg, peak/entry-1)
	if !armed {
		return 0
	}

	var trail float64

	if pct > 0 {
		trail = peak * (1 - pct)
	}

	// Wider wins in BOTH stages, deliberately: past the offset the effective
	// distance is max(trailing_tight_pct, atr_trail_mult × ATR), so on a
	// volatile instrument the ATR trail overrides the tight stage. A fixed 2%
	// trail inside a 3-ATR noise band is the noise exit the ATR multiple was
	// configured to prevent. Documented (README, config.example.yaml) rather
	// than silent, and pinned by TestATRTrailBoundsTheTightStage (EX2-4).
	return widerATRLevel(trail, peak, cfg.ATRTrailMult, atr)
}

// widerATRLevel is level widened to anchor − mult·ATR when that is lower and
// still positive; with no level of its own and an ATR level that cannot sit
// above zero, it is minLevelFrac of the anchor (see there).
func widerATRLevel(level, anchor, mult, atr float64) float64 {
	if mult <= 0 || atr <= 0 {
		return level
	}

	s := anchor - mult*atr

	switch {
	case s > 0 && (level == 0 || s < level):
		return s
	case s <= 0 && level == 0:
		return anchor * minLevelFrac
	}

	return level
}

// trailPct returns the trailing distance for a position whose peak is gain above
// entry, and whether any trail applies at all.
func trailPct(cfg config.ExitsConfig, gain float64) (float64, bool) {
	past := cfg.TrailingOffsetPct > 0 && gain >= cfg.TrailingOffsetPct

	if past && cfg.TrailingTightPct > 0 {
		return cfg.TrailingTightPct, true
	}

	// Below the offset the trail is either the loose one, or suppressed so that
	// only the stop-loss is in force.
	if !past && cfg.TrailingOnlyAfterOffset && cfg.TrailingOffsetPct > 0 {
		return 0, false
	}

	return cfg.TrailingStopPct, true
}

// targetLevel returns the take-profit price and which rule set it: the fixed
// take-profit, the ROI ladder at the position's current age, or whichever of
// the two is nearer.
func targetLevel(cfg config.ExitsConfig, entry float64, held time.Duration) (float64, string) {
	var (
		target float64
		reason string
	)

	if cfg.TakeProfitPct > 0 {
		target = entry * (1 + cfg.TakeProfitPct)
		reason = fmt.Sprintf("take-profit +%.1f%%", cfg.TakeProfitPct*100)
	}

	if roi, label, ok := roiThreshold(cfg.ROI, held); ok {
		if t := entry * (1 + roi); target == 0 || t < target {
			target = t
			reason = fmt.Sprintf("roi %s +%.1f%%", label, roi*100)
		}
	}

	return target, reason
}

// roiThreshold returns the profit fraction required at the given position age:
// the entry with the largest key that is not in the future. Keys are Go
// durations ("0s", "30m", "2h").
func roiThreshold(roi map[string]float64, held time.Duration) (float64, string, bool) {
	if len(roi) == 0 {
		return 0, "", false
	}

	type step struct {
		after time.Duration
		label string
		value float64
	}

	steps := make([]step, 0, len(roi))

	for k, v := range roi {
		d, err := time.ParseDuration(strings.TrimSpace(k))
		if err != nil {
			continue // an unparseable key is ignored rather than crashing a live bot
		}

		steps = append(steps, step{after: d, label: k, value: v})
	}

	if len(steps) == 0 {
		return 0, "", false
	}

	slices.SortFunc(steps, func(a, b step) int { return cmp.Compare(a.after, b.after) })

	best, found := step{}, false

	for _, s := range steps {
		if s.after <= held {
			best, found = s, true
		}
	}

	if !found {
		return 0, "", false
	}

	return best.value, best.label, true
}

// Triggered reports which rule (if any) a current price breaches, and a
// human-readable reason. It is the live-engine view: one price, checked as it
// moves. Downside rules are tested first — if both a stop and a target are in
// range, the loss is the one that must not be missed.
func (lv Levels) Triggered(price float64) string {
	switch {
	// The trail first when it sits above the stop: it is the level a falling
	// price reached, and naming the deeper stop mislabelled a trailing exit
	// on a gap (EX3-2).
	case lv.Trail > lv.Stop && price <= lv.Trail:
		return fmt.Sprintf("trailing-stop %.2f<=%.2f", price, lv.Trail)
	case lv.Stop > 0 && price <= lv.Stop:
		return fmt.Sprintf("stop-loss %.2f<=%.2f", price, lv.Stop)
	case lv.Trail > 0 && price <= lv.Trail:
		return fmt.Sprintf("trailing-stop %.2f<=%.2f", price, lv.Trail)
	case lv.Target > 0 && price >= lv.Target:
		return fmt.Sprintf("%s %.2f>=%.2f", lv.TargetReason, price, lv.Target)
	case lv.TimeExit != "":
		// Last: if a price barrier was also breached it names the real cause and
		// (in a backtest) fills at that barrier rather than the bar's close.
		return lv.TimeExit
	}

	return ""
}

// ----- scale-out ladder -----

// Partial is the scale-out instruction for a position: sell Fraction of the
// ORIGINAL quantity because rung Rung (0-based) has been reached.
type Partial struct {
	// Rung is the index of the highest ladder step now satisfied.
	Rung int
	// Fraction is the share of the original quantity to sell, summed over every
	// rung that became due since the last one taken. It is capped at 1.
	Fraction float64
	// Reason describes the trigger for the trade log.
	Reason string
}

// NextPartial reports whether a scale-out is due for a position currently up
// gain (as a fraction of entry) that has already taken rungs `taken` steps.
//
// Rungs are evaluated in ascending profit order and each fires at most once. A
// price that jumps several rungs in one move takes all of them at once rather
// than dribbling one out per evaluation, so a gap up does not leave the ladder
// permanently behind the price.
func NextPartial(cfg config.ExitsConfig, gain float64, taken int) (Partial, bool) {
	rungs := sortedRungs(cfg.PartialTakeProfit)
	if taken >= len(rungs) {
		return Partial{}, false
	}

	var (
		p   Partial
		hit bool
	)

	for i := taken; i < len(rungs); i++ {
		if gain < rungs[i].ProfitPct {
			break
		}

		p.Rung = i

		p.Fraction += rungs[i].SellPct

		hit = true
	}

	if !hit || p.Fraction <= 0 {
		return Partial{}, false
	}

	p.Fraction = min(p.Fraction, 1)
	p.Reason = fmt.Sprintf("partial take-profit rung %d (+%.1f%%, %.0f%% of position)",
		p.Rung+1, rungs[p.Rung].ProfitPct*100, p.Fraction*100)

	return p, true
}

// sortedRungs returns the usable ladder steps in ascending profit order, so the
// ladder means the same thing however it was written in the config file.
func sortedRungs(in []config.PartialExit) []config.PartialExit {
	out := make([]config.PartialExit, 0, len(in))

	for _, r := range in {
		if r.ProfitPct > 0 && r.SellPct > 0 {
			out = append(out, r)
		}
	}

	slices.SortStableFunc(out, func(a, b config.PartialExit) int {
		return cmp.Compare(a.ProfitPct, b.ProfitPct)
	})

	return out
}

// PartialRungs returns how many usable rungs the ladder has, so callers can tell
// "fully scaled out" from "no ladder configured".
func PartialRungs(cfg config.ExitsConfig) int { return len(sortedRungs(cfg.PartialTakeProfit)) }

// RungProfitPct returns the profit threshold of ladder rung i, indexed exactly
// as Partial.Rung indexes it — over the SORTED, FILTERED ladder, not the raw
// config slice. ok is false for an index outside the usable ladder.
//
// It exists so a caller that needs a rung's price level (internal/backtest,
// which fills a scale-out at the rung's own threshold rather than at the bar's
// high) reads it from the same list NextPartial counted. Re-deriving the order
// from cfg.PartialTakeProfit looked equivalent but silently was not:
// sortedRungs DROPS unusable rungs (profit_pct <= 0, sell_pct outside (0,1]),
// so a single bad entry shifted every index and the backtest filled each
// tranche at the wrong level. config.ExitsConfig.Validate rejects such a
// ladder, but backtest.LoadConfig deliberately runs a config that failed
// validation ("running it ANYWAY, exactly as invalid as reported"), so the
// mismatch was reachable. See CODE_REVIEW_FINDINGS.md's BT-2 finding.
func RungProfitPct(cfg config.ExitsConfig, i int) (float64, bool) {
	rungs := sortedRungs(cfg.PartialTakeProfit)
	if i < 0 || i >= len(rungs) {
		return 0, false
	}

	return rungs[i].ProfitPct, true
}
