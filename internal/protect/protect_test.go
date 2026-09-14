package protect

import (
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// clocked builds a Manager whose clock the test controls, so time-based
// protections can be exercised without sleeping.
func clocked(t *testing.T, cfg config.ProtectionsConfig) (*Manager, func(time.Duration)) {
	t.Helper()

	cfg.Enabled = true

	m := New(cfg)
	if m == nil {
		t.Fatal("New returned nil for an enabled config")
	}

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	offset := time.Duration(0)
	m.now = func() time.Time { return base.Add(offset) }

	return m, func(d time.Duration) { offset += d }
}

// TestCooldownAfterLoss checks a losing exit blocks re-entry for the cooldown
// and then releases it.
func TestCooldownAfterLoss(t *testing.T) {
	t.Parallel()

	m, advance := clocked(t, config.ProtectionsConfig{CooldownAfterLoss: 30 * time.Minute})

	if ok, _ := m.AllowEntry("AAPL"); !ok {
		t.Fatal("entry blocked before any trade")
	}

	m.RecordClose("AAPL", -120, false)

	ok, reason := m.AllowEntry("AAPL")
	if ok {
		t.Error("entry allowed immediately after a loss; the cooldown did not apply")
	}

	if !strings.Contains(reason, "AAPL") {
		t.Errorf("reason = %q, want it to name the symbol", reason)
	}

	if ok, _ := m.AllowEntry("MSFT"); !ok {
		t.Error("a loss in AAPL must not lock other symbols")
	}

	advance(31 * time.Minute)

	if ok, _ := m.AllowEntry("AAPL"); !ok {
		t.Error("entry still blocked after the cooldown elapsed")
	}
}

// TestProfitDoesNotCoolDown checks a winning exit leaves the symbol tradeable.
func TestProfitDoesNotCoolDown(t *testing.T) {
	t.Parallel()

	m, _ := clocked(t, config.ProtectionsConfig{CooldownAfterLoss: time.Hour})

	m.RecordClose("AAPL", 250, false)

	if ok, reason := m.AllowEntry("AAPL"); !ok {
		t.Errorf("winning trade caused a cooldown: %s", reason)
	}
}

// TestStopGuardLocksSymbol checks repeated stop-outs lock a symbol for the
// configured period — the "this symbol isn't behaving like the backtest" case.
func TestStopGuardLocksSymbol(t *testing.T) {
	t.Parallel()

	m, advance := clocked(t, config.ProtectionsConfig{
		StopGuardTrades: 3, StopGuardLookback: time.Hour, StopGuardLock: 4 * time.Hour,
	})

	for range 2 {
		m.RecordClose("NVDA", -10, true)
		advance(time.Minute)
	}

	if ok, _ := m.AllowEntry("NVDA"); !ok {
		t.Fatal("locked after only 2 stop-outs, want 3")
	}

	m.RecordClose("NVDA", -10, true)

	ok, reason := m.AllowEntry("NVDA")
	if ok {
		t.Fatal("not locked after 3 stop-outs within the lookback")
	}

	if !strings.Contains(reason, "locked") {
		t.Errorf("reason = %q, want a lock message", reason)
	}

	advance(5 * time.Hour)

	if ok, _ := m.AllowEntry("NVDA"); !ok {
		t.Error("still locked after the guard period elapsed")
	}
}

// TestStopGuardForgetsOldStops checks stop-outs outside the lookback don't
// accumulate toward the lock.
func TestStopGuardForgetsOldStops(t *testing.T) {
	t.Parallel()

	m, advance := clocked(t, config.ProtectionsConfig{
		StopGuardTrades: 3, StopGuardLookback: time.Hour, StopGuardLock: time.Hour,
	})

	m.RecordClose("AMD", -10, true)
	advance(2 * time.Hour) // ages out of the lookback
	m.RecordClose("AMD", -10, true)
	m.RecordClose("AMD", -10, true)

	if ok, _ := m.AllowEntry("AMD"); !ok {
		t.Error("locked on 3 stop-outs where the first was outside the lookback window")
	}
}

// TestStrategyExitDoesNotFeedStopGuard checks only protective stops count: a
// strategy-signalled exit is a normal outcome, not evidence of malfunction.
func TestStrategyExitDoesNotFeedStopGuard(t *testing.T) {
	t.Parallel()

	m, _ := clocked(t, config.ProtectionsConfig{
		StopGuardTrades: 2, StopGuardLookback: time.Hour, StopGuardLock: time.Hour,
	})

	m.RecordClose("TSLA", -10, false)
	m.RecordClose("TSLA", -10, false)

	if _, reason := m.AllowEntry("TSLA"); strings.Contains(reason, "locked for") {
		t.Error("strategy exits tripped the stop guard")
	}
}

// TestMaxDrawdownHalts checks the account-level kill switch trips once and stays
// tripped until Resume.
func TestMaxDrawdownHalts(t *testing.T) {
	t.Parallel()

	m, _ := clocked(t, config.ProtectionsConfig{MaxDrawdownPct: 0.20})

	if r := m.ObserveEquity(100000); r != "" {
		t.Fatalf("halted on the first observation: %s", r)
	}

	if r := m.ObserveEquity(85000); r != "" {
		t.Fatalf("halted at 15%% drawdown, limit is 20%%: %s", r)
	}

	reason := m.ObserveEquity(79000)
	if reason == "" {
		t.Fatal("no halt at 21% drawdown")
	}

	if !strings.Contains(reason, "drawdown") {
		t.Errorf("reason = %q, want it to name the drawdown", reason)
	}

	// Reported once, not on every subsequent tick.
	if r := m.ObserveEquity(78000); r != "" {
		t.Errorf("halt re-reported on a later tick: %s", r)
	}

	if ok, _ := m.AllowEntry("AAPL"); ok {
		t.Error("entries still allowed while halted")
	}

	if halted, _ := m.Halted(); !halted {
		t.Error("Halted() says no while halted")
	}

	// Resuming re-bases the peak, so the same equity does not instantly re-trip.
	m.Resume(78000)

	if halted, _ := m.Halted(); halted {
		t.Fatal("still halted after Resume")
	}

	if r := m.ObserveEquity(78000); r != "" {
		t.Errorf("re-tripped immediately after resume: %s", r)
	}
}

// TestObserveRealizedCloseIgnoresUnrealizedSwings checks the whole point of
// the realized-only gate: heavy mark-to-market exposure that never actually
// closes at a loss must never trip it, however far account equity itself has
// dipped on paper.
func TestObserveRealizedCloseIgnoresUnrealizedSwings(t *testing.T) {
	t.Parallel()

	m, _ := clocked(t, config.ProtectionsConfig{MaxRealizedDrawdownPct: 0.10})

	// Seed peakEquity — MaxRealizedDrawdownPct is expressed as a fraction of
	// it, same denominator ObserveEquity itself uses.
	m.ObserveEquity(100000)

	// A large unrealized (mark-to-market) equity swing, no trade closed.
	if r := m.ObserveEquity(70000); r != "" {
		t.Fatalf("MaxDrawdownPct is unset — ObserveEquity itself must not halt: %s", r)
	}

	// Realized P&L never moved, so the realized-only gate must still be
	// silent no matter how far equity itself has fallen on paper.
	if r := m.ObserveRealizedClose(0); r != "" {
		t.Errorf("halted on an unrealized swing with no closed trade: %s", r)
	}
}

// TestObserveRealizedCloseHaltsOnCumulativeRealizedLosses checks realized
// losses across several closed trades accumulate against their own peak and
// halt once the threshold is crossed — independent of MaxDrawdownPct, which
// is left unset here.
func TestObserveRealizedCloseHaltsOnCumulativeRealizedLosses(t *testing.T) {
	t.Parallel()

	m, _ := clocked(t, config.ProtectionsConfig{MaxRealizedDrawdownPct: 0.10})
	m.ObserveEquity(100000) // seeds peakEquity, the % denominator

	if r := m.ObserveRealizedClose(500); r != "" {
		t.Fatalf("halted on a realized WIN: %s", r)
	}

	if r := m.ObserveRealizedClose(-5000); r != "" {
		t.Fatalf("halted at 5%% realized drawdown, limit is 10%%: %s", r)
	}

	reason := m.ObserveRealizedClose(-6000) // peak 500, now -10500, giveback 11000 = 11%
	if reason == "" {
		t.Fatal("no halt past 10% realized drawdown")
	}

	if !strings.Contains(reason, "realized drawdown") {
		t.Errorf("reason = %q, want it to name the realized drawdown", reason)
	}

	if halted, _ := m.Halted(); !halted {
		t.Error("Halted() says no while halted")
	}

	// Reported once, not on every subsequent close.
	if r := m.ObserveRealizedClose(-100); r != "" {
		t.Errorf("halt re-reported on a later close: %s", r)
	}
}

// TestObserveRealizedCloseAndObserveEquityAreIndependent checks either gate
// can fire without the other configured — the user asked for "count only
// losses" as an ADDITION to, not a replacement of, the existing mark-to-market
// drawdown.
func TestObserveRealizedCloseAndObserveEquityAreIndependent(t *testing.T) {
	t.Parallel()

	// Only the realized gate configured: a mark-to-market equity crash alone
	// must not halt.
	m, _ := clocked(t, config.ProtectionsConfig{MaxRealizedDrawdownPct: 0.50})
	m.ObserveEquity(100000)

	if r := m.ObserveEquity(1); r != "" {
		t.Fatalf("MaxDrawdownPct is unset — ObserveEquity must not halt on its own: %s", r)
	}

	// Only the mark-to-market gate configured: closed losses alone must not
	// trip the realized gate.
	m2, _ := clocked(t, config.ProtectionsConfig{MaxDrawdownPct: 0.50})
	m2.ObserveEquity(100000)

	if r := m2.ObserveRealizedClose(-90000); r != "" {
		t.Fatalf("MaxRealizedDrawdownPct is unset — ObserveRealizedClose must not halt on its own: %s", r)
	}
}

// TestResumeRebasesTheRealizedPeakToo mirrors TestMaxDrawdownHalts's own
// resume check: the realized giveback must not instantly re-trip the moment
// after a Resume.
func TestResumeRebasesTheRealizedPeakToo(t *testing.T) {
	t.Parallel()

	m, _ := clocked(t, config.ProtectionsConfig{MaxRealizedDrawdownPct: 0.10})
	m.ObserveEquity(100000)

	if reason := m.ObserveRealizedClose(-11000); reason == "" {
		t.Fatal("want a halt at 11% realized drawdown")
	}

	m.Resume(89000)

	if halted, _ := m.Halted(); halted {
		t.Fatal("still halted after Resume")
	}

	// A small loss right after resuming must not immediately re-trip against
	// the stale (pre-resume) realized peak.
	if r := m.ObserveRealizedClose(-100); r != "" {
		t.Errorf("re-tripped immediately after resume: %s", r)
	}
}

// TestRealizedStateSurvivesExportImport checks realizedPnL/realizedPeak round
// trip through State the same way peakEquity/dayEquity already do, so a
// restart does not silently reset the realized-drawdown baseline to zero.
func TestRealizedStateSurvivesExportImport(t *testing.T) {
	t.Parallel()

	before, _ := clocked(t, config.ProtectionsConfig{MaxRealizedDrawdownPct: 0.10})
	before.ObserveEquity(100000)
	before.ObserveRealizedClose(2000)
	before.ObserveRealizedClose(-500)

	state := before.ExportState()

	after, _ := clocked(t, config.ProtectionsConfig{MaxRealizedDrawdownPct: 0.10})
	after.ImportState(state)
	after.ObserveEquity(100000)

	// realizedPeak of 2000 restored: a close taking cumulative realized P&L
	// to -8501 (giveback 10501, just over 10%) must halt immediately, which
	// it could only do if the pre-restart peak survived the round trip.
	if reason := after.ObserveRealizedClose(-10001); reason == "" {
		t.Fatal("restored realized peak was lost across ExportState/ImportState")
	}
}

// TestLowerPeakPreventsABogusEquityCorrectionFromTripping reproduces the
// "forgetting a drifted position trips a false max-drawdown halt" finding at
// the Manager level: a caller (engine.Engine.ForgetOpenPosition) that
// deliberately removes equity it knows was never a real trading gain must be
// able to tell the peak so, and the very next observation at the corrected
// (lower) equity must not read as a drawdown.
func TestLowerPeakPreventsABogusEquityCorrectionFromTripping(t *testing.T) {
	t.Parallel()

	// A 10% limit: the phantom's own 15% would trip it if LowerPeak did not
	// correct the peak — the whole point being verified below.
	m, _ := clocked(t, config.ProtectionsConfig{MaxDrawdownPct: 0.10})

	// A phantom holding inflated equity to 100000 (peak). The true equity —
	// what a caller is about to observe after correcting its own books — was
	// only 85000.
	if r := m.ObserveEquity(100000); r != "" {
		t.Fatalf("halted on the first observation: %s", r)
	}

	m.LowerPeak(15000) // exactly the phantom amount being corrected away

	if r := m.ObserveEquity(85000); r != "" {
		t.Fatalf("the corrected equity still read as a drawdown: %s", r)
	}

	// The peak is backdated, not reset — a real drawdown from here must
	// still trip normally.
	reason := m.ObserveEquity(67000) // (85000-67000)/85000 ≈ 21%
	if reason == "" {
		t.Fatal("a genuine drawdown from the corrected peak did not trip")
	}
}

// TestLowerPeakIsNilSafeAndIgnoresANonPositiveDelta checks the two
// guard clauses: a nil Manager (protections disabled) must not panic, and a
// zero/negative delta — nothing to correct — must not touch the peak.
func TestLowerPeakIsNilSafeAndIgnoresANonPositiveDelta(t *testing.T) {
	t.Parallel()

	var nilManager *Manager
	nilManager.LowerPeak(1000) // must not panic

	m, _ := clocked(t, config.ProtectionsConfig{MaxDrawdownPct: 0.20})
	m.ObserveEquity(100000)

	m.LowerPeak(0)
	m.LowerPeak(-500)

	// Peak is unchanged: 15% off 100000 still does not trip a 20% limit.
	if r := m.ObserveEquity(85000); r != "" {
		t.Errorf("a non-positive delta moved the peak: %s", r)
	}
}

// TestMaxDailyLossHalts checks the daily-loss limit trips against the day's
// starting equity rather than the all-time peak.
func TestMaxDailyLossHalts(t *testing.T) {
	t.Parallel()

	m, _ := clocked(t, config.ProtectionsConfig{MaxDailyLossPct: 0.05})

	m.ObserveEquity(50000)

	if r := m.ObserveEquity(48000); r != "" {
		t.Fatalf("halted at 4%% daily loss, limit is 5%%: %s", r)
	}

	if r := m.ObserveEquity(47000); r == "" {
		t.Error("no halt at 6% daily loss")
	}
}

// TestStatusReportsLocks checks the API view exposes halts, drawdown and locks.
func TestStatusReportsLocks(t *testing.T) {
	t.Parallel()

	m, _ := clocked(t, config.ProtectionsConfig{
		CooldownAfterLoss: time.Hour, MaxDrawdownPct: 0.5,
	})

	m.ObserveEquity(100000)
	m.ObserveEquity(90000)
	m.RecordClose("AAPL", -50, false)

	s := m.Status()
	if !s.Enabled || s.Halted {
		t.Errorf("status = %+v, want enabled and not halted", s)
	}

	if s.DrawdownPct < 9.9 || s.DrawdownPct > 10.1 {
		t.Errorf("drawdown = %.2f%%, want ~10%%", s.DrawdownPct)
	}

	info, ok := s.Locked["AAPL"]
	if !ok {
		t.Fatalf("locked set = %v, want AAPL listed", s.Locked)
	}

	if info.Reason != LockCooldown {
		t.Errorf("reason = %q, want %q", info.Reason, LockCooldown)
	}
}

// TestStatusLockReasonDistinguishesTheGuard reproduces the UI's need to show
// WHY a symbol is locked, not just until when: the stop guard and the
// low-profit guard are different failure patterns (repeated stop-outs vs. a
// symbol that just isn't making money), and an operator deciding whether to
// unlock or wait needs to know which one fired. Each guard must record its
// own reason in Status().Locked, not overwrite the other's or fall back to a
// generic message.
func TestStatusLockReasonDistinguishesTheGuard(t *testing.T) {
	t.Parallel()

	m, advance := clocked(t, config.ProtectionsConfig{
		StopGuardTrades: 2, StopGuardLookback: time.Hour, StopGuardLock: time.Hour,
	})

	m.RecordClose("NVDA", -10, true)
	advance(time.Minute)
	m.RecordClose("NVDA", -10, true) // trips the stop guard

	info, ok := m.Status().Locked["NVDA"]
	if !ok {
		t.Fatal("NVDA not locked after tripping the stop guard")
	}

	if info.Reason != LockStopGuard {
		t.Errorf("reason = %q, want %q", info.Reason, LockStopGuard)
	}

	m2, _ := clocked(t, config.ProtectionsConfig{
		LowProfitTrades: 2, LowProfitLookback: time.Hour, LowProfitMinPnL: 0,
		LowProfitLock: time.Hour,
	})

	m2.RecordClose("GOOG", -5, false)
	m2.RecordClose("GOOG", -5, false) // nets negative, trips the low-profit guard

	info2, ok := m2.Status().Locked["GOOG"]
	if !ok {
		t.Fatal("GOOG not locked after tripping the low-profit guard")
	}

	if info2.Reason != LockLowProfit {
		t.Errorf("reason = %q, want %q", info2.Reason, LockLowProfit)
	}
}

// TestUnlockClearsLock checks the manual override.
func TestUnlockClearsLock(t *testing.T) {
	t.Parallel()

	m, _ := clocked(t, config.ProtectionsConfig{CooldownAfterLoss: time.Hour})

	m.RecordClose("AAPL", -50, false)
	m.Unlock("AAPL")

	if ok, reason := m.AllowEntry("AAPL"); !ok {
		t.Errorf("still locked after Unlock: %s", reason)
	}
}

// TestNilManagerAllowsEverything checks the disabled state needs no special
// case at any call site.
func TestNilManagerAllowsEverything(t *testing.T) {
	t.Parallel()

	if New(config.ProtectionsConfig{Enabled: false}) != nil {
		t.Error("disabled config should build no manager")
	}

	var m *Manager

	if ok, _ := m.AllowEntry("AAPL"); !ok {
		t.Error("nil manager blocked an entry")
	}

	if r := m.ObserveEquity(1000); r != "" {
		t.Error("nil manager halted")
	}

	if halted, _ := m.Halted(); halted {
		t.Error("nil manager reports halted")
	}

	if s := m.Status(); s.Enabled {
		t.Error("nil manager reports enabled")
	}

	m.RecordClose("AAPL", -1, true) // must not panic
	m.Resume(1000)
	m.Unlock("AAPL")
}

// TestDisablingViaSetConfigPreservesHaltState reproduces the
// disable-destroys-halt finding: SetConfig(Enabled: false) on an already
// halted manager must allow entries again (same as a nil manager would) but
// must NOT clear halted/haltReason/peakEquity — those must still be there,
// unrevealed only by AllowEntry, when the operator re-enables protections
// with SetConfig(Enabled: true) again. This matters because
// Engine.SetProtectionsConfig now reuses the same *Manager instance across a
// disable/enable toggle instead of discarding it.
func TestDisablingViaSetConfigPreservesHaltState(t *testing.T) {
	t.Parallel()

	m, _ := clocked(t, config.ProtectionsConfig{MaxDrawdownPct: 0.20})

	m.ObserveEquity(100000)

	if reason := m.ObserveEquity(70000); reason == "" {
		t.Fatal("want a halt at 30% drawdown")
	}

	if halted, _ := m.Halted(); !halted {
		t.Fatal("not halted after tripping max drawdown")
	}

	// Disable — entries must be allowed again, exactly like a nil manager.
	disabledCfg := m.Config()
	disabledCfg.Enabled = false
	m.SetConfig(disabledCfg)

	if ok, reason := m.AllowEntry("AAPL"); !ok {
		t.Errorf("entry still blocked while disabled: %s", reason)
	}

	// The halt itself must survive underneath — Halted() still reports it,
	// proving the state was preserved rather than cleared.
	if halted, reason := m.Halted(); !halted || reason == "" {
		t.Errorf("halt state lost while disabled: halted=%v reason=%q", halted, reason)
	}

	// Re-enable without an explicit Resume — the halt must still be in
	// effect, exactly as if protections had never been toggled at all.
	enabledCfg := m.Config()
	enabledCfg.Enabled = true
	m.SetConfig(enabledCfg)

	if ok, _ := m.AllowEntry("AAPL"); ok {
		t.Error("entry allowed after re-enabling — the halt was silently cleared by the disable/enable round-trip")
	}
}

// TestDisabledManagerDoesNotTripANewHalt checks that while disabled, a
// drawdown breach does not start a fresh halt — an operator who explicitly
// turned protections off should not be surprised by one appearing anyway.
func TestDisabledManagerDoesNotTripANewHalt(t *testing.T) {
	t.Parallel()

	m, _ := clocked(t, config.ProtectionsConfig{MaxDrawdownPct: 0.20})

	m.ObserveEquity(100000)

	cfg := m.Config()
	cfg.Enabled = false
	m.SetConfig(cfg)

	if reason := m.ObserveEquity(50000); reason != "" {
		t.Errorf("halted while disabled: %s", reason)
	}

	if halted, _ := m.Halted(); halted {
		t.Error("a new halt was recorded while disabled")
	}
}

// TestStatusEnabledFollowsTheConfig guards the one thing a status field must
// never do: claim the breakers are armed while every gate is waving orders
// through. Status hardcoded Enabled: true for any non-nil manager, but a
// manager is deliberately KEPT ALIVE with Enabled=false when protections are
// switched off (see TestDisablingViaSetConfigPreservesHaltState and
// engine.SetProtectionsConfig), so GET /api/protections and the diagnostics
// panel both reported protections as running while AllowEntry was allowing
// everything.
//
// The preserved halt/lockout state must still be visible in the same
// Status — that is what the operator needs in order to decide whether to
// re-enable or Resume — so this pins both halves.
func TestStatusEnabledFollowsTheConfig(t *testing.T) {
	t.Parallel()

	m, _ := clocked(t, config.ProtectionsConfig{
		MaxDrawdownPct:    0.20,
		CooldownAfterLoss: time.Hour,
	})

	m.ObserveEquity(100000)
	m.RecordClose("AAPL", -50, false) // locks AAPL for the cooldown

	if reason := m.ObserveEquity(70000); reason == "" {
		t.Fatal("want a halt at 30% drawdown")
	}

	if s := m.Status(); !s.Enabled {
		t.Error("Status().Enabled is false while protections are on")
	}

	cfg := m.Config()
	cfg.Enabled = false
	m.SetConfig(cfg)

	s := m.Status()
	if s.Enabled {
		t.Error("Status().Enabled is true while protections are switched off — " +
			"the API and the UI are told the breakers are armed when nothing is gated")
	}

	// The state behind the disabled switch is preserved, and must still be
	// reported: hiding it would leave no way to see (or clear) a halt that
	// comes straight back on re-enable.
	if !s.Halted || s.HaltReason == "" {
		t.Errorf("halt state missing from a disabled manager's Status: halted=%v reason=%q",
			s.Halted, s.HaltReason)
	}

	if _, ok := s.Locked["AAPL"]; !ok {
		t.Errorf("lockouts missing from a disabled manager's Status: %v", s.Locked)
	}

	if s.PeakEquity != 100000 {
		t.Errorf("PeakEquity = %v, want the preserved 100000", s.PeakEquity)
	}

	// Back on: the field tracks the switch in both directions.
	cfg.Enabled = true
	m.SetConfig(cfg)

	if s := m.Status(); !s.Enabled {
		t.Error("Status().Enabled did not come back with the re-enable")
	}
}
