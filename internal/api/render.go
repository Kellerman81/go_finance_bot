package api

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// Server-side rendering for the v2 interface.
//
// The v2 UI began as a browser application: modules, a client-side router, a
// form builder, a translation engine and a canvas chart — about 2,200 lines of
// hand-written JavaScript. Almost all of it existed to turn JSON this process
// already had into HTML.
//
// So the HTML is produced here instead. Every screen is a Go template rendered
// from the same values the API returns, and the browser's job shrinks to
// fetching a fragment and putting it on the page. What is left of the client is
// about 180 lines, and none of it knows anything about trading.
//
// Three things this buys beyond size:
//
//   - **Escaping is contextual and automatic.** html/template knows whether a
//     value lands in text, an attribute or a URL and escapes accordingly. The
//     browser code had to be disciplined about never building markup from data;
//     now it cannot, because it does not build markup at all.
//   - **Validation stops being written twice.** The client had 267 lines
//     parsing durations, rung ladders and key:value maps, and range-checking
//     them — all of which Go already did on the way in. The form posts, the
//     existing handler validates, and the answer comes back rendered.
//   - **Translation is one lookup function**, not a runtime engine plus a
//     fallback chain. The catalogs are the same JSON files as before.

// tmplFS holds the templates, static assets and translation catalogs.
//
//go:embed all:web2
var tmplFS embed.FS

// catalogs maps a language code to its flat key→text table.
var catalogs = loadCatalogs()

// fallbackLang is the catalog every other language falls back to, key by key.
const fallbackLang = "en"

// languages lists the offered languages in menu order. Adding one means adding
// a JSON file and a line here.
var languages = []struct{ Code, Label string }{
	{"en", "English"},
	{"de", "Deutsch"},
}

// loadCatalogs reads every embedded translation file at startup.
func loadCatalogs() map[string]map[string]string {
	out := make(map[string]map[string]string)

	entries, err := fs.Glob(tmplFS, "web2/i18n/*.json")
	if err != nil {
		panic("api: glob translation catalogs: " + err.Error())
	}

	for _, name := range entries {
		raw, readErr := tmplFS.ReadFile(name)
		if readErr != nil {
			panic("api: read " + name + ": " + readErr.Error())
		}

		var table map[string]string

		if jsonErr := json.Unmarshal(raw, &table); jsonErr != nil {
			panic("api: parse " + name + ": " + jsonErr.Error())
		}

		code := strings.TrimSuffix(strings.TrimPrefix(name, "web2/i18n/"), ".json")

		out[code] = table
	}

	if _, ok := out[fallbackLang]; !ok {
		panic("api: no " + fallbackLang + " translation catalog")
	}

	return out
}

// translator returns the lookup function for one language.
//
// A missing key resolves against English rather than rendering as the key. A
// half-translated catalog then shows a mix of languages, which is unhelpful but
// usable; showing `settings.exits.max_hold.help` to someone deciding when to
// close a position is not.
func translator(lang string) func(string, ...any) string {
	table, ok := catalogs[lang]
	if !ok {
		table = catalogs[fallbackLang]
	}

	fallback := catalogs[fallbackLang]

	return func(key string, args ...any) string {
		text, found := table[key]
		if !found {
			if text, found = fallback[key]; !found {
				return key
			}
		}

		if len(args) == 0 {
			return text
		}

		// Named substitution: T "msg.added" "symbols" "AAPL" reads better at the
		// call site than positional verbs, and a template that passes the wrong
		// count leaves the placeholder visible instead of printing %!s(MISSING).
		for i := 0; i+1 < len(args); i += 2 {
			name, _ := args[i].(string)

			text = strings.ReplaceAll(text, "{"+name+"}", fmt.Sprint(args[i+1]))
		}

		return text
	}
}

// langOf picks the language for a request: an explicit ?lang, then the cookie
// the switcher sets, then Accept-Language, then English.
func langOf(c *gin.Context) string {
	if v := c.Query("lang"); catalogs[v] != nil {
		return v
	}

	if v, err := c.Cookie("lang"); err == nil && catalogs[v] != nil {
		return v
	}

	for _, tag := range strings.Split(c.GetHeader("Accept-Language"), ",") {
		base, _, _ := strings.Cut(strings.TrimSpace(tag), ";")

		base, _, _ = strings.Cut(base, "-")

		if catalogs[strings.ToLower(base)] != nil {
			return strings.ToLower(base)
		}
	}

	return fallbackLang
}

// templates is the parsed template set, built once at startup.
var templates = parseTemplates()

// parseTemplates compiles every template with the shared function map.
func parseTemplates() *template.Template {
	t := template.New("").Funcs(templateFuncs())

	t = template.Must(t.ParseFS(tmplFS, "web2/templates/*.html", "web2/templates/*/*.html"))

	return t
}

// renderPool reuses render buffers; a fragment is written to one, then copied to
// the response, so a template error never emits a half-written page.
var renderPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// render writes one template to the response.
//
// It renders into a buffer first. A template that fails halfway through would
// otherwise leave a truncated fragment on the page with a 200 status, which
// looks like data loss rather than an error.
func render(c *gin.Context, name string, data map[string]any) {
	lang := langOf(c)

	if data == nil {
		data = make(map[string]any)
	}

	data["Lang"] = lang
	data["Languages"] = languages
	data["T"] = translator(lang)

	buf, _ := renderPool.Get().(*bytes.Buffer)

	buf.Reset()

	defer renderPool.Put(buf)

	if err := templates.ExecuteTemplate(buf, name, data); err != nil {
		c.String(http.StatusInternalServerError, "render %s: %v", name, err)

		return
	}

	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	v2SecurityHeaders(c)

	_, _ = c.Writer.Write(buf.Bytes())
}

// templateFuncs is the formatting vocabulary the templates draw on.
//
// These replace the client's fmt.js. Numbers are formatted here rather than in
// the browser, which costs the per-viewer locale (a German operator sees a dot
// where a comma belongs) — the price of not shipping an Intl wrapper. Where it
// matters, the templates say what the unit is.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"num":     func(v any) string { return formatNum(toFloat(v), 2) },
		"num4":    func(v any) string { return formatNum(toFloat(v), 4) },
		"int":     func(v any) string { return formatNum(toFloat(v), 0) },
		"pct":     func(v any) string { return formatNum(toFloat(v), 2) + " %" },
		"pctFrac": func(v any) string { return formatNum(toFloat(v)*100, 2) + " %" },
		"money":   func(v any) string { return formatNum(toFloat(v), 2) },

		"time":     func(t time.Time) string { return zeroAware(t, "15:04:05") },
		"dateTime": func(t time.Time) string { return zeroAware(t, "2006-01-02 15:04:05") },
		"ago":      humanAgo,
		"dur":      func(d time.Duration) string { return d.String() },

		"signClass": func(v any) string {
			if toFloat(v) < 0 {
				return "loss"
			}

			return "win"
		},
		"actionClass": func(a any) string { return "act-" + fmt.Sprint(a) },

		"csv": func(v []string) string { return strings.Join(v, ", ") },
		"kv":  formatKV,
		"kvf": formatKVFloat,

		"dict": dict,
		"if_": func(cond bool, yes, no any) any {
			if cond {
				return yes
			}

			return no
		},
		"fieldValue": formValue,
		"tkey":       func(parts ...string) string { return strings.Join(parts, ".") },
		"hasKey":     func(m map[string]any, k string) bool { _, ok := m[k]; return ok },
		"add":        func(a, b int) int { return a + b },
		"deref": func(p *float64) any {
			if p == nil {
				return nil
			}

			return *p
		},
	}
}

// dict builds a map inline, so a partial can be called with named arguments.
func dict(pairs ...any) map[string]any {
	out := make(map[string]any, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		key, _ := pairs[i].(string)

		out[key] = pairs[i+1]
	}

	return out
}

// formatNum renders a float with thousands separators, or an em dash when there
// is no value to show.
func formatNum(v float64, digits int) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "—"
	}

	s := fmt.Sprintf("%.*f", digits, v)

	whole, frac, _ := strings.Cut(s, ".")
	neg := strings.HasPrefix(whole, "-")

	whole = strings.TrimPrefix(whole, "-")

	var b strings.Builder

	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(' ')
		}

		b.WriteRune(r)
	}

	out := b.String()
	if frac != "" {
		out += "." + frac
	}

	if neg {
		return "-" + out
	}

	return out
}

// zeroAware formats a timestamp, treating Go's zero time as absent.
func zeroAware(t time.Time, layout string) string {
	if t.IsZero() {
		return "—"
	}

	return t.Local().Format(layout)
}

// humanAgo renders how long since t in whole units.
func humanAgo(t time.Time) string {
	if t.IsZero() {
		return "—"
	}

	d := time.Since(t)
	if d < 0 {
		d = -d
	}

	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// formatKV renders a string map as the "a:1, b:2" form the forms accept.
func formatKV(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ":" + m[k]
	}

	return strings.Join(parts, ", ")
}

// formatKVFloat is formatKV for numeric maps (detector weights, the ROI ladder).
func formatKVFloat(m map[string]float64) string {
	if len(m) == 0 {
		return ""
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ":" + strings.TrimRight(strings.TrimRight(
			fmt.Sprintf("%.6f", m[k]), "0"), ".")
	}

	return strings.Join(parts, ", ")
}

// toFloat coerces the numeric shapes the templates receive.
func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case *float64:
		if n == nil {
			return math.NaN()
		}

		return *n

	case nil:
		return math.NaN()
	default:
		return math.NaN()
	}
}
