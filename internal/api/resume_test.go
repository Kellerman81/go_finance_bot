package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// TestResumeProtectionsAlsoReEnablesTrading reproduces the "click twice"
// friction: a protections halt always turns trading off itself
// (Engine.haltFromProtections), so resuming the halt must also turn trading
// back on in one action, rather than leaving the operator to separately find
// and click the trading toggle too.
func TestResumeProtectionsAlsoReEnablesTrading(t *testing.T) {
	s := newTestServer(t)
	s.rm.SetLimits(config.Limits{MaxOrderValue: 1000, MaxTotalInvested: 10000}) // trading needs ceilings

	s.eng.SetProtectionsConfig(config.ProtectionsConfig{Enabled: true, MaxDrawdownPct: 0.10})

	p := s.eng.Protections()
	p.ObserveEquity(100000)

	if reason := p.ObserveEquity(80000); reason == "" {
		t.Fatal("want a halt at 20% drawdown")
	}

	if s.eng.Status().TradingEnabled {
		t.Fatal("trading was not disabled by the halt")
	}

	code, body := do(t, s, http.MethodPost, "/api/protections/resume", "")
	if code != http.StatusOK {
		t.Fatalf("POST /api/protections/resume = %d: %s", code, body)
	}

	if halted, _ := p.Halted(); halted {
		t.Fatal("still halted after resume")
	}

	if !s.eng.Status().TradingEnabled {
		t.Error("trading is still disabled after resuming the halt that caused it")
	}
}

// TestResumeProtectionsWithNoActiveHaltDoesNotForceTradingOn checks the fix
// above is scoped to an actual halt: calling resume when nothing is halted
// (e.g. a defensive/idempotent call) must not flip a deliberately-disabled
// trading toggle back on as a side effect.
//
// AP3-6: nor may it touch the protections at all. Resume re-bases the
// drawdown peak, the day's equity and the realized peak; called with nothing
// halted — a stale tab, a double click — it forgave a drawdown in progress
// (9% down moved a 10% halt to 18% below the real peak). It answers 409.
func TestResumeProtectionsWithNoActiveHaltDoesNotForceTradingOn(t *testing.T) {
	s := newTestServer(t)

	s.eng.SetProtectionsConfig(config.ProtectionsConfig{Enabled: true, MaxDrawdownPct: 0.10})
	s.eng.SetTradingEnabled(false) // a deliberate, unrelated manual toggle-off

	code, body := do(t, s, http.MethodPost, "/api/protections/resume", "")
	if code != http.StatusConflict {
		t.Fatalf("POST /api/protections/resume with nothing halted = %d: %s, want 409 and no re-base",
			code, body)
	}

	if s.eng.Status().TradingEnabled {
		t.Error("resume with nothing halted turned trading back on")
	}
}

// TestDiagnosticsKeepsTheResumeButtonWhenProtectionsAreSwitchedOff guards the
// second half of the protect.Status().Enabled fix.
//
// Status used to hardcode Enabled: true for any live manager, and the
// diagnostics panel was gated on that field — so making the field honest would
// have hidden the whole panel for a manager that is switched off but still
// HOLDING a halt. That state is deliberately preserved for a re-enable
// (protect.SetConfig), and this panel's Resume button is the only way to clear
// it: hiding it would strand an operator with a halt they can see in the pill
// and cannot reach.
func TestDiagnosticsKeepsTheResumeButtonWhenProtectionsAreSwitchedOff(t *testing.T) {
	s := newTestServer(t)

	s.eng.SetProtectionsConfig(config.ProtectionsConfig{Enabled: true, MaxDrawdownPct: 0.10})

	p := s.eng.Protections()
	p.ObserveEquity(100000)

	if reason := p.ObserveEquity(80000); reason == "" {
		t.Fatal("want a halt at 20% drawdown")
	}

	// Switch protections off. The manager stays, carrying the halt.
	s.eng.SetProtectionsConfig(config.ProtectionsConfig{Enabled: false, MaxDrawdownPct: 0.10})

	if enabled := p.Status().Enabled; enabled {
		t.Fatal("Status().Enabled is still true after switching protections off")
	}

	if halted, _ := p.Halted(); !halted {
		t.Fatal("the halt was not preserved; this test no longer pins anything")
	}

	rec := fetch(t, s, "/v2/f/diagnostics")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v2/f/diagnostics = %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if !strings.Contains(body, "/api/protections/resume") {
		t.Error("the Resume button is gone while a halt is still in force — " +
			"nothing in the UI can clear it now")
	}

	if !strings.Contains(body, catalogs["en"]["diag.disabled"]) {
		t.Errorf("panel does not say protections are switched off (want %q)",
			catalogs["en"]["diag.disabled"])
	}
}

// TestResumeDoesNotEnableTradingWithoutCeilings is the regression for AP2-5.
// The trading toggle refuses to switch on without money ceilings
// (ValidateForTrading); Resume switched it on with no check at all, so
// ceilings zeroed while the halt had trading off went live unbounded. The
// halt is still cleared — trading stays off, and the response says why.
func TestResumeDoesNotEnableTradingWithoutCeilings(t *testing.T) {
	s := newTestServer(t)
	s.rm.SetLimits(config.Limits{}) // zeroed during the halt

	s.eng.SetProtectionsConfig(config.ProtectionsConfig{Enabled: true, MaxDrawdownPct: 0.10})

	p := s.eng.Protections()
	p.ObserveEquity(100000)
	p.ObserveEquity(80000)

	code, body := do(t, s, http.MethodPost, "/api/protections/resume", "")
	if code != http.StatusOK {
		t.Fatalf("resume = %d: %s", code, body)
	}

	if halted, _ := p.Halted(); halted {
		t.Error("the halt was not cleared")
	}

	if s.eng.Status().TradingEnabled {
		t.Error("resume switched trading on with no money ceilings")
	}

	if !strings.Contains(body, "trading_not_resumed") {
		t.Errorf("the response does not say why trading stayed off: %s", body)
	}
}

// The deliberate re-base the README advises after a withdrawal still works
// without a halt, when asked for by name — and leaves trading as it was.
func TestAnExplicitRebaseWorksWithoutAHalt(t *testing.T) {
	s := newTestServer(t)

	s.eng.SetProtectionsConfig(config.ProtectionsConfig{Enabled: true, MaxDrawdownPct: 0.10})
	s.eng.SetTradingEnabled(false)

	if code, body := do(t, s, http.MethodPost, "/api/protections/resume?rebase=1", ""); code != http.StatusOK {
		t.Fatalf("POST resume?rebase=1 = %d: %s", code, body)
	}

	if s.eng.Status().TradingEnabled {
		t.Error("a re-base with nothing halted turned trading on")
	}
}
