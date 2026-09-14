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

// stopLevel returns the stop-loss price: the wider of the percentage and ATR
// stops, so the percentage acts as a floor.
func stopLevel(cfg config.ExitsConfig, entry, atr float64) float64 {
	var stop float64

	if cfg.StopLossPct > 0 {
		stop = entry * (1 - cfg.StopLossPct)
	}

	if cfg.ATRStopMult > 0 && atr > 0 {
		if s := entry - cfg.ATRStopMult*atr; s > 0 && (stop == 0 || s < stop) {
			stop = s
		}
	}

	return stop
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

	if cfg.ATRTrailMult > 0 && atr > 0 {
		if s := peak - cfg.ATRTrailMult*atr; s > 0 && (trail == 0 || s < trail) {
			trail = s
		}
	}

	return trail
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
