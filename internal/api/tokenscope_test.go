package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// sseRequest builds a request against an already-cancelled context, so
// streamEvents' own select loop (which otherwise streams until the client
// disconnects or a 25s ping ticker fires) returns on its very first
// iteration instead of hanging the test — the auth middleware under test
// here runs before the handler and does not care whether the context is
// already done.
func sseRequest(method, path string) *http.Request {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	return httptest.NewRequest(method, path, nil).WithContext(ctx)
}

// TestQueryTokenAuthenticatesOnlyTheSSERoute reproduces the "Token
// persisted in localStorage on the v2 UI; ?token= accepted on every
// route, not just SSE" finding's actionable half: a query-string token
// travels in the URL, so accepting it on every route means it leaks into
// browser history, the Referer header on any same-origin navigation, and
// server/proxy access logs on every single request — not just the one
// (EventSource-based SSE) endpoint that has no alternative because it
// cannot set an Authorization header.
func TestQueryTokenAuthenticatesOnlyTheSSERoute(t *testing.T) {
	s := newTestServer(t)
	s.authToken = "secret"

	// /api/events is the one route where a ?token= must still work.
	rec := httptest.NewRecorder()
	req := sseRequest(http.MethodGet, "/api/events?token=secret")
	s.Router().ServeHTTP(rec, req)

	if rec.Code == http.StatusUnauthorized {
		t.Errorf("GET /api/events?token=secret = %d, want it authenticated (this is the one "+
			"route EventSource, which cannot set headers, actually needs it for)", rec.Code)
	}
}

// TestQueryTokenDoesNotAuthenticateOtherRoutes confirms the scope
// restriction: a non-SSE route must reject a ?token= query parameter and
// require the Authorization header instead.
func TestQueryTokenDoesNotAuthenticateOtherRoutes(t *testing.T) {
	s := newTestServer(t)
	s.authToken = "secret"

	for _, path := range []string{
		"/api/status?token=secret",
		"/api/settings?token=secret",
		"/v2/f/dashboard?token=secret",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		s.Router().ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s = %d, want 401 — a query-string token must not authenticate "+
				"routes other than the SSE stream", path, rec.Code)
		}
	}
}

// TestAuthorizationHeaderStillWorksOnEveryRoute confirms the fix didn't
// break the primary (header-based) auth path anywhere, SSE route included.
func TestAuthorizationHeaderStillWorksOnEveryRoute(t *testing.T) {
	s := newTestServer(t)
	s.authToken = "secret"

	for _, path := range []string{"/api/status", "/api/events"} {
		rec := httptest.NewRecorder()

		var req *http.Request
		if path == "/api/events" {
			req = sseRequest(http.MethodGet, path)
		} else {
			req = httptest.NewRequest(http.MethodGet, path, nil)
		}

		req.Header.Set("Authorization", "Bearer secret")
		s.Router().ServeHTTP(rec, req)

		if rec.Code == http.StatusUnauthorized {
			t.Errorf("GET %s with a valid Authorization header = %d, want it authenticated",
				path, rec.Code)
		}
	}
}
