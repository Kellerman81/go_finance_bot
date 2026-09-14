package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/notify"
)

// TestModeStatus is a snapshot of the training-window state, for the API/UI.
type TestModeStatus struct {
	Active   bool      `json:"active"`
	Deadline time.Time `json:"deadline"` // zero when inactive or no auto-expiry
}

// TestModeConfig returns the configured default (enabled/duration) used when
// this process starts and as the fallback duration for a manual start.
func (e *Engine) TestModeConfig() config.TestModeConfig {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.testModeCfg
}

// SetTestModeConfig installs the configured default. It does not itself
// start or stop an active window — see EnableTestMode/EndTestMode.
func (e *Engine) SetTestModeConfig(cfg config.TestModeConfig) {
	e.mu.Lock()

	e.testModeCfg = cfg
	e.mu.Unlock()
}

// TestModeStatus reports whether a training window is currently active.
func (e *Engine) TestModeStatus() TestModeStatus {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return TestModeStatus{Active: e.testModeActive, Deadline: e.testModeDeadline}
}

// inTestMode is the cheap boolean-only check internal call sites use (see
// checkDrawdown) — TestModeStatus builds a whole struct for the API/UI.
func (e *Engine) inTestMode() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.testModeActive
}

// EnableTestMode starts a training window: e.brk is swapped for an internal
// *broker.Sim seeded from the real broker's current cash and positions, so
// the simulation continues realistically from wherever the real account
// actually stands. dur <= 0 means no auto-expiry (checkTestModeExpiry never
// ends it on its own).
//
// It refuses while a protection halt is active — an operator must resume
// protections first rather than have a fresh simulated window silently paper
// over a real halt.
func (e *Engine) EnableTestMode(ctx context.Context, dur time.Duration) error {
	if p := e.Protections(); p != nil {
		if halted, reason := p.Halted(); halted {
			return fmt.Errorf("test mode refused: a protection halt is active (%s) — resume it first", reason)
		}
	}

	e.mu.RLock()
	realBrk := e.brk
	e.mu.RUnlock()

	acct, err := realBrk.GetAccount(ctx)
	if err != nil {
		return fmt.Errorf("test mode: snapshot account: %w", err)
	}

	positions, err := realBrk.GetPositions(ctx)
	if err != nil {
		return fmt.Errorf("test mode: snapshot positions: %w", err)
	}

	costs := e.CostsConfig()

	// ReferencePrice, not prices.Get: a symbol that warmup seeded from history
	// but whose live feed never ticks has no cached tick, and the engine will
	// still authorise orders for it from the last bar's close. An oracle that
	// could not see that history rejected every one of them with "sim: no price
	// for X", once per evaluation, forever. See CODE_REVIEW_FINDINGS.md's TM-1.
	sim := broker.NewSim(acct.Cash, e.ReferencePrice, costs.Commission)
	sim.SetSlippage(func(_ string, buy bool, price float64) float64 {
		return costs.FillPrice(buy, price)
	})
	sim.SeedPositions(acct.Cash, positions)

	var deadline time.Time
	if dur > 0 {
		deadline = time.Now().Add(dur)
	}

	e.mu.Lock()
	e.testModeRealBrk = realBrk
	e.brk = sim
	e.testModeActive = true
	e.testModeDeadline = deadline
	e.mu.Unlock()

	// Reflect the seeded book immediately rather than waiting for SyncInterval.
	e.syncPortfolio(ctx)

	e.log.Warn("test mode started — all orders are simulated until it ends",
		"seeded_cash", acct.Cash, "seeded_positions", len(positions), "deadline", deadline)
	e.Notify(notify.Event{
		Kind:  notify.KindAudit,
		Time:  time.Now(),
		Title: "test mode started",
		Text: fmt.Sprintf("training window active — seeded with %.2f cash and %d position(s); orders are simulated only",
			acct.Cash, len(positions)),
	})

	return nil
}

// EndTestMode restores the real broker and stops simulating orders. It is a
// no-op if test mode is not active.
func (e *Engine) EndTestMode(reason string) {
	e.mu.Lock()

	if !e.testModeActive {
		e.mu.Unlock()

		return
	}

	realBrk := e.testModeRealBrk
	e.brk = realBrk
	e.testModeActive = false
	e.testModeDeadline = time.Time{}
	e.testModeRealBrk = nil
	e.mu.Unlock()

	// Restore the dashboard/portfolio to the real broker's actual current
	// state — untouched by any simulated order, but prices/time have moved
	// on since the last real sync.
	e.syncPortfolio(context.Background())

	e.log.Warn("test mode ended — orders now reach the real broker", "reason", reason)
	e.Notify(notify.Event{
		Kind:  notify.KindAudit,
		Time:  time.Now(),
		Title: "test mode ended",
		Text:  reason,
	})
}

// checkTestModeExpiry lazily ends test mode once its deadline has passed,
// mirroring internal/protect's own lazy-checked-on-read cooldowns rather than
// running a background timer.
func (e *Engine) checkTestModeExpiry(_ context.Context) {
	e.mu.RLock()
	expired := e.testModeActive && !e.testModeDeadline.IsZero() && time.Now().After(e.testModeDeadline)
	e.mu.RUnlock()

	if expired {
		e.EndTestMode("training window elapsed")
	}
}
