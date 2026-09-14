package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// The Watchlist tab's per-symbol "exclude from autotune" / "include in
// autotune" button (POST /api/autotune/exclude/:symbol) — a shortcut for
// editing AutotuneConfig.ExcludeSymbols by hand on the Settings tab.

// TestSetAutotuneExclusionAddsAndRemoves checks the toggle both ways,
// live: excluding a symbol adds it to ExcludeSymbols, including it again
// removes it — case-insensitively, matching every other symbol comparison
// in this codebase.
func TestSetAutotuneExclusionAddsAndRemoves(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPost, "/api/autotune/exclude/aapl", `{"excluded":true}`)
	if code != http.StatusOK {
		t.Fatalf("exclude = %d: %s", code, body)
	}

	got := s.eng.AutotuneConfig().ExcludeSymbols
	if len(got) != 1 || got[0] != "AAPL" {
		t.Fatalf("ExcludeSymbols = %v, want [AAPL]", got)
	}

	// Excluding an already-excluded symbol again must not duplicate it.
	code, body = do(t, s, http.MethodPost, "/api/autotune/exclude/AAPL", `{"excluded":true}`)
	if code != http.StatusOK {
		t.Fatalf("re-exclude = %d: %s", code, body)
	}

	if got := s.eng.AutotuneConfig().ExcludeSymbols; len(got) != 1 {
		t.Fatalf("ExcludeSymbols = %v, want still just [AAPL], not duplicated", got)
	}

	code, body = do(t, s, http.MethodPost, "/api/autotune/exclude/AAPL", `{"excluded":false}`)
	if code != http.StatusOK {
		t.Fatalf("include = %d: %s", code, body)
	}

	if got := s.eng.AutotuneConfig().ExcludeSymbols; len(got) != 0 {
		t.Fatalf("ExcludeSymbols = %v, want empty after including it again", got)
	}
}

// TestSetAutotuneExclusionRejectsOverlapWithRunOnly checks the handler
// refuses to write a config that would fail to reload — AutotuneConfig's
// own Validate rejects a symbol named in both ExcludeSymbols and
// RunOnlyForSymbols while enabled, so excluding a symbol the operator has
// ALSO restricted the scheduler to (via Settings) must be rejected here
// too, rather than silently producing a config.yaml that breaks the next
// restart.
func TestSetAutotuneExclusionRejectsOverlapWithRunOnly(t *testing.T) {
	s := newTestServer(t)

	s.eng.SetAutotuneConfig(config.AutotuneConfig{
		Enabled: true, Interval: 24 * time.Hour,
		RunOnlyForSymbols: []string{"AAPL"},
	})

	code, body := do(t, s, http.MethodPost, "/api/autotune/exclude/AAPL", `{"excluded":true}`)
	if code != http.StatusBadRequest {
		t.Fatalf("excluding a run_only_for_symbols entry while enabled was accepted: %d (%s)", code, body)
	}

	if got := s.eng.AutotuneConfig().ExcludeSymbols; len(got) != 0 {
		t.Fatalf("ExcludeSymbols = %v, want unchanged after a rejected write", got)
	}
}

// TestSetAutotuneExclusionPersistsToConfig mirrors
// TestSettingsChangeIsWrittenToConfig: the toggle must reach config.yaml,
// not just the running process, or the exclusion silently reverts on the
// next restart.
func TestSetAutotuneExclusionPersistsToConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	const body = `# Live configuration.
autotune:
  enabled: false
  interval: 24h
  bars: 1440
  min_trades: 10
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s := newTestServer(t)
	s.SetConfigPath(path)

	// Mirrors cmd/bot/main.go's own startup wiring (eng.SetAutotuneConfig
	// from the just-loaded file) — without this, the engine's autotuneCfg
	// starts at its zero value, and persisting it would overwrite the
	// fixture's real interval/bars/min_trades with zeros alongside the
	// intended exclude_symbols change.
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("seed config.Load: %v", err)
	}

	s.eng.SetAutotuneConfig(loaded.Autotune)

	code, respBody := do(t, s, http.MethodPost, "/api/autotune/exclude/MSFT", `{"excluded":true}`)
	if code != http.StatusOK {
		t.Fatalf("exclude = %d: %s", code, respBody)
	}

	if strings.Contains(respBody, "not saved") {
		t.Fatalf("exclude reported a save failure: %s", respBody)
	}

	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(saved), "MSFT") {
		t.Errorf("config.yaml = %q, want MSFT written into exclude_symbols", saved)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load after save: %v", err)
	}

	if len(cfg.Autotune.ExcludeSymbols) != 1 || cfg.Autotune.ExcludeSymbols[0] != "MSFT" {
		t.Errorf("reloaded ExcludeSymbols = %v, want [MSFT]", cfg.Autotune.ExcludeSymbols)
	}
}

// TestWatchlistFragmentShowsAutotuneExclusionState checks the row rendering:
// an excluded symbol gets the tag and the "include" button; everyone else
// gets the "exclude" button and no tag.
func TestWatchlistFragmentShowsAutotuneExclusionState(t *testing.T) {
	s := newTestServer(t)

	if err := s.eng.AddSymbols("AAPL", "MSFT"); err != nil {
		t.Fatalf("AddSymbols: %v", err)
	}

	s.eng.SetAutotuneConfig(config.AutotuneConfig{ExcludeSymbols: []string{"AAPL"}})

	rec := fetch(t, s, "/v2/f/watchlist")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()

	if !strings.Contains(body, `/api/autotune/exclude/AAPL" data-body='{"excluded":false}'`) {
		t.Errorf("AAPL row missing the include-in-autotune button (excluded:false to flip back): %s", body)
	}

	if !strings.Contains(body, `/api/autotune/exclude/MSFT" data-body='{"excluded":true}'`) {
		t.Errorf("MSFT row missing the exclude-from-autotune button (excluded:true to flip): %s", body)
	}
}

// The Watchlist tab's per-symbol "restrict autotune to only this" /
// "remove autotune restriction" button (POST /api/autotune/run-only/:symbol)
// — AutotuneConfig.RunOnlyForSymbols is an ALLOW-list, so unlike
// ExcludeSymbols, populating it affects every OTHER watchlist symbol too;
// these tests cover that alongside the ordinary add/remove mechanics.

// TestSetAutotuneRunOnlyAddsAndRemoves mirrors
// TestSetAutotuneExclusionAddsAndRemoves for the run-only list.
func TestSetAutotuneRunOnlyAddsAndRemoves(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPost, "/api/autotune/run-only/aapl", `{"included":true}`)
	if code != http.StatusOK {
		t.Fatalf("include = %d: %s", code, body)
	}

	got := s.eng.AutotuneConfig().RunOnlyForSymbols
	if len(got) != 1 || got[0] != "AAPL" {
		t.Fatalf("RunOnlyForSymbols = %v, want [AAPL]", got)
	}

	// Restricting to an already-restricted symbol again must not duplicate it.
	code, body = do(t, s, http.MethodPost, "/api/autotune/run-only/AAPL", `{"included":true}`)
	if code != http.StatusOK {
		t.Fatalf("re-include = %d: %s", code, body)
	}

	if got := s.eng.AutotuneConfig().RunOnlyForSymbols; len(got) != 1 {
		t.Fatalf("RunOnlyForSymbols = %v, want still just [AAPL], not duplicated", got)
	}

	code, body = do(t, s, http.MethodPost, "/api/autotune/run-only/AAPL", `{"included":false}`)
	if code != http.StatusOK {
		t.Fatalf("remove restriction = %d: %s", code, body)
	}

	if got := s.eng.AutotuneConfig().RunOnlyForSymbols; len(got) != 0 {
		t.Fatalf("RunOnlyForSymbols = %v, want empty after removing the restriction", got)
	}
}

// TestSetAutotuneRunOnlyReportsCount checks the response discloses how many
// symbols are now on the allow-list, since adding even one changes every
// OTHER symbol's eligibility too — a client needs this to warn about that
// wider effect rather than presenting the action as a harmless per-row flip.
func TestSetAutotuneRunOnlyReportsCount(t *testing.T) {
	s := newTestServer(t)

	_, body := do(t, s, http.MethodPost, "/api/autotune/run-only/AAPL", `{"included":true}`)
	if !strings.Contains(body, `"run_only_count":1`) {
		t.Errorf("body = %s, want run_only_count:1 after adding the first symbol", body)
	}

	_, body = do(t, s, http.MethodPost, "/api/autotune/run-only/MSFT", `{"included":true}`)
	if !strings.Contains(body, `"run_only_count":2`) {
		t.Errorf("body = %s, want run_only_count:2 after adding a second symbol", body)
	}
}

// TestSetAutotuneRunOnlyRejectsOverlapWithExclude mirrors
// TestSetAutotuneExclusionRejectsOverlapWithRunOnly from the other
// direction: restricting the scheduler to a symbol that is ALSO on
// ExcludeSymbols while enabled must be rejected, not silently written.
func TestSetAutotuneRunOnlyRejectsOverlapWithExclude(t *testing.T) {
	s := newTestServer(t)

	s.eng.SetAutotuneConfig(config.AutotuneConfig{
		Enabled: true, Interval: 24 * time.Hour,
		ExcludeSymbols: []string{"AAPL"},
	})

	code, body := do(t, s, http.MethodPost, "/api/autotune/run-only/AAPL", `{"included":true}`)
	if code != http.StatusBadRequest {
		t.Fatalf("restricting to an excluded symbol while enabled was accepted: %d (%s)", code, body)
	}

	if got := s.eng.AutotuneConfig().RunOnlyForSymbols; len(got) != 0 {
		t.Fatalf("RunOnlyForSymbols = %v, want unchanged after a rejected write", got)
	}
}

// TestSetAutotuneRunOnlyPersistsToConfig mirrors
// TestSetAutotuneExclusionPersistsToConfig.
func TestSetAutotuneRunOnlyPersistsToConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	const body = `# Live configuration.
autotune:
  enabled: false
  interval: 24h
  bars: 1440
  min_trades: 10
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s := newTestServer(t)
	s.SetConfigPath(path)

	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("seed config.Load: %v", err)
	}

	s.eng.SetAutotuneConfig(loaded.Autotune)

	code, respBody := do(t, s, http.MethodPost, "/api/autotune/run-only/MSFT", `{"included":true}`)
	if code != http.StatusOK {
		t.Fatalf("include = %d: %s", code, respBody)
	}

	if strings.Contains(respBody, "not saved") {
		t.Fatalf("include reported a save failure: %s", respBody)
	}

	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(saved), "run_only_for_symbols: [MSFT]") {
		t.Errorf("config.yaml = %q, want run_only_for_symbols: [MSFT]", saved)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load after save: %v", err)
	}

	if len(cfg.Autotune.RunOnlyForSymbols) != 1 || cfg.Autotune.RunOnlyForSymbols[0] != "MSFT" {
		t.Errorf("reloaded RunOnlyForSymbols = %v, want [MSFT]", cfg.Autotune.RunOnlyForSymbols)
	}
}

// TestWatchlistFragmentShowsAutotuneRunOnlyState mirrors
// TestWatchlistFragmentShowsAutotuneExclusionState, and additionally checks
// the page-level banner that discloses the allow-list's wider effect.
func TestWatchlistFragmentShowsAutotuneRunOnlyState(t *testing.T) {
	s := newTestServer(t)

	if err := s.eng.AddSymbols("AAPL", "MSFT"); err != nil {
		t.Fatalf("AddSymbols: %v", err)
	}

	s.eng.SetAutotuneConfig(config.AutotuneConfig{RunOnlyForSymbols: []string{"AAPL"}})

	rec := fetch(t, s, "/v2/f/watchlist")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()

	if !strings.Contains(body, `/api/autotune/run-only/AAPL" data-body='{"included":false}'`) {
		t.Errorf("AAPL row missing the remove-restriction button (included:false to flip back): %s", body)
	}

	if !strings.Contains(body, `/api/autotune/run-only/MSFT" data-body='{"included":true}'`) {
		t.Errorf("MSFT row missing the restrict-to-only-this button (included:true to flip): %s", body)
	}

	if !strings.Contains(body, "restricted to only: AAPL") {
		t.Errorf("no page-level banner disclosing the active run-only restriction: %s", body)
	}
}

// TestWatchlistFragmentHasNoRunOnlyBannerWhenInactive checks the banner
// stays absent in the common case (empty RunOnlyForSymbols), so it doesn't
// clutter every watchlist view for a feature almost nobody has turned on.
func TestWatchlistFragmentHasNoRunOnlyBannerWhenInactive(t *testing.T) {
	s := newTestServer(t)

	if err := s.eng.AddSymbols("AAPL"); err != nil {
		t.Fatalf("AddSymbols: %v", err)
	}

	rec := fetch(t, s, "/v2/f/watchlist")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	if strings.Contains(rec.Body.String(), "restricted") {
		t.Errorf("run-only banner shown despite an empty RunOnlyForSymbols: %s", rec.Body.String())
	}
}
