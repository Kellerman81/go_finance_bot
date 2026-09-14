package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/ai"
	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/engine"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
	"github.com/gin-gonic/gin"
)

// newTestServer builds a Server over a fully offline engine (mock feed,
// simulated broker, no storage) so the settings endpoints can be exercised
// without touching the network.
func newTestServer(t *testing.T) *Server {
	t.Helper()

	return newTestServerWith(t, nil)
}

// newTestServerWith builds the server over a specific broker. Passing nil uses
// the simulator, which is deliberately NOT a broker.SymbolResolver — so the
// default server exercises the "broker cannot answer" path.
func newTestServerWith(t *testing.T, b broker.Broker) *Server {
	t.Helper()

	gin.SetMode(gin.TestMode)

	var (
		cfg    config.Config
		prices = engine.NewPriceCache()
		feed   = market.NewMock(time.Hour)
		rm     = risk.New(cfg.Limits, cfg.Costs)
		pf     = portfolio.New()
		log    = slog.New(slog.NewTextHandler(io.Discard, nil))
	)

	brk := b
	if brk == nil {
		brk = broker.NewSim(10000, prices.Get, cfg.Costs.Commission)
	}

	eng := engine.New(
		cfg.Engine, cfg.Exits, cfg.Safety, cfg.Strategy,
		feed, brk, rm, pf, prices, nil, log,
	)

	t.Cleanup(func() {
		eng.SetNotifyConfig(config.NotifyConfig{})
		eng.Close()
		feed.Close()
	})

	return New(eng, rm, pf, prices, "")
}

// do issues one request against the API and returns the status and body.
func do(t *testing.T, s *Server, method, path, body string) (int, string) {
	t.Helper()

	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	// The gzip middleware only compresses when the client asks for it, and this
	// test never does, so the recorded body is plain JSON.
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	return rec.Code, rec.Body.String()
}

// getSettingsJSON reads GET /api/settings into a generic map.
func getSettingsJSON(t *testing.T, s *Server) map[string]any {
	t.Helper()

	code, body := do(t, s, http.MethodGet, "/api/settings", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/settings = %d: %s", code, body)
	}

	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode settings: %v (body %s)", err, body)
	}

	return out
}

// section pulls one settings block out of the response.
func section(t *testing.T, all map[string]any, name string) map[string]any {
	t.Helper()

	sec, ok := all[name].(map[string]any)
	if !ok {
		t.Fatalf("settings has no %q object: %v", name, all[name])
	}

	return sec
}

func TestSettingsExposesEveryBlock(t *testing.T) {
	s := newTestServer(t)
	all := getSettingsJSON(t, s)

	for _, name := range []string{
		"costs", "protections", "portfolio", "sectors", "screener", "notify", "ai", "ml",
	} {
		if _, ok := all[name]; !ok {
			t.Errorf("GET /api/settings is missing the %q block", name)
		}
	}
}

func TestCostsRoundTrip(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPut, "/api/settings/costs",
		`{"commission_pct":0.002,"commission_flat":1,"commission_min":2,`+
			`"spread_pct":0.0005,"slippage_pct":0.0002}`)
	if code != http.StatusOK {
		t.Fatalf("PUT costs = %d: %s", code, body)
	}

	got := s.eng.CostsConfig()
	if got.CommissionPct != 0.002 || got.SpreadPct != 0.0005 || got.SlippagePct != 0.0002 {
		t.Fatalf("costs not applied: %+v", got)
	}

	// The snake_case names the UI submits must be the names it reads back.
	sec := section(t, getSettingsJSON(t, s), "costs")
	if sec["commission_pct"] != 0.002 {
		t.Fatalf("commission_pct did not survive the round-trip: %v", sec)
	}
}

func TestCostsRejectsNegative(t *testing.T) {
	s := newTestServer(t)

	code, _ := do(t, s, http.MethodPut, "/api/settings/costs", `{"commission_pct":-0.001}`)
	if code != http.StatusBadRequest {
		t.Fatalf("negative commission accepted: status %d", code)
	}
}

// Durations must travel as the strings config.yaml uses. encoding/json's default
// for time.Duration is a nanosecond integer, which no one can type into a form.
func TestDurationsTravelAsStrings(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPut, "/api/settings/protections",
		`{"enabled":true,"cooldown_after_loss":"90m","stop_guard_trades":3,`+
			`"stop_guard_lookback":"24h","stop_guard_lock":"4h",`+
			`"max_drawdown_pct":0.2,"max_daily_loss_pct":0.05}`)
	if code != http.StatusOK {
		t.Fatalf("PUT protections = %d: %s", code, body)
	}

	if got := s.eng.ProtectionsConfig().CooldownAfterLoss; got != 90*time.Minute {
		t.Fatalf("cooldown_after_loss = %v, want 1h30m", got)
	}

	sec := section(t, getSettingsJSON(t, s), "protections")
	if sec["cooldown_after_loss"] != "1h30m0s" {
		t.Fatalf(
			"cooldown_after_loss read back as %v, want the string form",
			sec["cooldown_after_loss"],
		)
	}
}

func TestBadDurationIsRejected(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPut, "/api/settings/protections",
		`{"enabled":true,"cooldown_after_loss":"two hours"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("unparseable duration accepted: status %d (%s)", code, body)
	}
}

func TestProtectionsOutOfRangeIsRejected(t *testing.T) {
	s := newTestServer(t)

	code, _ := do(t, s, http.MethodPut, "/api/settings/protections",
		`{"enabled":true,"max_drawdown_pct":20}`)
	if code != http.StatusBadRequest {
		t.Fatalf("max_drawdown_pct of 20 accepted: status %d", code)
	}
}

// Editing a threshold is a settings change, not a decision to resume trading. A
// halt that is live when the edit lands must still be live afterwards.
func TestProtectionsEditPreservesHalt(t *testing.T) {
	s := newTestServer(t)

	s.eng.SetProtectionsConfig(config.ProtectionsConfig{Enabled: true, MaxDrawdownPct: 0.10})

	prot := s.eng.Protections()
	if prot == nil {
		t.Fatal("protections not installed")
	}

	prot.ObserveEquity(10000) // sets the peak
	prot.ObserveEquity(8000)  // 20% below it — halt

	if halted, _ := prot.Halted(); !halted {
		t.Fatal("expected a drawdown halt before the edit")
	}

	code, body := do(t, s, http.MethodPut, "/api/settings/protections",
		`{"enabled":true,"max_drawdown_pct":0.30,"max_daily_loss_pct":0.05}`)
	if code != http.StatusOK {
		t.Fatalf("PUT protections = %d: %s", code, body)
	}

	if halted, _ := s.eng.Protections().Halted(); !halted {
		t.Fatal("editing a threshold cleared an active halt; resuming must be deliberate")
	}

	if got := s.eng.ProtectionsConfig().MaxDrawdownPct; got != 0.30 {
		t.Fatalf("max_drawdown_pct = %v, want 0.30", got)
	}
}

func TestNotifySecretsAreRedacted(t *testing.T) {
	s := newTestServer(t)

	s.eng.SetNotifyConfig(config.NotifyConfig{
		Enabled:       true,
		PushoverToken: "super-secret-token",
		PushoverUser:  "super-secret-user",
		TelegramToken: "12345:telegram",
	})

	code, body := do(t, s, http.MethodGet, "/api/settings", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/settings = %d", code)
	}

	for _, secret := range []string{"super-secret-token", "super-secret-user", "12345:telegram"} {
		if strings.Contains(body, secret) {
			t.Fatalf("GET /api/settings leaked %q", secret)
		}
	}

	sec := section(t, getSettingsJSON(t, s), "notify")
	if sec["pushover_token"] != redactedMarker {
		t.Fatalf("pushover_token = %v, want the redaction marker", sec["pushover_token"])
	}
}

// Saving a form that only ever showed the marker must not wipe the stored
// credential.
func TestSavingRedactedNotifyKeepsStoredSecrets(t *testing.T) {
	s := newTestServer(t)

	s.eng.SetNotifyConfig(config.NotifyConfig{
		Enabled:       true,
		PushoverToken: "keep-me",
		PushoverUser:  "keep-me-too",
	})

	code, body := do(t, s, http.MethodPut, "/api/settings/notify",
		`{"enabled":true,"min_notional":25,"timeout":"10s",`+
			`"pushover_token":"`+redactedMarker+`","pushover_user":"`+redactedMarker+`"}`)
	if code != http.StatusOK {
		t.Fatalf("PUT notify = %d: %s", code, body)
	}

	got := s.eng.NotifyConfig()
	if got.PushoverToken != "keep-me" || got.PushoverUser != "keep-me-too" {
		t.Fatalf("redacted save erased the stored secrets: %+v", got)
	}

	if got.MinNotional != 25 || got.Timeout != 10*time.Second {
		t.Fatalf("notify edits not applied: %+v", got)
	}
}

// A partial update that never mentions a secret field at all (as opposed to
// sending the redaction marker) must not wipe it either — an ordinary
// unmarshal of the omitted field into notifyWire's plain string leaves it
// "", indistinguishable from an explicit clear unless the handler checks
// which JSON keys were actually present.
func TestOmittingNotifySecretFieldsPreservesThem(t *testing.T) {
	s := newTestServer(t)

	s.eng.SetNotifyConfig(config.NotifyConfig{
		Enabled:         true,
		Webhook:         "https://example.com/hook",
		DiscordWebhook:  "https://discord.example.com/hook",
		TelegramToken:   "keep-telegram",
		PushoverToken:   "keep-pushover-token",
		PushoverUser:    "keep-pushover-user",
		AppriseServices: "keep-apprise",
	})

	// Only "enabled" and "min_notional" are mentioned; every secret field is
	// entirely absent from the body (not even sent as the redaction marker).
	code, body := do(t, s, http.MethodPut, "/api/settings/notify",
		`{"enabled":true,"min_notional":25,"timeout":"10s"}`)
	if code != http.StatusOK {
		t.Fatalf("PUT notify = %d: %s", code, body)
	}

	got := s.eng.NotifyConfig()
	if got.Webhook != "https://example.com/hook" ||
		got.DiscordWebhook != "https://discord.example.com/hook" ||
		got.TelegramToken != "keep-telegram" ||
		got.PushoverToken != "keep-pushover-token" ||
		got.PushoverUser != "keep-pushover-user" ||
		got.AppriseServices != "keep-apprise" {
		t.Fatalf("omitting secret fields wiped them: %+v", got)
	}

	if got.MinNotional != 25 {
		t.Fatalf("min_notional not applied: %+v", got)
	}
}

// An explicit empty string is still a real instruction to clear the field —
// only omission is "leave it alone".
func TestExplicitEmptyNotifySecretStillClearsIt(t *testing.T) {
	s := newTestServer(t)

	s.eng.SetNotifyConfig(config.NotifyConfig{Enabled: true, PushoverToken: "old-token"})

	code, body := do(t, s, http.MethodPut, "/api/settings/notify",
		`{"enabled":true,"timeout":"10s","pushover_token":""}`)
	if code != http.StatusOK {
		t.Fatalf("PUT notify = %d: %s", code, body)
	}

	if got := s.eng.NotifyConfig().PushoverToken; got != "" {
		t.Fatalf("pushover_token = %q, want cleared by the explicit empty string", got)
	}
}

// A real new value must still replace the stored one.
func TestNotifyAcceptsAReplacementSecret(t *testing.T) {
	s := newTestServer(t)

	s.eng.SetNotifyConfig(config.NotifyConfig{Enabled: true, PushoverToken: "old"})

	code, body := do(t, s, http.MethodPut, "/api/settings/notify",
		`{"enabled":true,"timeout":"10s","pushover_token":"new"}`)
	if code != http.StatusOK {
		t.Fatalf("PUT notify = %d: %s", code, body)
	}

	if got := s.eng.NotifyConfig().PushoverToken; got != "new" {
		t.Fatalf("pushover_token = %q, want %q", got, "new")
	}
}

// An absent sectors map means "leave it alone"; an explicit {} clears it.
func TestPortfolioSectorsAreOnlyReplacedWhenSent(t *testing.T) {
	s := newTestServer(t)

	s.eng.SetPortfolioConfig(
		config.PortfolioConfig{Enabled: true},
		map[string]string{"AAPL": "tech"},
		nil,
	)

	code, body := do(t, s, http.MethodPut, "/api/settings/portfolio",
		`{"enabled":true,"correlation_threshold":0.8,"max_correlated_positions":2}`)
	if code != http.StatusOK {
		t.Fatalf("PUT portfolio = %d: %s", code, body)
	}

	if _, sectors, _ := s.eng.PortfolioConfig(); sectors["AAPL"] != "tech" {
		t.Fatalf("omitting sectors dropped the map: %v", sectors)
	}

	code, body = do(t, s, http.MethodPut, "/api/settings/portfolio",
		`{"enabled":true,"correlation_threshold":0.8,"sectors":{"MSFT":"tech"}}`)
	if code != http.StatusOK {
		t.Fatalf("PUT portfolio = %d: %s", code, body)
	}

	_, sectors, _ := s.eng.PortfolioConfig()
	if sectors["MSFT"] != "tech" || sectors["AAPL"] != "" {
		t.Fatalf("sectors not replaced: %v", sectors)
	}
}

// The asset-class map follows the same absent/explicit rule as sectors, and
// an operator correcting a classification by hand may type the provider's own
// spelling — it has to land in the bucket the caps are keyed by.
func TestPortfolioAssetClassesAreOnlyReplacedWhenSentAndNormalized(t *testing.T) {
	s := newTestServer(t)

	s.eng.SetPortfolioConfig(
		config.PortfolioConfig{Enabled: true},
		nil,
		map[string]string{"IWDA.AS": "etf"},
	)

	code, body := do(t, s, http.MethodPut, "/api/settings/portfolio",
		`{"enabled":true,"max_pct_per_asset_class":{"etf":0.3}}`)
	if code != http.StatusOK {
		t.Fatalf("PUT portfolio = %d: %s", code, body)
	}

	port, _, classes := s.eng.PortfolioConfig()
	if classes["IWDA.AS"] != "etf" {
		t.Fatalf("omitting asset_classes dropped the map: %v", classes)
	}

	if port.MaxPctPerAssetClass["etf"] != 0.3 {
		t.Fatalf("the allocation cap was not applied: %v", port.MaxPctPerAssetClass)
	}

	code, body = do(t, s, http.MethodPut, "/api/settings/portfolio",
		`{"enabled":true,"asset_classes":{"vwce.de":"Etf"}}`)
	if code != http.StatusOK {
		t.Fatalf("PUT portfolio = %d: %s", code, body)
	}

	_, _, classes = s.eng.PortfolioConfig()
	if classes["VWCE.DE"] != "etf" {
		t.Fatalf("asset class not upper-cased/normalised on write: %v", classes)
	}

	if classes["IWDA.AS"] != "" {
		t.Fatalf("asset classes not replaced: %v", classes)
	}
}

// A percentage written as "30" instead of 0.30 is a cap that can never bind,
// on an operator who believes they are capped. The API must refuse it rather
// than store it.
func TestPortfolioRejectsANonFractionAssetClassCap(t *testing.T) {
	s := newTestServer(t)

	code, _ := do(t, s, http.MethodPut, "/api/settings/portfolio",
		`{"enabled":true,"max_pct_per_asset_class":{"etf":30}}`)
	if code == http.StatusOK {
		t.Fatal("a cap of 30 (meaning 30%) was accepted")
	}
}

// A changed screening interval cannot take effect without a restart; the API
// says so rather than pretending it applied.
func TestScreenerIntervalChangeIsReported(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPut, "/api/settings/screener",
		`{"enabled":true,"interval":"6h","max_symbols":20,"rank":"volume","keep_held":true}`)
	if code != http.StatusOK {
		t.Fatalf("PUT screener = %d: %s", code, body)
	}

	if !strings.Contains(body, "restart") {
		t.Fatalf("changed interval reported no restart note: %s", body)
	}

	if got := s.eng.ScreenerConfig().Interval; got != 6*time.Hour {
		t.Fatalf("interval = %v, want 6h", got)
	}
}

// TestAutotuneSettingsApply mirrors TestScreenerIntervalChangeIsReported:
// enabling the scheduled auto-optimizer for the first time (it starts
// disabled on every test server) is reported as needing a restart — the
// background goroutine is only started once, at process startup, gated on
// whether it was already enabled then — while the submitted fields are all
// still applied live immediately, so a follow-up GET reflects them without
// waiting for that restart.
func TestAutotuneSettingsApply(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPut, "/api/settings/autotune",
		`{"enabled":true,"interval":"12h","bars":2000,"min_trades":15,"objective":"return","min_return_pct":1.5,`+
			`"run_only_for_symbols":["AAPL"],"exclude_symbols":["GOOG"]}`)
	if code != http.StatusOK {
		t.Fatalf("PUT autotune = %d: %s", code, body)
	}

	if !strings.Contains(body, "restart") {
		t.Fatalf("enabling autotune for the first time reported no restart note: %s", body)
	}

	got := s.eng.AutotuneConfig()
	if !got.Enabled || got.Interval != 12*time.Hour || got.Bars != 2000 || got.MinTrades != 15 {
		t.Fatalf("AutotuneConfig = %+v, want the submitted fields applied live", got)
	}

	if got.Objective != "return" {
		t.Fatalf("Objective = %q, want \"return\"", got.Objective)
	}

	if got.MinReturnPct != 1.5 {
		t.Fatalf("MinReturnPct = %v, want 1.5", got.MinReturnPct)
	}

	if len(got.RunOnlyForSymbols) != 1 || got.RunOnlyForSymbols[0] != "AAPL" {
		t.Fatalf("RunOnlyForSymbols = %v, want [AAPL]", got.RunOnlyForSymbols)
	}

	if len(got.ExcludeSymbols) != 1 || got.ExcludeSymbols[0] != "GOOG" {
		t.Fatalf("ExcludeSymbols = %v, want [GOOG]", got.ExcludeSymbols)
	}
}

// The on_loss block round-trips through the API, including its cooldown as a
// duration STRING — the whole reason autotuneWire shadows the embedded field.
// A nanosecond count in a form is unusable, and a cooldown that silently
// decoded to zero would remove the only rate limit on reactive re-tuning.
func TestAutotuneOnLossSettingsRoundTrip(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPut, "/api/settings/autotune",
		`{"enabled":true,"interval":"12h",`+
			`"on_loss":{"enabled":true,"after_losses":4,"cooldown":"6h"}}`)
	if code != http.StatusOK {
		t.Fatalf("PUT autotune = %d: %s", code, body)
	}

	got := s.eng.AutotuneConfig()
	if !got.OnLoss.Enabled || got.OnLoss.AfterLosses != 4 || got.OnLoss.Cooldown != 6*time.Hour {
		t.Fatalf("OnLoss = %+v, want {true 4 6h}", got.OnLoss)
	}

	// And it comes back out in the same string form a form can render.
	code, body = do(t, s, http.MethodGet, "/api/settings", "")
	if code != http.StatusOK {
		t.Fatalf("GET settings = %d: %s", code, body)
	}

	if !strings.Contains(body, `"cooldown":"6h0m0s"`) {
		t.Fatalf("GET settings did not render the cooldown as a duration string: %s", body)
	}
}

// An unparseable cooldown is a bad request, not a silent zero.
func TestAutotuneOnLossRejectsABadCooldown(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPut, "/api/settings/autotune",
		`{"enabled":true,"on_loss":{"enabled":true,"after_losses":3,"cooldown":"soon"}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("an unparseable cooldown was accepted: %d (%s)", code, body)
	}
}

// TestAutotuneRejectsOverlappingSymbolLists checks the API surfaces
// config.AutotuneConfig.Validate's own rejection of a symbol named in both
// run_only_for_symbols and exclude_symbols, rather than silently accepting
// a self-contradictory list.
func TestAutotuneRejectsOverlappingSymbolLists(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPut, "/api/settings/autotune",
		`{"enabled":true,"run_only_for_symbols":["AAPL"],"exclude_symbols":["AAPL"]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("a symbol in both lists was accepted: %d (%s)", code, body)
	}
}

// TestAutotuneOtherFieldsApplyWithoutARestartNote checks the restart note is
// specific to enabling-for-the-first-time and to a changed interval — a
// save that touches neither (e.g. just tightening min_trades on an
// already-enabled job) applies live with nothing to warn about, same as
// applyScreener's own precedent.
func TestAutotuneOtherFieldsApplyWithoutARestartNote(t *testing.T) {
	s := newTestServer(t)

	s.eng.SetAutotuneConfig(config.AutotuneConfig{Enabled: true, Interval: 24 * time.Hour, Bars: 1440, MinTrades: 10})

	code, body := do(t, s, http.MethodPut, "/api/settings/autotune",
		`{"enabled":true,"interval":"24h","bars":1440,"min_trades":20}`)
	if code != http.StatusOK {
		t.Fatalf("PUT autotune = %d: %s", code, body)
	}

	if strings.Contains(body, "restart") {
		t.Fatalf("an unchanged interval on an already-enabled job reported a restart note: %s", body)
	}

	if got := s.eng.AutotuneConfig().MinTrades; got != 20 {
		t.Fatalf("min_trades = %d, want 20", got)
	}
}

func TestMLRejectsCrossedThresholds(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPut, "/api/settings/ml",
		`{"enabled":true,"buy_prob":0.5,"sell_prob":0.6}`)
	if code != http.StatusBadRequest {
		t.Fatalf("sell_prob above buy_prob accepted: %d (%s)", code, body)
	}
}

// The AI advisor's API key lives in a package-level singleton (ai.Default),
// not per-Server state, so this test resets it in a Cleanup rather than
// relying on the next test's own setup.
func configureAI(t *testing.T, cfg config.AIConfig) {
	t.Helper()

	if err := ai.Configure(cfg); err != nil {
		t.Fatalf("ai.Configure: %v", err)
	}

	t.Cleanup(func() { _ = ai.Configure(config.AIConfig{}) })
}

// Same omission-vs-explicit-clear distinction as notify, for the AI advisor's
// api_key field.
func TestOmittingAIAPIKeyFieldPreservesIt(t *testing.T) {
	s := newTestServer(t)

	configureAI(t, config.AIConfig{APIKey: "keep-me"})

	code, body := do(t, s, http.MethodPut, "/api/settings/ai",
		`{"enabled":false,"timeout":"10s","refresh_interval":"5m","max_age":"1h"}`)
	if code != http.StatusOK {
		t.Fatalf("PUT ai = %d: %s", code, body)
	}

	if got := s.aiConfig().APIKey; got != "keep-me" {
		t.Fatalf("omitting api_key wiped it: got %q", got)
	}
}

func TestExplicitEmptyAIAPIKeyStillClearsIt(t *testing.T) {
	s := newTestServer(t)

	configureAI(t, config.AIConfig{APIKey: "old-key"})

	code, body := do(t, s, http.MethodPut, "/api/settings/ai",
		`{"enabled":false,"timeout":"10s","refresh_interval":"5m","max_age":"1h","api_key":""}`)
	if code != http.StatusOK {
		t.Fatalf("PUT ai = %d: %s", code, body)
	}

	if got := s.aiConfig().APIKey; got != "" {
		t.Fatalf("api_key = %q, want cleared by the explicit empty string", got)
	}
}

// DELETE /api/open/:symbol is bookkeeping for a position closed outside the
// bot. Asking to forget one the bot has no record of is a 404, not a silent OK.
func TestForgetUnknownOpenPositionIs404(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodDelete, "/api/open/NOPE", "")
	if code != http.StatusNotFound {
		t.Fatalf("forgetting an unknown position = %d, want 404 (%s)", code, body)
	}
}

// POST /api/open/:symbol/adopt is the mirror of the forget endpoint, for a
// symbol the broker holds that the bot has no record of at all. Asking to
// adopt one the broker does not report is a 404, not a silent OK.
func TestAdoptOpenPositionIs404WhenNotHeldAtTheBroker(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPost, "/api/open/NOPE/adopt", "")
	if code != http.StatusNotFound {
		t.Fatalf("adopting an unheld symbol = %d, want 404 (%s)", code, body)
	}
}

// A symbol the broker reports but the bot never opened (the "held, not
// tracked" Reconciliation row — see engine.Engine.AdoptHeldPosition's doc
// comment for why an untracked holding gets only a bare stop-loss) must
// become a real, durable open-position record once adopted.
func TestAdoptOpenPositionCreatesATrackedRecordFromTheBrokerHolding(t *testing.T) {
	s := newTestServer(t)

	s.pf.SyncPositions([]portfolio.Holding{
		{Symbol: "MRVL", Qty: 0.4829, AvgPrice: 207.06, LastPrice: 210.00},
	})

	code, body := do(t, s, http.MethodPost, "/api/open/MRVL/adopt", "")
	if code != http.StatusOK {
		t.Fatalf("adopt = %d: %s", code, body)
	}

	open := s.eng.OpenPositions()
	if len(open) != 1 {
		t.Fatalf("OpenPositions() has %d record(s), want 1: %+v", len(open), open)
	}

	if open[0].Symbol != "MRVL" || open[0].Qty != 0.4829 || open[0].EntryPrice != 207.06 {
		t.Errorf("adopted record = %+v, want MRVL/0.4829/207.06", open[0])
	}
}

// The exits block now carries the ladders and the time barrier, so it round-trips
// through the settings API like every other section.
func TestExitsRoundTripIncludingTheLadders(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPut, "/api/settings/exits",
		`{"enabled":true,"stop_loss_pct":0.05,"trailing_stop_pct":0.10,`+
			`"trailing_offset_pct":0.03,"trailing_tight_pct":0.02,`+
			`"trailing_only_after_offset":true,"max_hold":"6h",`+
			`"roi":{"0s":0.10,"2h":0.02},`+
			`"partial_take_profit":[{"profit_pct":0.05,"sell_pct":0.33},`+
			`{"profit_pct":0.12,"sell_pct":0.33}]}`)
	if code != http.StatusOK {
		t.Fatalf("PUT exits = %d: %s", code, body)
	}

	got := s.eng.ExitsConfig()
	if got.MaxHold != 6*time.Hour {
		t.Errorf("max_hold = %v, want 6h", got.MaxHold)
	}

	if got.TrailingTightPct != 0.02 || !got.TrailingOnlyAfterOffset {
		t.Errorf("two-stage trail not applied: %+v", got)
	}

	if len(got.PartialTakeProfit) != 2 || got.PartialTakeProfit[0].SellPct != 0.33 {
		t.Errorf("scale-out ladder not applied: %+v", got.PartialTakeProfit)
	}

	if len(got.ROI) != 2 {
		t.Errorf("ROI ladder not applied: %+v", got.ROI)
	}

	sec := section(t, getSettingsJSON(t, s), "exits")
	if sec["max_hold"] != "6h0m0s" {
		t.Errorf("max_hold read back as %v, want the string form", sec["max_hold"])
	}
}

// A ladder that sells more than the position is a typo the API must catch, not
// pass through to a live engine.
func TestExitsRejectsAnOversizedLadder(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPut, "/api/settings/exits",
		`{"enabled":true,"partial_take_profit":[{"profit_pct":0.05,"sell_pct":0.7},`+
			`{"profit_pct":0.10,"sell_pct":0.7}]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("a 140%% ladder was accepted: %d (%s)", code, body)
	}
}

func TestEntriesRoundTrip(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPut, "/api/settings/entries",
		`{"allow_pyramiding":false,"skip_full_slots":true,`+
			`"scale_in":{"enabled":true,"step_pct":0.04,"max_adds":2,"size_pct":0.5,`+
			`"require_signal":true}}`)
	if code != http.StatusOK {
		t.Fatalf("PUT entries = %d: %s", code, body)
	}

	got := s.eng.EntryPolicy()
	if !got.SkipFullSlots || !got.ScaleIn.Enabled || got.ScaleIn.MaxAdds != 2 {
		t.Fatalf("entry policy not applied: %+v", got)
	}
}

// The cap and the size limit are what separate averaging down from a martingale,
// so the API enforces them exactly as the config loader does.
func TestEntriesRejectsAnUncappedOrOversizedLadder(t *testing.T) {
	s := newTestServer(t)

	for name, body := range map[string]string{
		"no cap": `{"scale_in":{"enabled":true,"step_pct":0.04,"max_adds":0,"size_pct":0.5}}`,
		"martingale sizing": `{"scale_in":{"enabled":true,"step_pct":0.04,` +
			`"max_adds":3,"size_pct":2}}`,
	} {
		code, resp := do(t, s, http.MethodPut, "/api/settings/entries", body)
		if code != http.StatusBadRequest {
			t.Errorf("%s: accepted with status %d (%s)", name, code, resp)
		}
	}
}

func TestUnknownSectionIs404(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPut, "/api/settings/nonsense", `{}`)
	if code != http.StatusNotFound {
		t.Fatalf("unknown section = %d, want 404", code)
	}

	if !strings.Contains(body, "costs") {
		t.Fatalf("404 body does not list the valid sections: %s", body)
	}
}
