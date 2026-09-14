package api

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/gin-gonic/gin"
)

// The v2 web interface.
//
// It is served alongside the original page rather than replacing it: the old UI
// stays at "/" and this one at "/v2/". Both talk to the same API, so nothing has
// to be migrated in one step and a regression in the new page never leaves the
// bot unmanageable.
//
// Three things differ structurally from the original single-file page, and each
// is a property this loader has to preserve:
//
//   - It is split into ES modules and one stylesheet, with NOTHING inline. That
//     is what makes the strict Content-Security-Policy below possible: with no
//     inline script or style to allow, an injected <script> cannot execute even
//     if data ever reaches the DOM as markup.
//   - Views load on demand. The original built every panel on boot; here the
//     router imports a view module the first time its tab is opened, and only
//     the visible view polls.
//   - All user-visible text comes from a translation catalog fetched at runtime,
//     so adding a language is a JSON file rather than a code change.

// v2FS holds the whole v2 asset tree.
//
//go:embed all:web2
var v2FS embed.FS

// v2Root is the embedded directory the tree lives in.
const v2Root = "web2"

// v2Prefix is the URL prefix the tree is served under.
const v2Prefix = "/v2/"

// v2CSP is the Content-Security-Policy for the new UI.
//
// 'self' only, with no 'unsafe-inline' anywhere — the page carries no inline
// script or style, so the browser can refuse both outright. connect-src is
// 'self' because every request the page makes goes to this bot's own API; a page
// that cannot reach any other origin cannot exfiltrate an API token to one.
const v2CSP = "default-src 'none'; " +
	"script-src 'self'; " +
	"style-src 'self'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"form-action 'none'; " +
	"frame-ancestors 'none'; " +
	"base-uri 'none'"

// v2Assets is the compiled asset tree, built once at package init.
var v2Assets = buildV2Assets()

// buildV2Assets walks the embedded tree and precomputes each file's gzip copy,
// content-hash ETag and content type. Doing it once at startup keeps request
// handling to a map lookup and a write.
func buildV2Assets() map[string]staticAsset {
	out := make(map[string]staticAsset)

	err := fs.WalkDir(v2FS, v2Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		raw, readErr := v2FS.ReadFile(p)
		if readErr != nil {
			return readErr
		}

		out[strings.TrimPrefix(p, v2Root+"/")] = compress(raw, contentTypeOf(p))

		return nil
	})
	if err != nil {
		// Embedded at build time — a walk failure is a programming error.
		panic("api: read embedded web2 tree: " + err.Error())
	}

	return out
}

// compress precomputes one asset's gzip copy and ETag.
func compress(raw []byte, mime string) staticAsset {
	var buf bytes.Buffer

	gw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)

	_, _ = gw.Write(raw)
	_ = gw.Close()

	sum := sha256.Sum256(raw)

	return staticAsset{
		raw:  raw,
		gz:   buf.Bytes(),
		etag: fmt.Sprintf("\"%x\"", sum[:16]),
		mime: mime,
	}
}

// contentTypeOf maps a file extension to its media type. The set is closed on
// purpose: an asset this bot does not ship should not be served with a guessed
// type.
func contentTypeOf(p string) string {
	switch path.Ext(p) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	default:
		return "application/octet-stream"
	}
}

// serveV2Static serves one file from web2/static — the stylesheet and the
// client script, and nothing else. Templates and translation catalogs are
// rendered, never served.
func (*Server) serveV2Static(c *gin.Context) {
	name := "static/" + strings.TrimPrefix(c.Param("filepath"), "/")

	asset, ok := v2Assets[name]
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "no such asset"})

		return
	}

	v2SecurityHeaders(c)
	asset.serve(c)
}

// v2SecurityHeaders sets the policy headers for every v2 response.
//
// These belong on the document and its assets rather than on /api: the API is
// consumed by scripts and other tools, while these headers describe how a
// *browser* should treat this page.
func v2SecurityHeaders(c *gin.Context) {
	c.Header("Content-Security-Policy", v2CSP)
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("X-Frame-Options", "DENY")
	// The UI needs none of these; denying them shrinks what a compromised page
	// could ask the browser for.
	c.Header("Permissions-Policy", "geolocation=(), microphone=(), camera=(), payment=()")
}
