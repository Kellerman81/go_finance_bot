package exits

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// close reports whether two prices are equal to the cent.
func closeTo(a, b float64) bool { return math.Abs(a-b) < 0.005 }

// TestPercentLevels checks the classic fixed-percentage rules.
func TestPercentLevels(t *testing.T) {
	t.Parallel()

	cfg := config.ExitsConfig{
		Enabled: true, StopLossPct: 0.05, TrailingStopPct: 0.08, TakeProfitPct: 0.20,
	}

	lv := Compute(cfg, 100, 120, 0, 0)

	if !closeTo(lv.Stop, 95) {
		t.Errorf("stop = %v, want 95", lv.Stop)
	}

	if !closeTo(lv.Trail, 110.4) {
		t.Errorf("trail = %v, want 110.40 (8%% below the 120 peak)", lv.Trail)
	}

	if !closeTo(lv.Target, 120) {
		t.Errorf("target = %v, want 120", lv.Target)
	}
}

// TestTrailInactiveBeforeProfit checks the trailing stop only engages once the
// position has risen — otherwise it would duplicate or undercut the stop-loss.
func TestTrailInactiveBeforeProfit(t *testing.T) {
	t.Parallel()

	cfg := config.ExitsConfig{Enabled: true, StopLossPct: 0.05, TrailingStopPct: 0.02}

	if lv := Compute(cfg, 100, 100, 0, 0); lv.Trail != 0 {
		t.Errorf("trail = %v at entry, want 0 (nothing to trail yet)", lv.Trail)
	}

	if lv := Compute(cfg, 100, 110, 0, 0); !closeTo(lv.Trail, 107.8) {
		t.Errorf("trail = %v after a rise to 110, want 107.80", lv.Trail)
	}
}

// TestATRStopWidensWithVolatility checks the point of ATR stops: the same
// configuration places a wider stop on a more volatile instrument.
func TestATRStopWidensWithVolatility(t *testing.T) {
	t.Parallel()

	cfg := config.ExitsConfig{Enabled: true, ATRStopMult: 2}

	calm := Compute(cfg, 100, 100, 0.5, 0) // ATR 0.5% of price
	wild := Compute(cfg, 100, 100, 4.0, 0) // ATR 4% of price

	if !closeTo(calm.Stop, 99) {
		t.Errorf("calm stop = %v, want 99 (2 x 0.5 ATR)", calm.Stop)
	}

	if !closeTo(wild.Stop, 92) {
		t.Errorf("wild stop = %v, want 92 (2 x 4.0 ATR)", wild.Stop)
	}

	if wild.Stop >= calm.Stop {
		t.Error("the volatile instrument did not get the wider stop")
	}
}

// TestPercentActsAsFloorForATRStop checks the wider of the two stops wins, so a
// momentarily tiny ATR cannot place the stop on top of the entry price.
func TestPercentActsAsFloorForATRStop(t *testing.T) {
	t.Parallel()

	cfg := config.ExitsConfig{Enabled: true, StopLossPct: 0.10, ATRStopMult: 2}

	// ATR 0.1 => an ATR stop at 99.8, far tighter than the 10% stop at 90.
	if lv := Compute(cfg, 100, 100, 0.1, 0); !closeTo(lv.Stop, 90) {
		t.Errorf("stop = %v, want 90: the percentage stop is the wider one", lv.Stop)
	}

	// ATR 8 => an ATR stop at 84, wider than the percentage stop.
	if lv := Compute(cfg, 100, 100, 8, 0); !closeTo(lv.Stop, 84) {
		t.Errorf("stop = %v, want 84: the ATR stop is the wider one", lv.Stop)
	}
}

// TestATRFallsBackWithoutVolatility checks an unknown ATR leaves the percentage
// rules in charge rather than disabling the stop.
func TestATRFallsBackWithoutVolatility(t *testing.T) {
	t.Parallel()

	cfg := config.ExitsConfig{Enabled: true, StopLossPct: 0.05, ATRStopMult: 3}

	if lv := Compute(cfg, 100, 100, 0, 0); !closeTo(lv.Stop, 95) {
		t.Errorf("stop = %v with no ATR available, want the 95 percentage stop", lv.Stop)
	}
}

// TestROILadderDecays checks the profit target falls as a position ages: demand
// a lot early, settle for less once the trade has gone stale.
func TestROILadderDecays(t *testing.T) {
	t.Parallel()

	cfg := config.ExitsConfig{Enabled: true, ROI: map[string]float64{
		"0s": 0.10, "30m": 0.05, "2h": 0.02, "6h": 0,
	}}

	cases := []struct {
		held time.Duration
		want float64
	}{
		{0, 110},
		{10 * time.Minute, 110},
		{45 * time.Minute, 105},
		{3 * time.Hour, 102},
		{8 * time.Hour, 100},
	}

	for _, tc := range cases {
		lv := Compute(cfg, 100, 100, 0, tc.held)
		if !closeTo(lv.Target, tc.want) {
			t.Errorf("held %s: target = %v, want %v", tc.held, lv.Target, tc.want)
		}

		if !strings.Contains(lv.TargetReason, "roi") {
			t.Errorf(
				"held %s: reason = %q, want it to name the ROI ladder",
				tc.held,
				lv.TargetReason,
			)
		}
	}
}

// TestROITakesTheNearerTarget checks the ladder and a fixed take-profit combine
// into whichever exits sooner.
func TestROITakesTheNearerTarget(t *testing.T) {
	t.Parallel()

	cfg := config.ExitsConfig{
		Enabled: true, TakeProfitPct: 0.20,
		ROI: map[string]float64{"0s": 0.05},
	}

	lv := Compute(cfg, 100, 100, 0, time.Minute)
	if !closeTo(lv.Target, 105) {
		t.Errorf("target = %v, want the nearer ROI target of 105", lv.Target)
	}
}

// TestROIIgnoresBadKeys checks an unparseable duration is skipped rather than
// crashing a running bot.
func TestROIIgnoresBadKeys(t *testing.T) {
	t.Parallel()

	cfg := config.ExitsConfig{Enabled: true, ROI: map[string]float64{
		"not-a-duration": 0.99, "0s": 0.05,
	}}

	if lv := Compute(cfg, 100, 100, 0, time.Hour); !closeTo(lv.Target, 105) {
		t.Errorf("target = %v, want 105 from the one valid key", lv.Target)
	}
}

// TestTriggeredPrefersTheLoss checks the ordering guarantee: when a price
// breaches both a stop and a target, the stop is reported.
func TestTriggeredPrefersTheLoss(t *testing.T) {
	t.Parallel()

	// A gap can put one price beyond both levels at once (stop below, target
	// above is the normal arrangement; here the target has decayed under the
	// stop via the ROI ladder). The loss must be the one reported.
	lv := Levels{Stop: 95, Target: 90, TargetReason: "roi 6h"}

	if got := lv.Triggered(94); !strings.Contains(got, "stop-loss") {
		t.Errorf("Triggered = %q, want the stop-loss to win", got)
	}

	normal := Levels{Stop: 95, Target: 110, TargetReason: "take-profit"}
	if got := normal.Triggered(100); got != "" {
		t.Errorf("Triggered = %q at a price between the levels, want no exit", got)
	}

	if got := normal.Triggered(111); !strings.Contains(got, "take-profit") {
		t.Errorf("Triggered = %q above the target, want a take-profit", got)
	}
}

// TestDisabledRulesProduceNoLevels checks zeroes mean "off", not "exit at 0".
func TestDisabledRulesProduceNoLevels(t *testing.T) {
	t.Parallel()

	lv := Compute(config.ExitsConfig{Enabled: true}, 100, 120, 2, time.Hour)
	if lv.Stop != 0 || lv.Trail != 0 || lv.Target != 0 {
		t.Errorf("levels = %+v with nothing configured, want all zero", lv)
	}

	if got := lv.Triggered(1); got != "" {
		t.Errorf("Triggered = %q with no rules configured, want none", got)
	}
}

// TestZeroEntryIsSafe checks a position with no known entry price produces no
// levels rather than nonsense ones.
func TestZeroEntryIsSafe(t *testing.T) {
	t.Parallel()

	cfg := config.ExitsConfig{Enabled: true, StopLossPct: 0.05, TakeProfitPct: 0.1}
	if lv := Compute(cfg, 0, 0, 0, 0); lv.Stop != 0 || lv.Target != 0 {
		t.Errorf("levels = %+v for a zero entry, want none", lv)
	}
}

// TestATRPeriodDefault checks the lookback default and override.
func TestATRPeriodDefault(t *testing.T) {
	t.Parallel()

	if got := ATRPeriod(config.ExitsConfig{}); got != defaultATRPeriod {
		t.Errorf("ATRPeriod = %d, want %d", got, defaultATRPeriod)
	}

	if got := ATRPeriod(config.ExitsConfig{ATRPeriod: 21}); got != 21 {
		t.Errorf("ATRPeriod = %d, want the configured 21", got)
	}

	if UsesATR(config.ExitsConfig{}) {
		t.Error("UsesATR true with no ATR multiples configured")
	}

	if !UsesATR(config.ExitsConfig{ATRTrailMult: 1}) {
		t.Error("UsesATR false with an ATR trail configured")
	}
}

// TestRungProfitPctIndexesTheUsableLadder reproduces the BT-2 finding.
// NextPartial indexes the SORTED, FILTERED ladder; a caller that needs a rung's
// price level (internal/backtest fills a scale-out at the rung's own threshold)
// must read it from the same list. Re-deriving the order from the raw config
// slice looked equivalent but silently was not — sortedRungs drops unusable
// rungs, so one bad entry shifted every index.
func TestRungProfitPctIndexesTheUsableLadder(t *testing.T) {
	cfg := config.ExitsConfig{
		Enabled: true,
		PartialTakeProfit: []config.PartialExit{
			{ProfitPct: 0, SellPct: 0.30},    // unusable: dropped by sortedRungs
			{ProfitPct: 0.10, SellPct: 0.30}, // usable rung 1 once sorted
			{ProfitPct: 0.05, SellPct: 0.30}, // usable rung 0 once sorted
		},
	}

	if got := PartialRungs(cfg); got != 2 {
		t.Fatalf("PartialRungs = %d, want 2 (the unusable rung is dropped)", got)
	}

	for i, want := range []float64{0.05, 0.10} {
		got, ok := RungProfitPct(cfg, i)
		if !ok || got != want {
			t.Errorf("RungProfitPct(%d) = %v, %v; want %v, true — indexes must match "+
				"the list NextPartial counted, in ascending profit order", i, got, ok, want)
		}
	}

	if _, ok := RungProfitPct(cfg, 2); ok {
		t.Error("RungProfitPct past the usable ladder reported ok=true")
	}

	if _, ok := RungProfitPct(cfg, -1); ok {
		t.Error("RungProfitPct(-1) reported ok=true")
	}

	// The index NextPartial actually hands out must resolve to the threshold it
	// named in its own reason string.
	part, due := NextPartial(cfg, 0.12, 0)
	if !due {
		t.Fatal("no rung due at +12% with none taken")
	}

	got, ok := RungProfitPct(cfg, part.Rung)
	if !ok || got != 0.10 {
		t.Errorf("the rung NextPartial reported (%d, %q) resolves to %v; want 0.10",
			part.Rung, part.Reason, got)
	}
}

// readmeTrail is the README's two-stage trailing stop: loose 10% below a 5%
// gain, tight 2% at or above it.
var readmeTrail = config.ExitsConfig{
	Enabled: true, TrailingStopPct: 0.10, TrailingOffsetPct: 0.05, TrailingTightPct: 0.02,
}

// TestRebaseOnAddAveragingDownKeepsTheTrailBelowThePrice is EX2-1's
// averaging-down case. Buy at 100, peak 104.5, loose trail 94.05; at 95 an
// equal add makes the average 97.5. Judged against 97.5, the 104.5 peak reads
// as +7.2% — the tight stage — and the trail sat at 102.41, above the 95
// price: the whole enlarged position sold at a loss on the next check.
func TestRebaseOnAddAveragingDownKeepsTheTrailBelowThePrice(t *testing.T) {
	t.Parallel()

	peak, floor := RebaseOnAdd(readmeTrail, 100, 104.5, 0, 0, 97.5, 95)

	lv := Compute(readmeTrail, 97.5, peak, 0, 0).WithTrailFloor(readmeTrail, floor)
	if lv.Trail >= 95 {
		t.Fatalf("trail after averaging down = %.2f, at or above the 95 price — "+
			"the next check sells the enlarged position", lv.Trail)
	}

	if math.Abs(lv.Trail-94.05) > 1e-9 {
		t.Errorf("trail = %.2f, want the 94.05 in force before the add, kept as the floor", lv.Trail)
	}

	if r := lv.Triggered(95); r != "" {
		t.Errorf("Triggered(95) = %q right after the add, want nothing", r)
	}
}

// TestRebaseOnAddPyramidingUpNeverLowersTheTrail is the other direction of
// EX2-1. Buy at 100, peak 110 (+10%, tight trail 107.80); a pyramid at 110
// raises the average to 105. Against 105 the gain reads +4.8% — back under
// the offset — so the trail fell to the loose 99, or vanished under
// trailing_only_after_offset. A trailing stop must not move down.
func TestRebaseOnAddPyramidingUpNeverLowersTheTrail(t *testing.T) {
	t.Parallel()

	for _, onlyAfter := range []bool{false, true} {
		cfg := readmeTrail
		cfg.TrailingOnlyAfterOffset = onlyAfter

		peak, floor := RebaseOnAdd(cfg, 100, 110, 0, 0, 105, 110)

		lv := Compute(cfg, 105, peak, 0, 0).WithTrailFloor(cfg, floor)
		if lv.Trail < 107.8-1e-9 {
			t.Errorf("only_after_offset=%v: trail after a pyramid = %.2f, below the 107.80 it already was",
				onlyAfter, lv.Trail)
		}
	}
}

// TestTrailFloorLapsesWithTheTrail: a floor an add left behind is part of the
// trailing stop, so switching every trailing distance off must switch it off
// too — found in verification of the EX2-1 fix, where a disabled trail kept
// firing for exactly the positions that had been added to.
func TestTrailFloorLapsesWithTheTrail(t *testing.T) {
	t.Parallel()

	off := config.ExitsConfig{Enabled: true, StopLossPct: 0.05}

	if lv := Compute(off, 100, 110, 0, 0).WithTrailFloor(off, 107.8); lv.Trail != 0 {
		t.Errorf("trail = %.2f with trailing switched off, want 0", lv.Trail)
	}
}

// EX2-4 pins a decision rather than a fix: past the offset, the ATR trail
// still overrides trailing_tight_pct when it is wider. The README and the
// example config now say so; this test exists so changing it is a decision
// someone makes on purpose, not a side effect.
func TestATRTrailBoundsTheTightStage(t *testing.T) {
	t.Parallel()

	cfg := config.ExitsConfig{
		Enabled: true, TrailingStopPct: 0.10, TrailingOffsetPct: 0.05,
		TrailingTightPct: 0.02, ATRTrailMult: 3,
	}

	if got := Compute(cfg, 100, 110, 1.5, 0).Trail; !closeTo(got, 105.50) {
		t.Errorf("trail = %.2f, want 105.50 (3 ATR wider than the 2%% tight stage)", got)
	}

	if got := Compute(cfg, 100, 110, 0.1, 0).Trail; !closeTo(got, 107.80) {
		t.Errorf("trail = %.2f, want 107.80: a small ATR must leave the tight stage in force", got)
	}
}

// EX3-2, the live label: with the trail above the stop, a gap below both was
// named a stop-loss, though the trail is the level a falling price reached.
func TestTriggeredNamesTheNearerDownsideLevel(t *testing.T) {
	t.Parallel()

	lv := Compute(config.ExitsConfig{Enabled: true, StopLossPct: 0.05, TrailingStopPct: 0.03}, 100, 102, 0, 0)

	if r := lv.Triggered(94); !strings.HasPrefix(r, "trailing-stop") {
		t.Errorf("a gap below trail %.2f and stop %.2f was named %q", lv.Trail, lv.Stop, r)
	}
}
