package api

import (
	"bytes"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestTokenScrubbingWriterRedactsTheQueryToken reproduces the "?token= +
// gin.Recovery = the token in the log" finding at the writer level: gin's
// broken-pipe recovery path dumps the raw request line, including the query
// string, in every gin mode — not only DebugMode — because EventSource
// cannot set an Authorization header and so requireToken also accepts
// ?token=. The wrapping writer installed in Router must strip it before it
// reaches the underlying log.
func TestTokenScrubbingWriterRedactsTheQueryToken(t *testing.T) {
	var out bytes.Buffer

	w := tokenScrubbingWriter{&out}

	n, err := w.Write([]byte("GET /api/events?token=super-secret-value HTTP/1.1\r\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if n != len("GET /api/events?token=super-secret-value HTTP/1.1\r\n") {
		t.Errorf("Write returned n=%d, want len(p) regardless of the redaction", n)
	}

	if bytes.Contains(out.Bytes(), []byte("super-secret-value")) {
		t.Errorf("the token survived redaction: %s", out.String())
	}

	if !bytes.Contains(out.Bytes(), []byte("token=REDACTED")) {
		t.Errorf("the query string lost its shape, not just its value: %s", out.String())
	}
}

// A request line with no token query param must pass through unchanged.
func TestTokenScrubbingWriterLeavesOrdinaryLinesAlone(t *testing.T) {
	var out bytes.Buffer

	w := tokenScrubbingWriter{&out}

	line := "GET /api/dashboard HTTP/1.1\r\n"
	if _, err := w.Write([]byte(line)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if out.String() != line {
		t.Errorf("Write = %q, want the input unchanged", out.String())
	}
}

// TestRouterUpgradesTheUnsetDefaultModeToRelease checks Router's guard: gin
// otherwise defaults to DebugMode until something calls SetMode, and nothing
// in cmd/bot ever did — which is what let gin.Recovery's debug-only request
// dump (containing ?token=) reach the log on every panic.
func TestRouterUpgradesTheUnsetDefaultModeToRelease(t *testing.T) {
	// gin.Mode() is process-global and other tests in this package
	// legitimately set it to TestMode; this test only asserts Router's
	// behavior starting from the DebugMode default, and restores whatever
	// mode was live before it ran so it does not affect test order.
	before := gin.Mode()
	t.Cleanup(func() { gin.SetMode(before) })

	// newTestServer itself calls gin.SetMode(gin.TestMode) as part of
	// building the engine, so the DebugMode starting point has to be forced
	// back AFTER construction and right before the call under test.
	s := newTestServer(t)
	gin.SetMode(gin.DebugMode)
	s.Router()

	if gin.Mode() == gin.DebugMode {
		t.Error("Router left gin in DebugMode; gin.Recovery would dump ?token= on a panic")
	}
}
