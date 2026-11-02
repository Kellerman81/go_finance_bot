// Package protect implements trading protections: the circuit breakers that
// stop a strategy digging when it is already in a hole.
//
// The money limits in internal/risk cap how much can be at stake at any moment.
// They cannot see *patterns*: a strategy that is wrong about a symbol four times
// in an hour, or one whose equity has slid 20% below its peak, is still within
// every per-order limit while it loses steadily. Protections watch outcomes over
// time and take the strategy off the field:
//
//   - a cooldown after a losing exit, so the bot does not immediately re-enter
//     the trade it just lost on;
//   - a stop-loss guard that locks a symbol after repeated stop-outs, which is
//     what a symbol behaving unlike the backtest looks like;
//   - a drawdown halt on the whole account, and a daily-loss halt, both of which
//     stop automated trading outright until a human looks at it.
//
// Every gate fails open on a zero setting (unconfigured protections block
// nothing) and closed once tripped (a halt persists until explicitly resumed).
package protect

import (
	"fmt"
	"sync"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// Manager evaluates the configured protections. The zero value is not usable;
// build one with New. A nil *Manager is valid and allows everything, so callers
// with protections disabled need no special case.
type Manager struct {
	cfg config.ProtectionsConfig
	now func() time.Time

	mu sync.Mutex
	// halted trading is off until Resume; the reason is shown in the UI and API.
	halted     bool
	haltReason string
	haltedAt   time.Time

	peakEquity  float64
	dayEquity   float64 // equity at the start of the current day
	lastEquity  float64
	day         string
	lockedUntil map[string]lockEntry // symbol -> no new entries before this, and why
	stopOuts    map[string][]time.Time
	closes      map[string][]closedTrade // symbol -> recent realized outcomes

	// realizedPnL/realizedPeak track MaxRealizedDrawdownPct — cumulative
	// realized P&L across all closed trades, and its own high-water mark.
	// Unlike peakEquity, these update only on a closed trade (ObserveRealizedClose),
	// never on every tick, so unrealized mark-to-market swings never touch them.
	realizedPnL  float64
	realizedPeak float64
}

// lockEntry is one symbol's entry lock: how long it lasts and which guard set
// it, so the UI can show more than just a bare timestamp.
type lockEntry struct {
	until  time.Time
	reason string
}

// Lock reasons, shared between lockUntil's callers and the UI (see
// Status.Locked / LockReasons).
const (
	LockCooldown  = "cooldown after a loss"
	LockStopGuard = "stop-loss guard"
	LockLowProfit = "low-profit guard"
)

// closedTrade is one realized round-trip, kept only long enough to answer the
// low-profit guard's question.
type closedTrade struct {
	at  time.Time
	pnl float64
}

// Config returns the active protection settings.
func (m *Manager) Config() config.ProtectionsConfig {
	if m == nil {
		return config.ProtectionsConfig{}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	return m.cfg
}

// SetConfig replaces the thresholds while PRESERVING the live state: an active
// halt stays halted, symbol lockouts stay locked, and the equity peak is kept.
//
// That preservation is the whole point. Rebuilding the manager on every edit
// would mean adjusting an unrelated threshold silently cleared a drawdown halt —
// turning a settings tweak into an un-halt, which is exactly the action that is
// supposed to require a deliberate Resume.
func (m *Manager) SetConfig(cfg config.ProtectionsConfig) {
	if m == nil {
		return
	}

	m.mu.Lock()

	m.cfg = cfg
	m.mu.Unlock()
}

// SetClock replaces the manager's time source. A backtest must drive it from
// the bar being simulated, not the wall clock: cooldowns and stop-guard windows
// measured in real seconds would expire instantly while replaying years of
// history in a few seconds, and the protections would appear to do nothing.
//
// Call it before use; it is not safe to change the clock concurrently with
// evaluation.
func (m *Manager) SetClock(now func() time.Time) {
	if m == nil || now == nil {
		return
	}

	m.now = now
}

// Status is the protections' current state, for the API and UI.
type Status struct {
	// Enabled reports whether the gates are actually enforcing. It is false
	// both when no manager exists and when one exists with protections
	// switched off — the latter still carries real Halted/PeakEquity/Locked
	// state, preserved for a re-enable, so a consumer that wants "is there
	// anything to show?" must look at those rather than at this.
	Enabled     bool      `json:"enabled"`
	Halted      bool      `json:"halted"`
	HaltReason  string    `json:"halt_reason,omitempty"`
	HaltedAt    time.Time `json:"halted_at,omitempty"`
	PeakEquity  float64   `json:"peak_equity"`
	DrawdownPct float64   `json:"drawdown_pct"`
	// RealizedPnL/RealizedPeak/RealizedDrawdownPct mirror PeakEquity/
	// DrawdownPct for MaxRealizedDrawdownPct — see ObserveRealizedClose.
	RealizedPnL         float64             `json:"realized_pnl"`
	RealizedPeak        float64             `json:"realized_peak"`
	RealizedDrawdownPct float64             `json:"realized_drawdown_pct"`
	Locked              map[string]LockInfo `json:"locked,omitempty"` // symbol -> until/why
}

// LockInfo is one symbol's current entry lock, as reported by Status.
type LockInfo struct {
	Until  string `json:"until"`  // RFC3339
	Reason string `json:"reason"` // one of the Lock* constants above
}

// State is the subset of a Manager's state that must survive a process
// restart — everything a fresh Manager would otherwise have to rediscover
// from scratch, which for Halted specifically means not rediscovering it at
// all: a restart resets peakEquity to whatever equity is on the first
// observation after startup, so a drawdown halt that was active before the
// restart silently un-halts, because the (now-reset) peak makes the
// drawdown compute as 0%. See CODE_REVIEW_FINDINGS.md's halt-not-persisted
// finding, and Engine.persistProtectionState/RestoreProtectionState.
type State struct {
	Halted       bool      `json:"halted"`
	HaltReason   string    `json:"halt_reason,omitempty"`
	HaltedAt     time.Time `json:"halted_at,omitempty"`
	PeakEquity   float64   `json:"peak_equity"`
	DayEquity    float64   `json:"day_equity"`
	Day          string    `json:"day"`
	RealizedPnL  float64   `json:"realized_pnl"`
	RealizedPeak float64   `json:"realized_peak"`
}

// ExportState returns the manager's restart-durable state, for a caller to
// persist.
func (m *Manager) ExportState() State {
	if m == nil {
		return State{}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	return State{
		Halted: m.halted, HaltReason: m.haltReason, HaltedAt: m.haltedAt,
		PeakEquity: m.peakEquity, DayEquity: m.dayEquity, Day: m.day,
		RealizedPnL: m.realizedPnL, RealizedPeak: m.realizedPeak,
	}
}

// ImportState restores previously-exported state. Call once, right after
// New, before the manager observes any live equity — it does not merge with
// existing state, it replaces it.
func (m *Manager) ImportState(s State) {
	if m == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.halted, m.haltReason, m.haltedAt = s.Halted, s.HaltReason, s.HaltedAt
	m.peakEquity, m.dayEquity, m.day = s.PeakEquity, s.DayEquity, s.Day
	m.realizedPnL, m.realizedPeak = s.RealizedPnL, s.RealizedPeak
}

// New builds a Manager from config. It returns nil when protections are
// disabled, which every method handles.
func New(cfg config.ProtectionsConfig) *Manager {
	if !cfg.Enabled {
		return nil
	}

	return &Manager{
		cfg:         cfg,
		now:         time.Now,
		lockedUntil: make(map[string]lockEntry),
		stopOuts:    make(map[string][]time.Time),
		closes:      make(map[string][]closedTrade),
	}
}

// AllowEntry reports whether a new position may be opened in symbol. The reason
// explains a refusal and is recorded on the blocked trade, so the log says which
// protection fired rather than just "blocked".
//
// Exits are never gated: a protection must be able to stop the bot buying, never
// stop it selling out of a position it already holds.
func (m *Manager) AllowEntry(symbol string) (bool, string) {
	if m == nil {
		return true, ""
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Disabled behaves like nil for gating purposes, but — unlike replacing
	// the manager with nil — preserves halted/lockedUntil/peakEquity so a
	// later re-enable doesn't silently clear an active halt. See
	// Engine.SetProtectionsConfig and CODE_REVIEW_FINDINGS.md's
	// disable-destroys-halt finding.
	if !m.cfg.Enabled {
		return true, ""
	}

	if m.halted {
		return false, "protections: trading halted (" + m.haltReason + ")"
	}

	if entry, locked := m.lockedUntil[symbol]; locked {
		if now := m.now(); now.Before(entry.until) {
			return false, fmt.Sprintf("protections: %s locked for another %s (%s)",
				symbol, entry.until.Sub(now).Round(time.Second), entry.reason)
		}

		delete(m.lockedUntil, symbol)
	}

	return true, ""
}

// RecordClose reports a closed position's outcome. A loss starts the cooldown; a
// stop-loss exit also counts toward the stop guard. stoppedOut marks exits that
// came from a protective stop rather than a strategy signal — repeated stop-outs
// are the signal the guard is looking for.
//
//nolint:cyclop // one guard per protection a close feeds
func (m *Manager) RecordClose(symbol string, pnl float64, stoppedOut bool) {
	if m == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.cfg.Enabled {
		return
	}

	now := m.now()

	if pnl < 0 && m.cfg.CooldownAfterLoss > 0 {
		m.lockUntil(symbol, now.Add(m.cfg.CooldownAfterLoss), LockCooldown)
	}

	m.checkLowProfit(symbol, pnl, now)

	// A stop-out that LOST is the pattern this guard exists for — a symbol
	// behaving unlike the backtest. A trailing stop exiting in profit is the
	// trail doing its job: counting those locked a symbol for 4h after 4
	// winning exits in a day, as a "stop-loss guard" (PR2-3). Both the live
	// engine and the backtester report trailing-stop exits as stop-outs, so
	// the P&L is checked here, once, for both.
	if !stoppedOut || pnl >= 0 || m.cfg.StopGuardTrades <= 0 || m.cfg.StopGuardLookback <= 0 {
		return
	}

	cutoff := now.Add(-m.cfg.StopGuardLookback)
	kept := m.stopOuts[symbol][:0]

	for _, t := range m.stopOuts[symbol] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}

	kept = append(kept, now)
	m.stopOuts[symbol] = kept

	if len(kept) >= m.cfg.StopGuardTrades {
		m.lockUntil(symbol, now.Add(m.cfg.StopGuardLock), LockStopGuard)

		m.stopOuts[symbol] = nil
	}
}

// checkLowProfit records a realized outcome and locks the symbol when its recent
// trades have not, on balance, made money.
//
// The guard needs at least LowProfitTrades closes inside the window before it
// judges: two losses on a strategy that wins six times in ten is ordinary, and
// locking on that would remove symbols for being normal. Caller holds m.mu.
func (m *Manager) checkLowProfit(symbol string, pnl float64, now time.Time) {
	if m.cfg.LowProfitTrades <= 0 || m.cfg.LowProfitLookback <= 0 {
		return
	}

	cutoff := now.Add(-m.cfg.LowProfitLookback)
	kept := m.closes[symbol][:0]

	for _, c := range m.closes[symbol] {
		if c.at.After(cutoff) {
			kept = append(kept, c)
		}
	}

	kept = append(kept, closedTrade{at: now, pnl: pnl})
	m.closes[symbol] = kept

	if len(kept) < m.cfg.LowProfitTrades {
		return
	}

	var total float64

	for _, c := range kept {
		total += c.pnl
	}

	if total > m.cfg.LowProfitMinPnL {
		return
	}

	m.lockUntil(symbol, now.Add(m.cfg.LowProfitLock), LockLowProfit)

	// Start the count again: the symbol has been judged on this window, and
	// carrying it forward would re-lock immediately on the next single close.
	m.closes[symbol] = nil
}

// lockUntil extends a symbol's entry lock to at least t, recording reason as
// the guard that set it (overwriting an earlier, shorter lock's reason along
// with its expiry — the UI shows whichever guard is currently the binding
// one). Caller holds m.mu.
func (m *Manager) lockUntil(symbol string, t time.Time, reason string) {
	if cur, ok := m.lockedUntil[symbol]; !ok || t.After(cur.until) {
		m.lockedUntil[symbol] = lockEntry{until: t, reason: reason}
	}
}

// ObserveEquity feeds the account's current equity to the drawdown and
// daily-loss protections. It returns a halt reason the first time a limit is
// breached (empty otherwise), so the caller can log and record the halt once
// rather than on every tick.
//
//nolint:cyclop // two independent limit checks over one equity observation
func (m *Manager) ObserveEquity(equity float64) string {
	if m == nil || equity <= 0 {
		return ""
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()

	if day := now.UTC().Format("2006-01-02"); day != m.day {
		m.day = day
		m.dayEquity = equity
	}

	m.lastEquity = equity
	if equity > m.peakEquity {
		m.peakEquity = equity
	}

	if m.halted {
		return ""
	}

	// Peak/day equity are still tracked above even while disabled, so a later
	// re-enable doesn't judge drawdown against a stale peak — but a NEW halt
	// must not fire while the operator has explicitly disabled protections.
	if !m.cfg.Enabled {
		return ""
	}

	if m.cfg.MaxDrawdownPct > 0 && m.peakEquity > 0 {
		if dd := (m.peakEquity - equity) / m.peakEquity; dd >= m.cfg.MaxDrawdownPct {
			return m.halt(fmt.Sprintf("max drawdown %.1f%% >= %.1f%% (peak %.2f, now %.2f)",
				dd*100, m.cfg.MaxDrawdownPct*100, m.peakEquity, equity))
		}
	}

	if m.cfg.MaxDailyLossPct > 0 && m.dayEquity > 0 {
		if loss := (m.dayEquity - equity) / m.dayEquity; loss >= m.cfg.MaxDailyLossPct {
			return m.halt(fmt.Sprintf("daily loss %.1f%% >= %.1f%% (day start %.2f, now %.2f)",
				loss*100, m.cfg.MaxDailyLossPct*100, m.dayEquity, equity))
		}
	}

	return ""
}

// ObserveRealizedClose feeds one closed trade's realized P&L to the
// realized-drawdown protection. Unlike ObserveEquity, it never runs on a bare
// tick — only when a trade actually closes — so unrealized mark-to-market
// swings in positions still held never count toward it; only money a closed
// trade actually locked in does. Returns a halt reason the first time
// MaxRealizedDrawdownPct is breached (empty otherwise).
//
// The drawdown is expressed as a fraction of peakEquity (the same
// mark-to-market peak ObserveEquity tracks) rather than of realizedPeak
// itself, since realizedPeak can sit at or near zero — a percentage of that
// would be meaningless or wildly unstable early on.
func (m *Manager) ObserveRealizedClose(pnl float64) string {
	if m == nil {
		return ""
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.realizedPnL += pnl
	if m.realizedPnL > m.realizedPeak {
		m.realizedPeak = m.realizedPnL
	}

	if m.halted || !m.cfg.Enabled || m.cfg.MaxRealizedDrawdownPct <= 0 || m.peakEquity <= 0 {
		return ""
	}

	giveback := m.realizedPeak - m.realizedPnL
	if dd := giveback / m.peakEquity; dd >= m.cfg.MaxRealizedDrawdownPct {
		return m.halt(fmt.Sprintf(
			"realized drawdown %.1f%% >= %.1f%% (peak realized P&L %.2f, now %.2f)",
			dd*100, m.cfg.MaxRealizedDrawdownPct*100, m.realizedPeak, m.realizedPnL))
	}

	return ""
}

// halt records the halt and returns its reason. Caller holds m.mu.
func (m *Manager) halt(reason string) string {
	m.halted = true
	m.haltReason = reason
	m.haltedAt = m.now()

	return reason
}

// Halted reports whether automated trading is halted, and why.
func (m *Manager) Halted() (bool, string) {
	if m == nil {
		return false, ""
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	return m.halted, m.haltReason
}

// Resume clears a halt. It is a deliberate human action (an API call): a
// protection that cleared itself on a timer would let an unattended bot resume
// into exactly the conditions that tripped it. Resuming also re-bases the
// drawdown peak to the current equity, so the same drawdown does not
// immediately re-trip.
func (m *Manager) Resume(equity float64) {
	if m == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.halted = false
	m.haltReason = ""
	m.haltedAt = time.Time{}

	if equity > 0 {
		m.peakEquity = equity
		m.dayEquity = equity
	}

	// Re-base the realized-drawdown baseline too, same reasoning as the
	// equity peak above: without this, a symbol closing at even a small loss
	// right after Resume would immediately re-trip on the still-stale
	// realizedPeak.
	m.realizedPeak = m.realizedPnL
}

// LowerPeak backdates the tracked peak/day-equity baselines by delta, for a
// caller that just removed equity from its own accounting that was never a
// real trading gain to begin with — the sole current caller is
// engine.Engine.ForgetOpenPosition, whose delta is the phantom market value
// a stale, broker-unconfirmed position record had been contributing to
// equity. Unlike Resume, this does not touch the halt state or reset the
// peak to current equity: it only removes exactly the amount that was never
// real, so a bookkeeping correction can never itself read as a drawdown.
// peakEquity/dayEquity are floored at 0 rather than going negative, which
// would otherwise flip the sign of every subsequent drawdown/daily-loss
// ratio.
func (m *Manager) LowerPeak(delta float64) {
	if m == nil || delta <= 0 {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.peakEquity = max(0, m.peakEquity-delta)
	m.dayEquity = max(0, m.dayEquity-delta)
}

// Unlock clears a symbol's entry lock (cooldown or stop guard).
func (m *Manager) Unlock(symbol string) {
	if m == nil {
		return
	}

	m.mu.Lock()

	delete(m.lockedUntil, symbol)
	delete(m.stopOuts, symbol)
	delete(m.closes, symbol) // releasing a symbol also forgives its recent P&L
	m.mu.Unlock()
}

// Status returns a snapshot of the protections' state.
func (m *Manager) Status() Status {
	if m == nil {
		return Status{}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// cfg.Enabled, not a hardcoded true: a manager is deliberately kept alive
	// with Enabled=false when the operator turns protections off (see
	// SetConfig and engine.SetProtectionsConfig), so that an active halt and
	// the symbol lockouts survive for a later re-enable. Reporting "enabled"
	// for that manager told the API and the UI that the breakers were armed
	// while every gate was in fact waving orders through — the one thing a
	// status field must never do.
	s := Status{
		Enabled: m.cfg.Enabled, Halted: m.halted, HaltReason: m.haltReason,
		HaltedAt: m.haltedAt, PeakEquity: m.peakEquity,
		RealizedPnL: m.realizedPnL, RealizedPeak: m.realizedPeak,
	}

	if m.peakEquity > 0 && m.lastEquity > 0 {
		s.DrawdownPct = (m.peakEquity - m.lastEquity) / m.peakEquity * 100
	}

	if m.peakEquity > 0 {
		s.RealizedDrawdownPct = (m.realizedPeak - m.realizedPnL) / m.peakEquity * 100
	}

	now := m.now()

	for sym, entry := range m.lockedUntil {
		if entry.until.After(now) {
			if s.Locked == nil {
				s.Locked = make(map[string]LockInfo)
			}

			s.Locked[sym] = LockInfo{Until: entry.until.Format(time.RFC3339), Reason: entry.reason}
		}
	}

	return s
}
