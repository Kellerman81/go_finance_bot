package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/autotune"
	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// The per-symbol optimizer's search/scoring logic now lives in
// internal/autotune, shared with the scheduled auto-optimizer, and is
// covered there (baseline/proposal shape, the dual-baseline gate,
// non-overridable-field disclosure, real-vs-what-if windowing). What
// remains API-level is exactly the HTTP wrapping: routing, the shared
// busy-semaphore surfacing as a friendly message, and the template
// executing without error against a real Result.

func TestSymbolOptimizeRejectsAnUnknownSymbol(t *testing.T) {
	s := newTestServer(t)

	rec := fetch(t, s, "/v2/f/symbolOptimize?symbol=NOPE")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	if !strings.Contains(rec.Body.String(), "not on the watchlist") {
		t.Errorf("expected the unknown-symbol message, got: %s", rec.Body.String())
	}
}

// TestSymbolOptimizeReturnsBusyWhenARunIsAlreadyInFlight mirrors
// TestOptimizeFieldReturnsBusyWhenASweepIsAlreadyRunning: a second request
// arriving while a search is already running — whether that's another
// manual click or a scheduled autotune pass, since autotune.TryAcquire is
// the single semaphore both share — must fail fast, not start a concurrent
// search or queue indefinitely.
func TestSymbolOptimizeReturnsBusyWhenARunIsAlreadyInFlight(t *testing.T) {
	s := newTestServer(t)

	if err := s.eng.AddSymbols("AAPL"); err != nil {
		t.Fatalf("AddSymbols: %v", err)
	}

	acquireSlot(t)
	defer autotune.Release()

	rec := fetch(t, s, "/v2/f/symbolOptimize?symbol=AAPL")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	if !strings.Contains(rec.Body.String(), "Another sweep is already running") {
		t.Errorf("expected the busy message, got: %s", rec.Body.String())
	}
}

// TestSymbolOptimizeHTTPRendersWithoutATemplateError drives the full HTTP
// path so the "symbolOptimize" template actually executes against a
// populated, "Improved" Result — including the NotOverridable branch's csv
// template func — at least once. go vet/build only catch a template's
// Go-side field-name typos, never a template-execution-time error (a type
// html/template funcs aren't checked against until Execute runs), so this
// is the only thing that would catch one here.
func TestSymbolOptimizeHTTPRendersWithoutATemplateError(t *testing.T) {
	s := newTestServer(t)

	if err := s.eng.AddSymbols("AAPL"); err != nil {
		t.Fatalf("AddSymbols: %v", err)
	}

	strat := config.Default().Strategy
	strat.Detectors = []string{"ema_cross", "rsi", "macd"}
	s.eng.SetStrategyConfig(strat)

	rec := fetch(t, s, "/v2/f/symbolOptimize?symbol=AAPL")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if strings.Contains(body, "template:") || strings.Contains(body, "executing \"") {
		t.Errorf("response looks like a template execution error: %s", body)
	}
}

// TestSymbolOptimizeUsesTheSubmittedObjectiveAndMinReturn checks the manual
// page's new Objective/Minimum-return controls actually reach
// autotune.RunSymbolOptimize rather than being stuck on the old hardcoded
// defaults — mirrors the same fields the scheduled auto-optimizer's
// settings form already exposes (internal/api/formfields.go's "autotune"
// section), so an operator exploring a symbol by hand can try the same
// objectives and loss floor a scheduled pass would use.
func TestSymbolOptimizeUsesTheSubmittedObjectiveAndMinReturn(t *testing.T) {
	s := newTestServer(t)

	if err := s.eng.AddSymbols("AAPL"); err != nil {
		t.Fatalf("AddSymbols: %v", err)
	}

	strat := config.Default().Strategy
	strat.Detectors = []string{"ema_cross", "rsi", "macd"}
	s.eng.SetStrategyConfig(strat)

	// A recognized objective is echoed back as the selected option.
	rec := fetch(t, s, "/v2/f/symbolOptimize?symbol=AAPL&objective=return")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	if !strings.Contains(rec.Body.String(), `value="return" selected`) {
		t.Errorf("submitted objective %q not reflected as selected: %s", "return", rec.Body.String())
	}

	// An unreachable minimum-return floor must sink Improved regardless of
	// what the search finds — the same MinReturnPct gate autotune.Schedule
	// itself is subject to (see beatsBothBaselines).
	rec = fetch(t, s, "/v2/f/symbolOptimize?symbol=AAPL&min_return_pct=100000")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	if strings.Contains(rec.Body.String(), `class="pill on"`) {
		t.Errorf("an unreachable min_return_pct still produced a \"best\" proposal: %s", rec.Body.String())
	}

	// A malformed value degrades to the default rather than erroring.
	rec = fetch(t, s, "/v2/f/symbolOptimize?symbol=AAPL&min_return_pct=not-a-number")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if strings.Contains(body, "template:") || strings.Contains(body, "executing \"") {
		t.Errorf("a malformed min_return_pct produced a template error instead of falling back: %s", body)
	}
}

func TestSymbolOptimizeWithNoSelectionJustShowsTheForm(t *testing.T) {
	s := newTestServer(t)

	rec := fetch(t, s, "/v2/f/symbolOptimize")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	if strings.Contains(rec.Body.String(), "opt.") {
		t.Errorf("an untranslated key leaked into the response: %s", rec.Body.String())
	}
}
