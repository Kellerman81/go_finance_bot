package api

import (
	"regexp"
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// inputValue extracts the value= attribute of the "f-<prefix>-<name>" input
// or select from a rendered fragment, or fails the test if it isn't found.
func inputValue(t *testing.T, body, prefix, name string) string {
	t.Helper()

	id := "f-" + prefix + "-" + name
	re := regexp.MustCompile(`(?s)id="` + regexp.QuoteMeta(id) + `".*?value="([^"]*)"`)

	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no %s element found in body:\n%s", id, body)
	}

	return m[1]
}

// selectedOption reports whether "f-<prefix>-<name>"'s <select> has the given
// option marked selected.
func selectedOption(t *testing.T, body, prefix, name, option string) bool {
	t.Helper()

	id := "f-" + prefix + "-" + name

	block := regexp.MustCompile(`(?s)id="` + regexp.QuoteMeta(id) + `".*?</select>`).
		FindString(body)
	if block == "" {
		t.Fatalf("no %s select found in body:\n%s", id, body)
	}

	return regexp.MustCompile(`value="` + regexp.QuoteMeta(option) + `"\s+selected`).MatchString(block)
}

// TestStrategyFragmentShowsTheLiveConfigValues reproduces the "/v2 strategy
// page fields render empty" finding: fragStrategy builds its Values map by
// JSON-marshalling config.StrategyConfig, whose json tags are snake_case
// ("fast_ma"), and hands it to the same "form" partial every settings block
// uses, keyed by strategyFields' declared Name. If those names don't match
// the struct's json tags exactly, lookupPath finds nothing and every input
// renders empty — invisibly, since the page still loads with status 200 and
// correctly labelled, just blank, fields.
func TestStrategyFragmentShowsTheLiveConfigValues(t *testing.T) {
	s := newTestServer(t)

	strat := config.Default().Strategy
	strat.Detectors = []string{"rsi", "macd"}
	strat.Combine = "weighted"
	strat.MinStrength = 0.37
	strat.FastMA = 13
	strat.SlowMA = 41
	strat.TrendGate = true
	strat.DenyGate = true
	strat.DenyBreakoutPeriod = 15
	strat.DenyBreakoutRVOLThreshold = 1.3
	strat.DenyReversalStochPeriod = 0
	s.eng.SetStrategyConfig(strat)

	rec := fetch(t, s, "/v2/f/strategy")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()

	if got := inputValue(t, body, "strategy", "detectors"); got != "rsi, macd" {
		t.Errorf("detectors input = %q, want %q", got, "rsi, macd")
	}

	if got := inputValue(t, body, "strategy", "min_strength"); got != "0.37" {
		t.Errorf("min_strength input = %q, want %q", got, "0.37")
	}

	if got := inputValue(t, body, "strategy", "fast_ma"); got != "13" {
		t.Errorf("fast_ma input = %q, want %q", got, "13")
	}

	if got := inputValue(t, body, "strategy", "slow_ma"); got != "41" {
		t.Errorf("slow_ma input = %q, want %q", got, "41")
	}

	if !selectedOption(t, body, "strategy", "combine", "weighted") {
		t.Error("combine select does not have weighted selected")
	}

	if !selectedOption(t, body, "strategy", "trend_gate", "true") {
		t.Error("trend_gate select does not have true selected")
	}

	if !selectedOption(t, body, "strategy", "deny_gate", "true") {
		t.Error("deny_gate select does not have true selected")
	}

	if got := inputValue(t, body, "strategy", "deny_breakout_period"); got != "15" {
		t.Errorf("deny_breakout_period input = %q, want %q", got, "15")
	}

	if got := inputValue(t, body, "strategy", "deny_breakout_rvol_threshold"); got != "1.3" {
		t.Errorf("deny_breakout_rvol_threshold input = %q, want %q", got, "1.3")
	}
}

// TestPutStrategyRoundTripsThroughTheV2Form reproduces the save side of the
// same finding: the v2 form posts field values under the names declared in
// strategyFields, which decodeForm turns straight into the JSON body
// putStrategy binds into config.StrategyConfig. If a declared name doesn't
// match the struct's own json tag, that field silently keeps its zero value
// instead of the submission — a save through the UI would quietly wipe the
// live strategy to (mostly) zeroes rather than applying what was typed.
func TestPutStrategyRoundTripsThroughTheV2Form(t *testing.T) {
	s := newTestServer(t)

	rec := postForm(t, s, "/v2/form/strategy", map[string][]string{
		"detectors":                    {"rsi,macd"},
		"combine":                      {"weighted"},
		"min_strength":                 {"0.37"},
		"fast_ma":                      {"13"},
		"slow_ma":                      {"41"},
		"trend_gate":                   {"true"},
		"deny_gate":                    {"true"},
		"deny_breakout_period":         {"15"},
		"deny_breakout_rvol_threshold": {"1.3"},
	})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	got := s.eng.StrategyConfig()
	if got.FastMA != 13 || got.SlowMA != 41 {
		t.Errorf("FastMA/SlowMA = %d/%d, want 13/41 — the submission was not applied",
			got.FastMA, got.SlowMA)
	}

	if got.Combine != "weighted" {
		t.Errorf("Combine = %q, want %q", got.Combine, "weighted")
	}

	if got.MinStrength != 0.37 {
		t.Errorf("MinStrength = %v, want 0.37", got.MinStrength)
	}

	if len(got.Detectors) != 2 || got.Detectors[0] != "rsi" || got.Detectors[1] != "macd" {
		t.Errorf("Detectors = %v, want [rsi macd]", got.Detectors)
	}

	if !got.DenyGate {
		t.Error("DenyGate = false, want true — the submission was not applied")
	}

	if got.DenyBreakoutPeriod != 15 {
		t.Errorf("DenyBreakoutPeriod = %d, want 15", got.DenyBreakoutPeriod)
	}

	if got.DenyBreakoutRVOLThreshold != 1.3 {
		t.Errorf("DenyBreakoutRVOLThreshold = %v, want 1.3", got.DenyBreakoutRVOLThreshold)
	}
}

// TestStrategyFragmentShowsNoOverridesByDefault covers the empty state: a
// server with no per-symbol strategy_overrides configured must render the
// "no overrides" message, not an empty table.
func TestStrategyFragmentShowsNoOverridesByDefault(t *testing.T) {
	s := newTestServer(t)

	rec := fetch(t, s, "/v2/f/strategy")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if !strings.Contains(body, "own strategy parameters") {
		t.Errorf("expected the no-overrides empty state, got:\n%s", body)
	}
}

// TestStrategyFragmentListsPerSymbolOverrides reproduces the read-only
// per-symbol override panel: a symbol with its own strategy_overrides entry
// must appear in the list, reduced to only the fields that actually differ
// from the shared config — not the whole (mostly-inherited) struct.
func TestStrategyFragmentListsPerSymbolOverrides(t *testing.T) {
	s := newTestServer(t)

	base := s.eng.StrategyConfig()

	goog := base
	goog.Detectors = []string{"ema_cross", "rsi", "macd"}
	goog.Combine = "unanimous"
	goog.MinStrength = 0.20

	s.eng.SetSymbolStrategies(map[string]config.StrategyConfig{"GOOG": goog})

	rec := fetch(t, s, "/v2/f/strategy")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()

	if !strings.Contains(body, "GOOG") {
		t.Errorf("expected GOOG to be listed as an overridden symbol, got:\n%s", body)
	}

	if !strings.Contains(body, "unanimous") {
		t.Errorf("expected the overridden combine mode to appear in the diff, got:\n%s", body)
	}

	// fast_ma was NOT overridden (goog inherits it from base) — it must not
	// show up as a spurious diff entry.
	if strings.Contains(body, "fast_ma:") {
		t.Errorf("fast_ma was not overridden and should not appear in the diff, got:\n%s", body)
	}
}
