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
// POST /api/screener/rescreen) is unaffected — there's no Content-Type to
// check and nothing a form/text CSRF payload could carry anyway.
func TestBodylessWriteStillWorks(t *testing.T) {
	s := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/screener/rescreen", nil)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	if rec.Code == http.StatusUnsupportedMediaType {
		t.Errorf("a bodyless request was rejected for its (absent) Content-Type: %d %s", rec.Code, rec.Body.String())
	}
}
