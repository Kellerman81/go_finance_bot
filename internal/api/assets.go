package api

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"net/http"
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
}

// indexAsset is the compiled UI page, built once at package init.
var indexAsset = buildIndexAsset()

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
	return staticAsset{raw: raw, gz: buf.Bytes(), etag: fmt.Sprintf("\"%x\"", sum[:16])}
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
	c.Header("Content-Type", "text/html; charset=utf-8")
	if strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") {
		c.Header("Content-Encoding", "gzip")
		c.Header("Vary", "Accept-Encoding")
		c.Data(http.StatusOK, "text/html; charset=utf-8", a.gz)
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", a.raw)
}

// gzipJSON compresses eligible /api JSON responses. It never compresses the SSE
// stream (which must flush unbuffered) and only engages when the client accepts
// gzip.
func gzipJSON() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.FullPath() == "/api/events" || !strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") {
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

func (g *gzipWriter) Write(b []byte) (int, error) { return g.gz.Write(b) }

func (g *gzipWriter) WriteString(s string) (int, error) { return g.gz.Write([]byte(s)) }

// WriteHeader strips Content-Length, which no longer matches once the body is
// compressed (the response falls back to chunked transfer encoding).
func (g *gzipWriter) WriteHeader(code int) {
	g.Header().Del("Content-Length")
	g.ResponseWriter.WriteHeader(code)
}

// requireToken authenticates /api requests against token when it is non-empty.
// The token may be supplied as "Authorization: Bearer <token>" or a ?token=
// query parameter (the latter for EventSource, which cannot set headers).
func requireToken(token string) gin.HandlerFunc {
	return func(c *gin.Context) {
		presented := c.Query("token")
		if h := c.GetHeader("Authorization"); presented == "" && strings.HasPrefix(h, "Bearer ") {
			presented = strings.TrimPrefix(h, "Bearer ")
		}
		if subtleEqual(presented, token) {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized: missing or invalid API token"})
	}
}

// subtleEqual compares two strings in constant time to avoid leaking the token
// via timing.
func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
