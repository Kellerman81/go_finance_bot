package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// TestStartTestModeUsesTheConfiguredDefaultDuration checks a bare POST with
// no body falls back to whatever duration is configured, rather than
// requiring every caller to always specify one.
func TestStartTestModeUsesTheConfiguredDefaultDuration(t *testing.T) {
	s := newTestServer(t)

	s.eng.SetTestModeConfig(config.TestModeConfig{Enabled: true, Duration: 0})

	code, body := do(t, s, http.MethodPost, "/api/testmode/start", "")
	if code != http.StatusOK {
		t.Fatalf("POST /api/testmode/start = %d: %s", code, body)
	}

	if !s.eng.Status().TestModeActive {
		t.Fatal("test mode not active after starting")
	}
}

// TestStartTestModeAcceptsAnExplicitDuration checks an operator can override
// the configured default for a single window.
func TestStartTestModeAcceptsAnExplicitDuration(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPost, "/api/testmode/start", `{"duration":"2h"}`)
	if code != http.StatusOK {
		t.Fatalf("POST /api/testmode/start = %d: %s", code, body)
	}

	deadline := s.eng.Status().TestModeDeadline
	if deadline.IsZero() {
		t.Fatal("no deadline recorded for an explicit 2h duration")
	}
}

// TestStartTestModeRejectsAnUnparseableDuration checks a malformed duration
// string is refused rather than silently falling back to the default.
func TestStartTestModeRejectsAnUnparseableDuration(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPost, "/api/testmode/start", `{"duration":"not-a-duration"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("bad duration = %d, want 400: %s", code, body)
	}

	if s.eng.Status().TestModeActive {
		t.Fatal("test mode became active despite the malformed duration")
	}
}

// TestStopTestModeRevertsTheBrokerName checks GET /api/status reports the
// real broker again once test mode ends — the visible signal an operator
// relies on to confirm real trading has resumed.
func TestStopTestModeRevertsTheBrokerName(t *testing.T) {
	s := newTestServer(t)

	before := s.eng.Status().Broker

	if code, body := do(t, s, http.MethodPost, "/api/testmode/start", ""); code != http.StatusOK {
		t.Fatalf("start = %d: %s", code, body)
	}

	if s.eng.Status().Broker != "sim" {
		t.Fatalf("broker while active = %q, want the internal sim", s.eng.Status().Broker)
	}

	code, body := do(t, s, http.MethodPost, "/api/testmode/stop", "")
	if code != http.StatusOK {
		t.Fatalf("POST /api/testmode/stop = %d: %s", code, body)
	}

	status := s.eng.Status()
	if status.TestModeActive {
		t.Error("test mode still reports active after stopping")
	}

	if status.Broker != before {
		t.Errorf("broker after stopping = %q, want the original %q restored", status.Broker, before)
	}
}

// TestStopTestModeIsANoOpWhenNotActive checks the endpoint is safe to call
// even with nothing running (e.g. a UI double-click).
func TestStopTestModeIsANoOpWhenNotActive(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPost, "/api/testmode/stop", "")
	if code != http.StatusOK {
		t.Fatalf("POST /api/testmode/stop with nothing active = %d: %s", code, body)
	}
}

// TestTestModeSettingsRoundTrip checks PUT /api/settings/test_mode persists
// the configured default and GET /api/settings reads it back.
func TestTestModeSettingsRoundTrip(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPut, "/api/settings/test_mode",
		`{"enabled":false,"duration":"6h"}`)
	if code != http.StatusOK {
		t.Fatalf("PUT /api/settings/test_mode = %d: %s", code, body)
	}

	got := s.eng.TestModeConfig()
	if got.Enabled || got.Duration.String() != "6h0m0s" {
		t.Fatalf("TestModeConfig() = %+v, want {Enabled:false Duration:6h}", got)
	}

	settings := getSettingsJSON(t, s)

	tm, ok := settings["test_mode"].(map[string]any)
	if !ok {
		t.Fatal("GET /api/settings has no test_mode section")
	}

	if tm["duration"] != "6h0m0s" {
		t.Errorf("settings.test_mode.duration = %v, want %q", tm["duration"], "6h0m0s")
	}
}

// TestStatusReportsTestModeActive checks GET /api/status carries the fields
// the dashboard pill depends on.
func TestStatusReportsTestModeActive(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodGet, "/api/status", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/status = %d: %s", code, body)
	}

	if !strings.Contains(body, `"test_mode_active":false`) {
		t.Errorf("status body = %s, want test_mode_active:false before starting", body)
	}

	do(t, s, http.MethodPost, "/api/testmode/start", "")

	_, body = do(t, s, http.MethodGet, "/api/status", "")
	if !strings.Contains(body, `"test_mode_active":true`) {
		t.Errorf("status body = %s, want test_mode_active:true after starting", body)
	}
}
