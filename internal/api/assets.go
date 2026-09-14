package api

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
)

// staticAsset holds a precomputed embedded page: the raw bytes, a gzip-compressed
// copy, and a content-hash ETag. Compressing and hashing once at startup avoids
// re-reading the embed FS and re-compressing on every request.
type staticAsset struct {
	raw  []byte
	gz   []byte
	etag string
	mime string // media type; empty means HTML, for the original single-page asset
}

// indexAsset is the compiled UI page, built once at package init.
var indexAsset = buildIndexAsset()

// buildIndexAsset reads the embedded UI page and precomputes its gzip copy
// and content-hash ETag.
func buildIndexAsset() staticAsset {
	raw, err := webFS.ReadFile("web/index.html")
	if err != nil {
		// Embedded at build time — a read failure is a programming error.
		panic("api: read embedded index.html: " + err.Error())
	}

	var buf bytes.Buffer

	gw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)

	_, _ = gw.Write(raw)
	_ = gw.Close()

	sum := sha256.Sum256(raw)

	return staticAsset{
		raw:  raw,
		gz:   buf.Bytes(),
		etag: fmt.Sprintf("\"%x\"", sum[:16]),
		mime: "text/html; charset=utf-8",
	}
}

// serveIndex serves the UI page with an ETag and long-lived cache, returning 304
// when the client already has the current build, and a gzip copy when accepted.
func (a staticAsset) serve(c *gin.Context) {
	c.Header("ETag", a.etag)
	c.Header("Cache-Control", "no-cache") // always revalidate; ETag makes that a 304

	if match := c.GetHeader("If-None-Match"); match != "" && strings.Contains(match, a.etag) {
		c.Status(http.StatusNotModified)

		return
	}

	if strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") {
		c.Header("Content-Encoding", "gzip")
		c.Header("Vary", "Accept-Encoding")
		c.Data(http.StatusOK, a.mime, a.gz)

		return
	}

	c.Data(http.StatusOK, a.mime, a.raw)
}

// gzipJSON compresses eligible /api JSON responses. It never compresses the SSE
// stream (which must flush unbuffered) and only engages when the client accepts
// gzip.
func gzipJSON() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.FullPath() == "/api/events" ||
			!strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") {
			c.Next()

			return
		}

		gz, _ := gzip.NewWriterLevel(c.Writer, gzip.DefaultCompression)

		defer gz.Close()

		c.Header("Content-Encoding", "gzip")
		c.Header("Vary", "Accept-Encoding")

		c.Writer = &gzipWriter{ResponseWriter: c.Writer, gz: gz}
		c.Next()
	}
}

type gzipWriter struct {
	gin.ResponseWriter
	gz *gzip.Writer
}

// Write compresses p through the gzip writer.
func (g *gzipWriter) Write(b []byte) (int, error) { return g.gz.Write(b) }

// WriteString compresses s through the gzip writer.
func (g *gzipWriter) WriteString(s string) (int, error) { return g.gz.Write([]byte(s)) }

// WriteHeader strips Content-Length, which no longer matches once the body is
// compressed (the response falls back to chunked transfer encoding).
func (g *gzipWriter) WriteHeader(code int) {
	g.Header().Del("Content-Length")
	g.ResponseWriter.WriteHeader(code)
}

// blockSimpleCrossOriginWrites rejects a state-changing request (anything but
// GET/HEAD/OPTIONS) whose Content-Type is not application/json.
//
// There is no CSRF token and no CORS configuration on this server. A gin
// handler binds JSON regardless of the request's declared Content-Type, so a
// cross-origin page can reach postOrder/setTrading/putSettings/etc. with an
// ordinary <form> or a fetch() using one of the three MIME types a browser
// allows without a CORS preflight (application/x-www-form-urlencoded,
// multipart/form-data, text/plain) — a "simple request" the browser sends
// straight through, no permission asked. None of those three are
// application/json, so requiring it here closes that path: a legitimate
// same-origin caller (the bundled UI) already sends application/json for
// every JSON body, and any cross-origin fetch that tries to send
// application/json instead is itself a "not simple" request that requires a
// CORS preflight — which this server does not answer with an
// Access-Control-Allow-Origin header, so the browser blocks it before it
// ever arrives here. This does not require an auth_token to be configured:
// it holds even in the (see CRIT-11) unauthenticated-by-default case. See
// CODE_REVIEW_FINDINGS.md's CSRF finding.
func blockSimpleCrossOriginWrites() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			c.Next()

			return
		}

		ct := strings.ToLower(c.GetHeader("Content-Type"))
		if ct == "" && c.Request.ContentLength <= 0 {
			// No body at all (e.g. POST /api/screener/rescreen, POST
			// /api/protections/resume) — nothing a form/text CSRF payload
			// could carry anyway.
			c.Next()

			return
		}

		if !strings.HasPrefix(ct, "application/json") {
			c.AbortWithStatusJSON(http.StatusUnsupportedMediaType, gin.H{
				"error": "Content-Type must be application/json for this request",
			})

			return
		}

		c.Next()
	}
}

// maxRequestBodyBytes bounds a request body this API ever legitimately needs
// to accept — the largest is a full settings section or strategy config, a
// few KB of JSON — with generous headroom.
const maxRequestBodyBytes = 1 << 20 // 1 MiB

// limitRequestBody caps how many bytes a handler will read from the request
// body, so a client that trickles an unbounded body cannot hold a connection
// (and the goroutine reading it) open indefinitely. The server sets no
// ReadTimeout at all (see Run) because the SSE stream is long-lived and
// would be killed by one — this is the size-based half of that gap instead
// of the time-based half, which would need a per-route deadline (excluding
// SSE) to close fully; not attempted in this pass. Requests without a body
// (GET, the SSE stream) are unaffected: http.MaxBytesReader only limits
// what is actually read. See CODE_REVIEW_FINDINGS.md's "no request body
// size limit or ReadTimeout".
func limitRequestBody() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBodyBytes)
		c.Next()
	}
}

// tokenParamPattern matches a "token=" query parameter's value, to redact it
// from anything gin.Recovery logs.
var tokenParamPattern = regexp.MustCompile(`token=[^&\s"]+`)

// tokenScrubbingWriter wraps an io.Writer and redacts any "token=" query
// value before the bytes reach it.
//
// gin.Recovery dumps the raw request line (which carries the query string) on
// every panic that looks like a broken connection, in every gin mode — not
// only DebugMode. requireToken accepts the API token as ?token=, for
// EventSource requests, which cannot set an Authorization header. Without
// this, that one code path still writes the live token to stderr even after
// the server is switched to ReleaseMode (see Router).
type tokenScrubbingWriter struct{ w io.Writer }

func (t tokenScrubbingWriter) Write(p []byte) (int, error) {
	if _, err := t.w.Write(tokenParamPattern.ReplaceAll(p, []byte("token=REDACTED"))); err != nil {
		return 0, err
	}

	return len(p), nil
}

// sseTokenPaths are the only routes where a bearer token may be supplied as
// a ?token= query parameter instead of an Authorization header — needed
// because the browser's native EventSource API cannot set custom headers,
// and the legacy /web UI's live-events stream still uses it (the v2 UI's
// own stream reads via fetch precisely so it can send the header instead,
// per app.js's own comment). Every other route requires the header: a
// query parameter travels in the URL, so it leaks into browser history, the
// Referer header on any same-origin navigation away from the page, and
// server/proxy access logs — acceptable for the one endpoint with no
// alternative, not for every route under /api and /v2 (which has no SSE
// route of its own at all). See CODE_REVIEW_FINDINGS.md's
// token-in-query-param finding.
var sseTokenPaths = map[string]bool{
	"/api/events": true,
}

// requireToken authenticates /api requests against token when it is non-empty.
// The token may be supplied as "Authorization: Bearer <token>", or — only on
// sseTokenPaths — a ?token= query parameter.
func requireToken(token string) gin.HandlerFunc {
	return func(c *gin.Context) {
		var presented string
		if sseTokenPaths[c.FullPath()] {
			presented = c.Query("token")
		}

		if h := c.GetHeader("Authorization"); presented == "" && strings.HasPrefix(h, "Bearer ") {
			presented = strings.TrimPrefix(h, "Bearer ")
		}

		if subtleEqual(presented, token) {
			c.Next()

			return
		}

		c.AbortWithStatusJSON(
			http.StatusUnauthorized,
			gin.H{"error": "unauthorized: missing or invalid API token"},
		)
	}
}

// subtleEqual compares two strings in constant time to avoid leaking the
// token via timing. Uses the standard library's audited implementation
// rather than a hand-rolled XOR-accumulate loop — the hand-rolled version
// was already correct, but there is no reason to carry custom
// constant-time-comparison code an auditor has to re-verify when the
// stdlib already provides it. See CODE_REVIEW_FINDINGS.md's "a hand-rolled
// constant-time compare (correct, but should use crypto/subtle)".
func subtleEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
