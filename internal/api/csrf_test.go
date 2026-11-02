package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSimpleCrossOriginWriteIsRejected reproduces the CSRF finding: with no
// CORS configuration and no CSRF token, a cross-origin page can reach a
// mutating endpoint using a "simple request" (one of the three MIME types a
// browser sends without a CORS preflight: application/x-www-form-urlencoded,
// multipart/form-data, text/plain) because gin used to bind JSON regardless
// of the declared Content-Type. A request with a non-JSON Content-Type must
// now be rejected outright.
func TestSimpleCrossOriginWriteIsRejected(t *testing.T) {
	s := newTestServer(t)

	for _, ct := range []string{
		"text/plain",
		"application/x-www-form-urlencoded",
		"multipart/form-data; boundary=x",
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/trading",
			strings.NewReader(`{"enabled":false}`))
		req.Header.Set("Content-Type", ct)

		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)

		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("Content-Type %q: status = %d, want %d (a browser sends this cross-origin with no CORS preflight)",
				ct, rec.Code, http.StatusUnsupportedMediaType)
		}
	}
}

// TestLegitimateJSONWriteStillWorks checks the fix doesn't collaterally
// break the bundled UI's own requests, which always send
// application/json.
func TestLegitimateJSONWriteStillWorks(t *testing.T) {
	s := newTestServer(t)

	code, body := do(t, s, http.MethodPost, "/api/trading", `{"enabled":false}`)
	if code != http.StatusOK {
		t.Errorf("legitimate application/json request rejected: %d %s", code, body)
	}
}

// TestBodylessWriteStillWorks checks an endpoint with no request body (e.g.
// POST /api/screener/rescreen) still works for a script or the bot's own UI.
// "Nothing a CSRF payload could carry" was wrong for a browser, though — see
// TestACrossSiteBodylessPostIsRefused.
func TestBodylessWriteStillWorks(t *testing.T) {
	s := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/screener/rescreen", nil)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	if rec.Code == http.StatusUnsupportedMediaType {
		t.Errorf("a bodyless request was rejected for its (absent) Content-Type: %d %s", rec.Code, rec.Body.String())
	}
}

// AP3-1: a bodyless POST passed blockSimpleCrossOriginWrites, and /api had no
// Origin check, so with auth off (the default) any web page could
// fetch(url, {method: "POST", mode: "no-cors"}) and start test mode — every
// real stop then filled in the simulator for 48 h — clear a drawdown halt and
// switch trading on, or start autotune.
func TestACrossSiteBodylessPostIsRefused(t *testing.T) {
	s := newTestServer(t)

	for _, path := range []string{"/api/testmode/start", "/api/protections/resume", "/api/autotune/run"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Sec-Fetch-Site", "cross-site")

		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("cross-site POST %s: status %d, want 403", path, rec.Code)
		}
	}

	if s.eng.TestModeStatus().Active {
		t.Error("a cross-site request started test mode")
	}
}

// AP3-1's GET half: /v2/f/symbolOptimize runs an optimizer search holding the
// shared autotune slot, and an <img> on any page could request it. Fragments
// are loaded only by the UI's own same-origin fetches.
func TestACrossSiteFragmentLoadIsRefused(t *testing.T) {
	s := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/v2/f/symbolOptimize?symbol=AAPL", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")

	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site fragment GET: status %d, want 403", rec.Code)
	}

	same := httptest.NewRequest(http.MethodGet, "/v2/f/activity", nil)
	same.Header.Set("Sec-Fetch-Site", "same-origin")

	rec = httptest.NewRecorder()
	s.Router().ServeHTTP(rec, same)

	if rec.Code == http.StatusForbidden {
		t.Error("the UI's own same-origin fragment load was refused")
	}
}
