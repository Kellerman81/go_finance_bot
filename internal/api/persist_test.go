package api

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// The settings screen used to apply changes to the running process only: correct
// until the next restart, at which point config.yaml silently won for every
// value. These check that a save reaches the file and that nothing else in it
// moves.

// configFixture writes a small but realistic config and returns its path.
func configFixture(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	const body = `# Live configuration. The comments are half the value of this file.
server:
  auth_token: "keep-me-secret"

exits:
  enabled: true
  stop_loss_pct: 0.10         # sell if price falls 10% below entry
  trailing_stop_pct: 0.08
  take_profit_pct: 0.30
  atr_period: 14
  atr_stop_mult: 0
  atr_trail_mult: 0
  trailing_offset_pct: 0
  trailing_tight_pct: 0
  trailing_only_after_offset: false
  max_hold: 0s

# strategy_overrides:
#   NVDA:
#     min_strength: 0.40
`

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	return path
}

// A settings change must reach the file, so it survives a restart.
func TestSettingsChangeIsWrittenToConfig(t *testing.T) {
	path := configFixture(t)
	s := newTestServer(t)
	s.SetConfigPath(path)

	rec := postForm(t, s, "/v2/form/settings/exits", url.Values{
		"enabled":       {"true"},
		"stop_loss_pct": {"0.05"},
		"max_hold":      {"720h"},
	})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	out := readFile(t, path)

	if !strings.Contains(out, "stop_loss_pct: 0.05") {
		t.Errorf("the change did not reach config.yaml:\n%s", out)
	}
}

// And the rest of the file must be untouched — comments, credentials, and the
// commented-out block a marshal round-trip would have deleted.
func TestSettingsWriteLeavesTheFileIntact(t *testing.T) {
	path := configFixture(t)
	s := newTestServer(t)
	s.SetConfigPath(path)

	postForm(t, s, "/v2/form/settings/exits", url.Values{
		"enabled": {"true"}, "stop_loss_pct": {"0.05"},
	})

	out := readFile(t, path)

	for _, want := range []string{
		"# Live configuration. The comments are half the value of this file.",
		"sell if price falls 10% below entry",
		`auth_token: "keep-me-secret"`,
		"# strategy_overrides:",
		"#     min_strength: 0.40",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("lost from config.yaml: %q", want)
		}
	}
}

// The file must still load, or the bot would fail to start next time.
func TestWrittenConfigStillLoads(t *testing.T) {
	path := configFixture(t)
	s := newTestServer(t)
	s.SetConfigPath(path)

	postForm(t, s, "/v2/form/settings/exits", url.Values{
		"enabled": {"true"}, "stop_loss_pct": {"0.05"}, "max_hold": {"720h"},
	})

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the written config no longer loads: %v", err)
	}

	if cfg.Exits.StopLossPct != 0.05 {
		t.Errorf("stop_loss_pct = %v after reload, want 0.05", cfg.Exits.StopLossPct)
	}
}

// With no path configured the old behaviour stands: applied live, not saved.
// Nothing may be written, and nothing may fail.
func TestNoConfigPathMeansNoWrite(t *testing.T) {
	s := newTestServer(t)

	rec := postForm(t, s, "/v2/form/settings/exits", url.Values{
		"enabled": {"true"}, "stop_loss_pct": {"0.05"},
	})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

// A write that cannot happen must be reported, not swallowed: the change IS
// live, so silently implying it was saved would strand the operator at the next
// restart wondering why their setting reverted.
func TestFailedWriteIsReported(t *testing.T) {
	s := newTestServer(t)
	s.SetConfigPath(filepath.Join(t.TempDir(), "does-not-exist", "config.yaml"))

	rec := postForm(t, s, "/v2/form/settings/exits", url.Values{
		"enabled": {"true"}, "stop_loss_pct": {"0.05"},
	})

	if !strings.Contains(rec.Body.String(), "not saved") {
		t.Errorf("a failed save was not reported to the operator: %q", rec.Body.String())
	}
}

// The engine keeps running the new value even when the file could not be
// written. Reverting the live change would be a worse answer than saying so.
func TestFailedWriteStillAppliesLive(t *testing.T) {
	s := newTestServer(t)
	s.SetConfigPath(filepath.Join(t.TempDir(), "nope", "config.yaml"))

	postForm(t, s, "/v2/form/settings/exits", url.Values{
		"enabled": {"true"}, "stop_loss_pct": {"0.07"},
	})

	if got := s.eng.ExitsConfig().StopLossPct; got != 0.07 {
		t.Errorf("live stop_loss_pct = %v, want 0.07 applied regardless of the file", got)
	}
}

// TestLegacySizingReportsAFailedSaveInTheBody reproduces the "Patch...
// reports this only via a response header" finding for the legacy JSON API:
// PUT /api/sizing and PUT /api/behavior used to attach a failed-save reason
// only to the X-Config-Save-Warning header (via noteSave), invisible to any
// caller — such as a plain JSON client, unlike the v2 form layer — that only
// reads the response body. The change silently reverts on the next restart
// with no indication anything went wrong. noteSave now also returns the
// note so these handlers can fold it into their own body.
func TestLegacySizingReportsAFailedSaveInTheBody(t *testing.T) {
	s := newTestServer(t)
	s.SetConfigPath(filepath.Join(t.TempDir(), "does-not-exist", "config.yaml"))

	code, body := do(t, s, http.MethodPut, "/api/sizing",
		`{"order_size_usd":150,"min_positions":3}`)
	if code != http.StatusOK {
		t.Fatalf("PUT sizing = %d: %s", code, body)
	}

	if !strings.Contains(body, "not saved") {
		t.Errorf("a failed save was not reported in the response body: %s", body)
	}
}

// The same for behavior, and a healthy save must report an empty note rather
// than omitting the field inconsistently.
func TestLegacyBehaviorReportsAFailedSaveInTheBody(t *testing.T) {
	s := newTestServer(t)
	s.SetConfigPath(filepath.Join(t.TempDir(), "does-not-exist", "config.yaml"))

	code, body := do(t, s, http.MethodPut, "/api/behavior",
		`{"keep_interval":"1h","confirm_buy":2,"confirm_sell":2}`)
	if code != http.StatusOK {
		t.Fatalf("PUT behavior = %d: %s", code, body)
	}

	if !strings.Contains(body, "not saved") {
		t.Errorf("a failed save was not reported in the response body: %s", body)
	}
}

func TestLegacySizingReportsAnEmptyNoteOnSuccess(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPut, "/api/sizing",
		`{"order_size_usd":150,"min_positions":3}`)
	if code != http.StatusOK {
		t.Fatalf("PUT sizing = %d: %s", code, body)
	}

	if !strings.Contains(body, `"note":""`) {
		t.Errorf(`a successful save with no config path did not report note:"": %s`, body)
	}
}

// TestPutSizingRoundTripsRiskFields checks the three fields added alongside
// order_size_usd/min_positions: risk_per_trade_pct, max_risk_size_usd and
// min_order_size_usd must actually reach the engine and read back correctly,
// including 0 — unlike order_size_usd/min_positions, 0 is a valid value for
// all three (risk sizing off; no cap; no floor) and must not be silently
// ignored.
func TestPutSizingRoundTripsRiskFields(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPut, "/api/sizing",
		`{"min_positions":3,"max_risk_size_usd":400,"min_order_size_usd":50}`)
	if code != http.StatusOK {
		t.Fatalf("PUT sizing = %d: %s", code, body)
	}

	if minPos, maxRisk, minOrder := s.eng.Sizing(); minPos != 3 || maxRisk != 400 || minOrder != 50 {
		t.Fatalf("after PUT: min_positions/max_risk_size_usd/min_order_size_usd = %v/%v/%v, want 3/400/50",
			minPos, maxRisk, minOrder)
	}

	code, body = do(t, s, http.MethodPut, "/api/sizing",
		`{"min_positions":3,"max_risk_size_usd":0,"min_order_size_usd":0}`)
	if code != http.StatusOK {
		t.Fatalf("PUT sizing (clearing) = %d: %s", code, body)
	}

	if _, maxRisk, minOrder := s.eng.Sizing(); maxRisk != 0 || minOrder != 0 {
		t.Errorf("after clearing: max_risk_size_usd/min_order_size_usd = %v/%v, want 0/0",
			maxRisk, minOrder)
	}
}

// TestPutSizingRejectsANegativeRiskField mirrors the existing
// min_positions < 0 guard for the ceiling/threshold fields.
func TestPutSizingRejectsANegativeRiskField(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPut, "/api/sizing",
		`{"min_positions":3,"max_risk_size_usd":-1,"min_order_size_usd":0}`)
	if code != http.StatusBadRequest {
		t.Fatalf("PUT sizing with a negative max_risk_size_usd = %d, want 400: %s", code, body)
	}

	code, body = do(t, s, http.MethodPut, "/api/sizing",
		`{"min_positions":3,"max_risk_size_usd":0,"min_order_size_usd":-1}`)
	if code != http.StatusBadRequest {
		t.Fatalf("PUT sizing with a negative min_order_size_usd = %d, want 400: %s", code, body)
	}
}

// TestLimitsStrategyAndExitsPersistToDisk reproduces the "PUT /api/limits,
// /api/strategy, /api/exits never persist to disk even though the
// persistence path exists for them" finding: liveSection already supported
// all three section names (the same machinery Sizing/Behavior use), the
// handlers simply never called it — a tightened limit, a retuned strategy,
// or an edited exits ladder applied live but silently reverted on the next
// restart, unlike the sibling forms on the very same settings screen.
func TestLimitsStrategyAndExitsPersistToDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	const body = "limits:\n  max_order_value: 100\n" +
		"strategy:\n  fast_ma: 10\n  slow_ma: 50\n" +
		"exits:\n  enabled: true\n  stop_loss_pct: 0.05\n"

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s := newTestServer(t)
	s.SetConfigPath(path)

	if code, resp := do(t, s, http.MethodPut, "/api/limits",
		`{"max_order_value":500,"max_total_invested":0,"max_per_position":0,`+
			`"max_daily_spend":0,"cash_reserve":0,"max_open_positions":0}`,
	); code != http.StatusOK {
		t.Fatalf("PUT limits = %d: %s", code, resp)
	}

	if code, resp := do(t, s, http.MethodPut, "/api/strategy",
		`{"fast_ma":12,"slow_ma":55}`,
	); code != http.StatusOK {
		t.Fatalf("PUT strategy = %d: %s", code, resp)
	}

	if code, resp := do(t, s, http.MethodPut, "/api/exits",
		`{"enabled":true,"stop_loss_pct":0.08}`,
	); code != http.StatusOK {
		t.Fatalf("PUT exits = %d: %s", code, resp)
	}

	out := readFile(t, path)

	for _, want := range []string{
		"max_order_value: 500",
		"fast_ma: 12",
		"slow_ma: 55",
		"stop_loss_pct: 0.08",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("config.yaml is missing %q after the save — the section was applied "+
				"live but never persisted:\n%s", want, out)
		}
	}
}

// The four forms that write into the shared engine: block must land there, not
// in a block named after the form.
func TestSizingWritesIntoTheEngineBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	const body = "engine:\n  min_positions: 4    # spread the budget over this many\n  min_order_size_usd: 10\n"

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s := newTestServerFrom(t, nil, config.Default())
	s.SetConfigPath(path)

	rec := postForm(t, s, "/v2/form/sizing", url.Values{
		"min_positions": {"3"}, "min_order_size_usd": {"25"},
	})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	out := readFile(t, path)

	if !strings.Contains(out, "min_positions: 3") {
		t.Errorf("the sizing form did not write into engine::\n%s", out)
	}

	if !strings.Contains(out, "spread the budget") {
		t.Error("the trailing comment was lost")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	return string(b)
}
