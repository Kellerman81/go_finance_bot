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
func TestResumeProtectionsWithNoActiveHaltDoesNotForceTradingOn(t *testing.T) {
	s := newTestServer(t)

	s.eng.SetProtectionsConfig(config.ProtectionsConfig{Enabled: true, MaxDrawdownPct: 0.10})
	s.eng.SetTradingEnabled(false) // a deliberate, unrelated manual toggle-off

	code, body := do(t, s, http.MethodPost, "/api/protections/resume", "")
	if code != http.StatusOK {
		t.Fatalf("POST /api/protections/resume = %d: %s", code, body)
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
