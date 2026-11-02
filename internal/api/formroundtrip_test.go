package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// formOf renders values the way the settings view does and returns exactly
// what the browser would post back for those fields.
func formOf(values map[string]any, fields []field) url.Values {
	v := url.Values{}
	for _, f := range fields {
		v.Set(f.Name, formValue(values, f))
	}

	return v
}

// mustSectionFields returns a settings section's form fields.
func mustSectionFields(t *testing.T, name string) []field {
	t.Helper()

	fields, ok := sectionFields(name)
	if !ok {
		t.Fatalf("no settings section %q", name)
	}

	return fields
}

// TestExitsFormKeepsBypassTradingHalt is the regression for AP2-1. exitsWire
// restated ExitsConfig field by field and never gained bypass_trading_halt
// when the setting and its form field were added: the form always rendered
// "No", choosing "Yes" was ignored, and every exits save switched it off in
// the live engine AND in config.yaml.
func TestExitsFormKeepsBypassTradingHalt(t *testing.T) {
	src, err := os.ReadFile("../../data/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "c.yaml")
	txt := strings.Replace(string(src), "bypass_trading_halt: false", "bypass_trading_halt: true", 1)

	if err := os.WriteFile(path, []byte(txt), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t)
	s.eng.SetExitsConfig(cfg.Exits)
	s.SetConfigPath(path)

	exits, _ := getSettingsJSON(t, s)["exits"].(map[string]any)
	form := formOf(exits, mustSectionFields(t, "exits"))

	if got := form.Get("bypass_trading_halt"); got != "true" {
		t.Errorf("the form renders bypass_trading_halt as %q for a config that has it on", got)
	}

	if rec := postForm(t, s, "/v2/form/settings/exits", form); rec.Code >= 400 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}

	if !s.eng.ExitsConfig().BypassTradingHalt {
		t.Error("saving the exits form switched bypass_trading_halt off in the running engine")
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if !reloaded.Exits.BypassTradingHalt {
		t.Error("saving the exits form wrote bypass_trading_halt: false to config.yaml")
	}
}

// TestStrategyFormKeepsTheHigherTimeframeSettings is the regression for AP2-2.
// The strategy form declares no htf_* fields and putStrategy bound the request
// into an EMPTY StrategyConfig, so saving the strategy page — even unchanged —
// reset htf_multiple, htf_detectors and htf_mode to zero: the higher-timeframe
// gate switched off, live and in the file.
func TestStrategyFormKeepsTheHigherTimeframeSettings(t *testing.T) {
	s := newTestServer(t)

	cfg := s.eng.StrategyConfig()
	cfg.Detectors = []string{"ema_cross"}
	cfg.FastMA, cfg.SlowMA = 5, 20
	cfg.HTFMultiple, cfg.HTFDetectors, cfg.HTFMode = 15, []string{"trend"}, "gate"
	s.eng.SetStrategyConfig(cfg)

	if rec := postForm(t, s, "/v2/form/strategy", formOf(asMap(s.eng.StrategyConfig()), strategyFields)); rec.Code >= 400 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}

	got := s.eng.StrategyConfig()
	if got.HTFMultiple != 15 || len(got.HTFDetectors) != 1 || got.HTFMode != "gate" {
		t.Errorf("an unchanged strategy save left htf_multiple=%d htf_detectors=%v htf_mode=%q, want 15 [trend] gate",
			got.HTFMultiple, got.HTFDetectors, got.HTFMode)
	}
}

// TestScreenerFormKeepsWhatItDoesNotShow is the regression for AP2-3: the
// screener form has no by_asset_class / universe_by_asset_class / probe.bars
// fields (the form system cannot render maps of nested structs), and a save
// dropped the first two from the running engine and wrote probe.bars as 0.
func TestScreenerFormKeepsWhatItDoesNotShow(t *testing.T) {
	s := newTestServer(t)

	sc := s.eng.ScreenerConfig()
	n := 3
	sc.ByAssetClass = map[string]config.ScreenerProfile{"etf": {MaxSymbols: &n}}
	sc.UniverseByAssetClass = map[string][]string{"etf": {"SPY"}}
	sc.Probe.Bars = 500
	s.eng.SetScreenerConfig(sc)

	form := formOf(asMap(wrapScreener(s.eng.ScreenerConfig())), mustSectionFields(t, "screener"))
	if rec := postForm(t, s, "/v2/form/settings/screener", form); rec.Code >= 400 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}

	got := s.eng.ScreenerConfig()
	if len(got.ByAssetClass) != 1 || len(got.UniverseByAssetClass) != 1 || got.Probe.Bars != 500 {
		t.Errorf("an unchanged screener save left by_asset_class=%v universe_by_asset_class=%v probe.bars=%d",
			got.ByAssetClass, got.UniverseByAssetClass, got.Probe.Bars)
	}
}

// TestPutStrategyReplacesAMapItMentions guards the other side of bindMerged:
// merging onto the live config must not make a mentioned map additive, or
// deleting a weight would silently keep it.
func TestPutStrategyReplacesAMapItMentions(t *testing.T) {
	s := newTestServer(t)

	cfg := s.eng.StrategyConfig()
	cfg.FastMA, cfg.SlowMA = 5, 20
	cfg.Weights = map[string]float64{"rsi": 2, "macd": 1}
	s.eng.SetStrategyConfig(cfg)

	if code, body := do(t, s, "PUT", "/api/strategy", `{"weights":{"rsi":2}}`); code != 200 {
		t.Fatalf("PUT /api/strategy: %d %s", code, body)
	}

	if w := s.eng.StrategyConfig().Weights; len(w) != 1 {
		t.Errorf("weights after removing macd = %v, want only rsi", w)
	}
}

// TestRejectedExitsSaveLeavesTheLiveLadderAlone guards bindMerged against the
// defect verification found in it: the live config getters return shallow
// copies, and json decodes an array into the existing backing array in place.
// A save that Validate then REJECTED had already rewritten the running
// engine's scale-out ladder — the engine sold on rungs the operator was told
// were refused, and config.yaml disagreed.
func TestRejectedExitsSaveLeavesTheLiveLadderAlone(t *testing.T) {
	s := newTestServer(t)

	live := s.eng.ExitsConfig()
	live.PartialTakeProfit = []config.PartialExit{{ProfitPct: 0.10, SellPct: 0.5}}
	s.eng.SetExitsConfig(live)

	code, _ := do(t, s, "PUT", "/api/settings/exits",
		`{"stop_loss_pct": -5, "partial_take_profit": [{"profit_pct": 0.5, "sell_pct": 0.25}]}`)
	if code < 400 {
		t.Fatalf("a negative stop_loss_pct was accepted (%d); the scenario did not set up", code)
	}

	if got := s.eng.ExitsConfig().PartialTakeProfit; len(got) != 1 || got[0].ProfitPct != 0.10 {
		t.Errorf("a rejected save rewrote the live ladder to %+v", got)
	}
}

// TestPutExitsAcceptsADurationString is the regression for AP2-8. The legacy
// PUT /api/exits bound the bare ExitsConfig, whose max_hold is a
// time.Duration — integer nanoseconds in JSON — so "72h", the spelling the
// settings form and config.yaml use, was a 400. A body carrying nanoseconds,
// as GET /api/exits serves it, must still be accepted.
func TestPutExitsAcceptsADurationString(t *testing.T) {
	s := newTestServer(t)

	if code, body := do(t, s, "PUT", "/api/exits", `{"enabled": true, "max_hold": "72h"}`); code != 200 {
		t.Fatalf("PUT /api/exits with max_hold \"72h\": %d %s", code, body)
	}

	if got := s.eng.ExitsConfig().MaxHold; got != 72*time.Hour {
		t.Errorf("max_hold = %v, want 72h", got)
	}

	if code, body := do(t, s, "PUT", "/api/exits", `{"enabled": true, "max_hold": 3600000000000}`); code != 200 {
		t.Fatalf("PUT /api/exits with max_hold in nanoseconds: %d %s", code, body)
	}

	if got := s.eng.ExitsConfig().MaxHold; got != time.Hour {
		t.Errorf("max_hold = %v, want 1h", got)
	}
}

// TestV2FormsRefuseCrossSiteWrites is the regression for AP2-6. The CSRF fix
// guarded /api only; /v2 takes form-encoded posts — the shape a hostile page
// can auto-submit — and had no origin check, so with auth off (the default)
// any page the operator visited could rewrite the limits, exits or notify
// settings, live and in config.yaml.
func TestV2FormsRefuseCrossSiteWrites(t *testing.T) {
	s := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v2/form/settings/costs",
		strings.NewReader("commission_pct=0.5"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")

	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("a cross-site form post got %d, want 403", rec.Code)
	}

	if got := s.eng.CostsConfig().CommissionPct; got == 0.5 {
		t.Error("the cross-site post changed the live costs")
	}
}
