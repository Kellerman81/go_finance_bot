package broker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kellerman81/go_finance_bot/internal/errlog"
)

// TestRedactSaxoPathBlanksIdentifierParams checks the pure redaction logic
// directly: ClientKey/AccountKey values must be replaced, everything else
// in the path (including other query params) must survive unchanged.
func TestRedactSaxoPathBlanksIdentifierParams(t *testing.T) {
	got := redactSaxoPath("/port/v1/orders/me?ClientKey=CK123&AccountKey=ACC456&Foo=bar")

	if strings.Contains(got, "CK123") || strings.Contains(got, "ACC456") {
		t.Errorf("redactSaxoPath(...) = %q, still contains the raw identifier values", got)
	}

	if !strings.Contains(got, "Foo=bar") {
		t.Errorf("redactSaxoPath(...) = %q, want the unrelated Foo param preserved", got)
	}

	if !strings.Contains(got, "REDACTED") {
		t.Errorf("redactSaxoPath(...) = %q, want a REDACTED marker in place of the values", got)
	}
}

// TestRedactSaxoPathLeavesPlainPathsAlone confirms a path with no query
// string (or none of the identifier params) passes through unchanged.
func TestRedactSaxoPathLeavesPlainPathsAlone(t *testing.T) {
	for _, p := range []string{
		"/port/v1/balances",
		"/trade/v2/orders/12345",
		"/ref/v1/instruments?Keywords=AAPL",
	} {
		if got := redactSaxoPath(p); got != p {
			t.Errorf("redactSaxoPath(%q) = %q, want unchanged", p, got)
		}
	}
}

// TestSaxoErrorPathDoesNotLeakAccountIdentifiersToErrlog reproduces the
// finding end to end: a failing Saxo call whose path carries
// ClientKey/AccountKey (orderWorking, GetAccount, and others all build their
// path this way) must not put those raw values into the errlog ring the
// web UI's diagnostics panel surfaces.
func TestSaxoErrorPathDoesNotLeakAccountIdentifiersToErrlog(t *testing.T) {
	errlog.Clear()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /port/v1/balances", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	s := &Saxo{
		baseURL:    srv.URL,
		tokens:     saxoTestToken("tok"),
		http:       srv.Client(),
		clientKey:  "MY-SECRET-CLIENT-KEY",
		accountKey: "MY-SECRET-ACCOUNT-KEY",
	}

	if _, err := s.GetAccount(context.Background()); err == nil {
		t.Fatal("GetAccount succeeded against a server returning 500, want an error")
	}

	for _, e := range errlog.Recent() {
		if strings.Contains(e.Message, "MY-SECRET-CLIENT-KEY") ||
			strings.Contains(e.Message, "MY-SECRET-ACCOUNT-KEY") {
			t.Errorf("errlog entry leaked the raw account identifiers: %q", e.Message)
		}
	}
}

// EL3-3: only the status-code paths used the redacted path. A transport
// failure — DNS, TCP, TLS, a timeout — is net/http's *url.Error, whose text
// embeds the whole request URL, ClientKey and AccountKey included, and every
// sync of an outage wrote them into the UI-served errlog ring.
func TestASaxoTransportErrorDoesNotLeakAccountIdentifiers(t *testing.T) {
	errlog.Clear()

	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close() // connection refused from here on

	s := &Saxo{
		baseURL:    base,
		tokens:     saxoTestToken("tok"),
		http:       &http.Client{},
		clientKey:  "MY-SECRET-CLIENT-KEY",
		accountKey: "MY-SECRET-ACCOUNT-KEY",
	}

	if _, err := s.GetAccount(context.Background()); err == nil {
		t.Fatal("GetAccount succeeded against a closed server")
	}

	if len(errlog.Recent()) == 0 {
		t.Fatal("the transport failure was not recorded at all")
	}

	for _, e := range errlog.Recent() {
		if strings.Contains(e.Message, "MY-SECRET") {
			t.Errorf("errlog entry leaked the raw account identifiers: %q", e.Message)
		}
	}
}
