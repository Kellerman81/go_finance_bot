package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// fetch performs one GET against the router and returns the recorder.
func fetch(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	return rec
}

// postForm submits a form to the v2 form endpoint.
func postForm(t *testing.T, s *Server, path string, values url.Values) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	return rec
}

// ----- the parallel-deployment promise -----

// The original single-page dashboard was removed (UI-1): it had no CSP, a
// known stored-XSS path to the auth token it held as a JS global, an auth flow
// that prompted once per failed fetch, and it had silently fallen behind — no
// autotune or test_mode settings existed in it at all.
//
// The root must still go somewhere useful, so a bookmark on / is not a 404.
func TestRootRedirectsToTheV2UI(t *testing.T) {
	s := newTestServer(t)

	rec := fetch(t, s, "/")
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("GET / = %d, want a permanent redirect", rec.Code)
	}

	if got := rec.Header().Get("Location"); got != "/v2/" {
		t.Errorf("GET / redirects to %q, want /v2/", got)
	}
}

func TestV2ShellIsServed(t *testing.T) {
	s := newTestServer(t)

	rec := fetch(t, s, "/v2/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v2/ = %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `src="/v2/static/app.js"`) {
		t.Error("the shell does not load the client script")
	}

	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", got)
	}
}

func TestV2BareRedirects(t *testing.T) {
	s := newTestServer(t)

	if rec := fetch(t, s, "/v2"); rec.Code != http.StatusMovedPermanently {
		t.Fatalf("GET /v2 = %d, want a redirect to /v2/", rec.Code)
	}
}

// ----- every screen renders -----

// A template that references a field that has been renamed fails at execution,
// not at parse. Rendering each one is the only way to catch that, and a screen
// that 500s is a screen the operator cannot use.
func TestEveryFragmentRenders(t *testing.T) {
	s := newTestServer(t)

	for _, name := range []string{
		"pills", "dashboard", "activity", "chart", "positions", "trades",
		"watchlist", "strategy", "optimizeField", "symbolOptimize", "settings",
		"diagnostics", "login",
	} {
		rec := fetch(t, s, "/v2/f/"+name)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /v2/f/%s = %d: %s", name, rec.Code,
				strings.TrimSpace(rec.Body.String()))
		}
	}
}

// Fragments render in every language, so a catalog whose placeholder was
// mistyped is caught here rather than by whoever switched language.
func TestFragmentsRenderInEveryLanguage(t *testing.T) {
	s := newTestServer(t)

	for lang := range catalogs {
		for _, name := range []string{
			"dashboard", "settings", "strategy", "optimizeField", "symbolOptimize", "diagnostics",
		} {
			rec := fetch(t, s, "/v2/f/"+name+"?lang="+lang)
			if rec.Code != http.StatusOK {
				t.Errorf("GET /v2/f/%s in %q = %d: %s", name, lang, rec.Code,
					strings.TrimSpace(rec.Body.String()))
			}
		}
	}
}

func TestUnknownFragmentIs404(t *testing.T) {
	s := newTestServer(t)

	if rec := fetch(t, s, "/v2/f/nonsense"); rec.Code != http.StatusNotFound {
		t.Fatalf("GET an unknown fragment = %d, want 404", rec.Code)
	}
}

// ----- static assets -----

func TestV2StaticAssets(t *testing.T) {
	s := newTestServer(t)

	for path, want := range map[string]string{
		"/v2/static/app.css": "text/css",
		"/v2/static/app.js":  "text/javascript",
	} {
		rec := fetch(t, s, path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d", path, rec.Code)

			continue
		}

		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, want) {
			t.Errorf("%s Content-Type = %q, want %q", path, got, want)
		}

		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s served without nosniff", path)
		}
	}
}

// Templates and translation catalogs are rendered, never served. Exposing the
// template source would hand an attacker the shape of every internal value.
func TestTemplatesAreNotServed(t *testing.T) {
	s := newTestServer(t)

	for _, path := range []string{
		"/v2/static/../templates/layout.html",
		"/v2/static/templates/layout.html",
		"/v2/static/i18n/en.json",
	} {
		if rec := fetch(t, s, path); rec.Code == http.StatusOK {
			t.Errorf("GET %s was served; templates and catalogs must not be", path)
		}
	}
}

func TestV2AssetsAreETagged(t *testing.T) {
	s := newTestServer(t)

	etag := fetch(t, s, "/v2/static/app.js").Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on a v2 asset")
	}

	req := httptest.NewRequest(http.MethodGet, "/v2/static/app.js", nil)
	req.Header.Set("If-None-Match", etag)

	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotModified {
		t.Errorf("a matching ETag returned %d, want 304", rec.Code)
	}
}

// ----- the security headers -----

func TestV2SecurityHeaders(t *testing.T) {
	s := newTestServer(t)
	rec := fetch(t, s, "/v2/")

	for header, value := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
		"X-Frame-Options":        "DENY",
	} {
		if got := rec.Header().Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}

	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("no Content-Security-Policy on the v2 shell")
	}

	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("CSP allows unsafe execution: %s", csp)
	}

	for _, directive := range []string{
		"script-src 'self'", "style-src 'self'", "connect-src 'self'",
		"frame-ancestors 'none'", "base-uri 'none'",
	} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP is missing %q: %s", directive, csp)
		}
	}
}

// The strict policy is only viable because the rendered page carries no inline
// script or style. A template that adds one breaks the page in a browser but not
// in Go, so it is asserted here.
func TestRenderedPageHasNoInlineCode(t *testing.T) {
	s := newTestServer(t)

	// Comments legitimately discuss <script>; strip them before looking.
	body := regexp.MustCompile(`(?s)<!--.*?-->`).
		ReplaceAllString(fetch(t, s, "/v2/").Body.String(), "")

	for _, tag := range regexp.MustCompile(`(?s)<script[^>]*>(.*?)</script>`).
		FindAllStringSubmatch(body, -1) {
		if !strings.Contains(tag[0], " src=") {
			t.Errorf("the page contains an inline <script>: %.60s", tag[0])
		}

		if strings.TrimSpace(tag[1]) != "" {
			t.Errorf("a <script> tag has a body: %.60s", tag[1])
		}
	}

	if regexp.MustCompile(`\sstyle\s*=|<style`).MatchString(body) {
		t.Error("the page contains inline style, which the CSP forbids")
	}

	if strings.Contains(body, "onclick=") || strings.Contains(body, "onload=") {
		t.Error("the page contains an inline event handler, which the CSP forbids")
	}
}

// Server-rendered HTML is only safe because html/template escapes contextually.
// A symbol that contains markup must arrive as text.
func TestRenderedValuesAreEscaped(t *testing.T) {
	s := newTestServer(t)

	hostile := `<script>alert(1)</script>`
	if err := s.eng.AddSymbols(hostile); err != nil {
		t.Skipf("engine refused the hostile symbol (%v) — nothing to escape", err)
	}

	body := fetch(t, s, "/v2/f/watchlist").Body.String()
	if strings.Contains(body, hostile) {
		t.Error("a hostile symbol reached the page unescaped")
	}
}

// ----- forms -----

// A form post must reach the same validation the JSON API uses. The scale-in cap
// is the sharpest example: without it, averaging down is a martingale.
func TestFormRejectsWhatTheAPIRejects(t *testing.T) {
	s := newTestServer(t)

	rec := postForm(t, s, "/v2/form/settings/entries", url.Values{
		"allow_pyramiding":        {"false"},
		"skip_full_slots":         {"false"},
		"scale_in.enabled":        {"true"},
		"scale_in.step_pct":       {"0.04"},
		"scale_in.max_adds":       {"0"},
		"scale_in.size_pct":       {"0.5"},
		"scale_in.require_signal": {"true"},
	})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an uncapped scale-in ladder was accepted: %d (%s)", rec.Code, rec.Body)
	}

	if !strings.Contains(rec.Body.String(), "max_adds") {
		t.Errorf("the refusal does not name the offending field: %s", rec.Body)
	}
}

// A valid post applies, and the answer says so.
func TestFormApplies(t *testing.T) {
	s := newTestServer(t)

	rec := postForm(t, s, "/v2/form/settings/costs", url.Values{
		"commission_pct":  {"0.002"},
		"commission_flat": {"1"},
		"commission_min":  {"2"},
		"spread_pct":      {"0.0005"},
		"slippage_pct":    {"0.0002"},
		"measure_spread":  {"true"},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("PUT costs via form = %d: %s", rec.Code, rec.Body)
	}

	got := s.eng.CostsConfig()
	if got.CommissionPct != 0.002 || !got.MeasureSpread {
		t.Errorf("costs not applied: %+v", got)
	}
}

// A duration typed with a day unit is the mistake people actually make, and the
// message has to say so rather than reporting a parse failure.
func TestFormRejectsADayUnit(t *testing.T) {
	s := newTestServer(t)

	rec := postForm(t, s, "/v2/form/settings/exits", url.Values{
		"enabled":  {"true"},
		"max_hold": {"3d"},
	})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a 3d duration was accepted: %d", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), "72h") {
		t.Errorf("the message does not suggest the working form: %s", rec.Body)
	}
}

func TestUnknownFormIs404(t *testing.T) {
	s := newTestServer(t)

	if rec := postForm(t, s, "/v2/form/nonsense", url.Values{}); rec.Code != http.StatusNotFound {
		t.Fatalf("an unknown form = %d, want 404", rec.Code)
	}
}

// ----- the auth probe -----

// The client cannot tell "no token configured" from "token missing" without
// this, and would either nag for a token nobody set or fail every request.
func TestHealthReportsWhetherAuthIsRequired(t *testing.T) {
	open := newTestServer(t)

	var body map[string]any
	if err := json.Unmarshal(fetch(t, open, "/health").Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /health: %v", err)
	}

	if body["auth_required"] != false {
		t.Errorf("auth_required = %v on an unprotected server, want false", body["auth_required"])
	}

	locked := newTestServer(t)
	locked.authToken = "secret"

	if err := json.Unmarshal(fetch(t, locked, "/health").Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /health: %v", err)
	}

	if body["auth_required"] != true {
		t.Errorf("auth_required = %v on a protected server, want true", body["auth_required"])
	}
}

func TestHealthNeedsNoToken(t *testing.T) {
	s := newTestServer(t)
	s.authToken = "secret"

	if rec := fetch(t, s, "/health"); rec.Code != http.StatusOK {
		t.Fatalf("GET /health = %d on a protected server, want 200", rec.Code)
	}
}

// Fragments carry the same data the API does, so they must be behind the same
// token. Serving them unauthenticated would make the token pointless.
func TestFragmentsRequireTheToken(t *testing.T) {
	s := newTestServer(t)
	s.authToken = "secret"

	if rec := fetch(t, s, "/v2/f/dashboard"); rec.Code != http.StatusUnauthorized {
		t.Errorf("an unauthenticated fragment request = %d, want 401", rec.Code)
	}

	// The shell itself may be served: it contains no data, and the client needs
	// it to render the login screen.
	if rec := fetch(t, s, "/v2/"); rec.Code != http.StatusOK {
		t.Errorf("the shell = %d on a protected server, want 200", rec.Code)
	}
}

// The login fragment is the one screen a client with no token must be able to
// reach — it is what the client's 401 handler shows. Gating it behind the
// same token it exists to collect creates an unbounded retry loop: fetch
// login → 401 → show login → fetch login → 401 → ...
func TestLoginFragmentNeedsNoToken(t *testing.T) {
	s := newTestServer(t)
	s.authToken = "secret"

	rec := fetch(t, s, "/v2/f/login")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v2/f/login on a protected server = %d, want 200", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), "token") {
		t.Errorf("the login fragment does not look like a login form: %s", rec.Body)
	}
}

// Every other fragment must still require the token — only login is special.
func TestLoginFragmentDoesNotBypassAuthForOthers(t *testing.T) {
	s := newTestServer(t)
	s.authToken = "secret"

	for _, name := range []string{"dashboard", "settings", "strategy"} {
		if rec := fetch(t, s, "/v2/f/"+name); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET /v2/f/%s = %d, want 401", name, rec.Code)
		}
	}
}

func TestFormsRequireTheToken(t *testing.T) {
	s := newTestServer(t)
	s.authToken = "secret"

	rec := postForm(t, s, "/v2/form/settings/costs", url.Values{"commission_pct": {"0.5"}})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated form post = %d, want 401", rec.Code)
	}
}

// ----- the translation catalogs -----

// Every language must define the keys English does. A missing key falls back to
// English at runtime, so this is not fatal — but a silently half-translated
// interface is worth failing a build over.
func TestCatalogsAgree(t *testing.T) {
	en := catalogs[fallbackLang]

	for lang, other := range catalogs {
		if lang == fallbackLang {
			continue
		}

		var missing []string

		for key := range en {
			if strings.HasPrefix(key, "_") {
				continue
			}

			if _, ok := other[key]; !ok {
				missing = append(missing, key)
			}
		}

		if len(missing) > 0 {
			t.Errorf("%s is missing %d key(s), e.g. %v",
				lang, len(missing), missing[:min(5, len(missing))])
		}

		for key := range other {
			if strings.HasPrefix(key, "_") {
				continue
			}

			if _, ok := en[key]; !ok {
				t.Errorf("%s defines %q, which English does not — the fallback would never find it",
					lang, key)
			}
		}
	}
}

// A placeholder that exists in one language and not another produces a literal
// "{count}" on screen for half the users.
func TestCatalogPlaceholdersMatch(t *testing.T) {
	en := catalogs[fallbackLang]
	ph := regexp.MustCompile(`\{(\w+)\}`)

	for lang, other := range catalogs {
		if lang == fallbackLang {
			continue
		}

		for key, text := range en {
			if strings.HasPrefix(key, "_") {
				continue
			}

			translated, ok := other[key]
			if !ok {
				continue
			}

			want := ph.FindAllString(text, -1)
			got := ph.FindAllString(translated, -1)

			if len(want) != len(got) {
				t.Errorf("%s/%s: placeholders differ — en %v, %s %v",
					lang, key, want, lang, got)
			}
		}
	}
}

// Every label and help string a form declares must exist, or the operator gets a
// dotted key where an explanation should be.
func TestEveryFieldHasTranslations(t *testing.T) {
	en := catalogs[fallbackLang]

	check := func(prefix string, fields []field) {
		for _, fl := range fields {
			for _, suffix := range []string{"label", "help"} {
				key := prefix + "." + fl.Name + "." + suffix
				if _, ok := en[key]; !ok {
					t.Errorf("no translation for %q", key)
				}
			}
		}
	}

	for _, sec := range settingsSections {
		check("settings."+sec.Name, sec.Fields)

		for _, suffix := range []string{"title", "intro"} {
			if _, ok := en["settings."+sec.Name+"."+suffix]; !ok {
				t.Errorf("no translation for %q", "settings."+sec.Name+"."+suffix)
			}
		}

		if sec.Danger {
			if _, ok := en["settings."+sec.Name+".danger"]; !ok {
				t.Errorf("section %q is marked dangerous but has no warning text", sec.Name)
			}
		}
	}

	check("strategy", strategyFields)
	check("settings.limits", limitFields)
	check("settings.sizing", sizingFields)
	check("settings.behavior", behaviorFields)
}

// ----- the template/client contract -----

// Every data- attribute the templates use must be handled by the client.
//
// This exists because of a bug it would have caught: the tab bar was rendered
// with data-nav and the client never listened for it, so every menu link
// silently did nothing. Nothing failed — not the build, not a template, not a
// request. A missing handler has no symptom except a dead interface, which is
// precisely the kind of failure a test has to cover.
func TestEveryTemplateAttributeIsHandled(t *testing.T) {
	client := clientSource(t)
	used := make(map[string]bool)

	// Behaviour attributes only: data-target, data-body and data-confirm are
	// arguments to a behaviour rather than triggers of one.
	arguments := map[string]bool{
		"data-target": true, "data-body": true, "data-confirm": true,
		"data-done": true, "data-form-status": true, "data-theme": true,
		"data-up": true, "data-down": true, "data-help": true,
		"data-fill-value": true,
	}

	attr := regexp.MustCompile(`\bdata-[a-z-]+`)

	for name, asset := range v2Assets {
		if !strings.HasSuffix(name, ".html") {
			continue
		}

		for _, found := range attr.FindAllString(string(asset.raw), -1) {
			if !arguments[found] {
				used[found] = true
			}
		}
	}

	if len(used) == 0 {
		t.Fatal("no data- attributes found in the templates; this test is not testing anything")
	}

	// Only the dispatch sites count. Searching the whole file would not have
	// caught the original bug: data-nav appeared in navigate(), which sets
	// aria-current on the tabs — so the string was present while nothing
	// listened for a click on it.
	dispatch := functionBody(t, client, "listen") + functionBody(t, client, "wire")

	for name := range used {
		// The client reads them through the dataset API, so data-inline-get is
		// dataset.inlineGet. Either spelling counts — but the dataset form must
		// be matched with its prefix: a bare "nav" also occurs inside
		// "navigate", which is exactly how a first version of this test passed
		// while the bug was still present.
		if strings.Contains(dispatch, name) ||
			strings.Contains(dispatch, "dataset."+datasetName(name)) {
			continue
		}

		t.Errorf("templates use %s but neither listen() nor wire() dispatches on it — "+
			"whatever it drives will silently do nothing", name)
	}
}

// functionBody extracts one top-level function from the client source, by
// brace-matching from its opening brace.
func functionBody(t *testing.T, src, name string) string {
	t.Helper()

	at := strings.Index(src, "function "+name+"(")
	if at < 0 {
		t.Fatalf("the client has no function %s()", name)
	}

	open := strings.Index(src[at:], "{")
	if open < 0 {
		t.Fatalf("function %s() has no body", name)
	}

	depth := 0

	for i := at + open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--

			if depth == 0 {
				return src[at : i+1]
			}
		}
	}

	t.Fatalf("function %s() is not closed", name)

	return ""
}

// clientSource returns app.js, which is embedded alongside the templates.
func clientSource(t *testing.T) string {
	t.Helper()

	asset, ok := v2Assets["static/app.js"]
	if !ok {
		t.Fatal("static/app.js is not in the embedded tree")
	}

	return string(asset.raw)
}

// datasetName converts data-inline-get to the inlineGet the dataset API exposes.
func datasetName(attr string) string {
	parts := strings.Split(strings.TrimPrefix(attr, "data-"), "-")
	for i := 1; i < len(parts); i++ {
		parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
	}

	return strings.Join(parts, "")
}

// Every tab must resolve to a fragment. A tab naming a screen that does not
// exist looks identical to one whose handler is missing.
func TestEveryTabHasAFragment(t *testing.T) {
	s := newTestServer(t)

	for _, tab := range v2Tabs {
		if rec := fetch(t, s, "/v2/f/"+tab); rec.Code != http.StatusOK {
			t.Errorf("tab %q resolves to /v2/f/%s = %d", tab, tab, rec.Code)
		}

		if _, ok := catalogs[fallbackLang]["nav."+tab]; !ok {
			t.Errorf("tab %q has no nav.%s translation", tab, tab)
		}
	}
}

// The shell must render a button for every tab, or the screen is unreachable.
func TestShellRendersEveryTab(t *testing.T) {
	s := newTestServer(t)
	body := fetch(t, s, "/v2/").Body.String()

	for _, tab := range v2Tabs {
		if !strings.Contains(body, `data-nav="`+tab+`"`) {
			t.Errorf("the shell has no tab for %q", tab)
		}
	}
}

// ----- layout -----

// The view lays panels out as a wrapping flexbox in which every panel grows,
// so a row is always filled whatever it holds — one panel, two, or a full-width
// one. It used to be `grid-template-columns: repeat(auto-fit, minmax(340px,
// 1fr))`, which cannot do that: auto-fit collapses only EMPTY tracks, and every
// view has a `.wide` panel spanning all of them, so narrow panels stayed one
// track wide with the rest of a wide window blank beside them.
//
// (This replaces a test that banned a lone narrow panel in a view. Under flex a
// lone panel fills its row by itself, so the layout it guarded against is gone.)
func TestPanelsFillTheirRow(t *testing.T) {
	s := newTestServer(t)
	css := fetch(t, s, "/v2/static/app.css").Body.String()

	view := regexp.MustCompile(`(?s)\n\.view \{(.*?)\}`).FindStringSubmatch(css)
	if view == nil {
		t.Fatal("app.css has no .view rule")
	}

	for _, want := range []string{"display: flex", "flex-wrap: wrap"} {
		if !strings.Contains(view[1], want) {
			t.Errorf(".view is missing %q: %s", want, strings.Join(strings.Fields(view[1]), " "))
		}
	}

	panel := regexp.MustCompile(`(?s)\n\.panel \{(.*?)\}`).FindStringSubmatch(css)
	if panel == nil {
		t.Fatal("app.css has no .panel rule")
	}

	if !strings.Contains(panel[1], "flex: 1 1 ") {
		t.Errorf(".panel does not grow into its row: %s",
			strings.Join(strings.Fields(panel[1]), " "))
	}

	if !strings.Contains(css, ".panel.wide { flex-basis: 100%; }") {
		t.Error(".panel.wide no longer takes the full row")
	}
}

// A panel modifier the stylesheet does not define is a silent no-op: the panel
// still renders, looks nearly right, and quietly keeps the width it was meant
// to be given.
func TestPanelModifiersAreStyled(t *testing.T) {
	s := newTestServer(t)
	css := fetch(t, s, "/v2/static/app.css").Body.String()
	panel := regexp.MustCompile(`<(?:section|details) class="panel([^"]*)"`)

	for _, name := range v2Tabs {
		body := fetch(t, s, "/v2/f/"+name).Body.String()

		for _, m := range panel.FindAllStringSubmatch(body, -1) {
			for _, mod := range strings.Fields(m[1]) {
				if !strings.Contains(css, ".panel."+mod+" {") {
					t.Errorf("view %q puts %q on a panel, but app.css has no .panel.%s rule",
						name, mod, mod)
				}
			}
		}
	}
}

// Every form renders its fields into the grid wrapper. Without it they stack in
// one column, which for the 39-field strategy block is unusable.
func TestFormsUseTheFieldGrid(t *testing.T) {
	s := newTestServer(t)

	for _, name := range []string{"strategy", "settings"} {
		body := fetch(t, s, "/v2/f/"+name).Body.String()

		forms := strings.Count(body, "<form data-form")
		grids := strings.Count(body, `class="fields"`)

		if forms == 0 {
			t.Errorf("view %q renders no form", name)

			continue
		}

		if grids != forms {
			t.Errorf("view %q has %d form(s) but %d field grid(s)", name, forms, grids)
		}
	}
}

// ----- asset integrity -----

// No embedded text asset may contain a stray control character.
//
// This exists because one reached the browser: a CSS escape written as "\25b8"
// was mangled into a raw 0x15 byte followed by the literal text "b8", so every
// settings block was labelled "b8 Screener". Nothing failed — it is valid CSS,
// valid UTF-8 and a perfectly serveable file. Only a human looking at the page
// could tell, which is exactly when a test should be doing the looking.
func TestAssetsHaveNoControlCharacters(t *testing.T) {
	for name, asset := range v2Assets {
		if !isTextAsset(name) {
			continue
		}

		for i, r := range string(asset.raw) {
			if r < 0x20 && r != '\n' && r != '\t' && r != '\r' {
				t.Errorf("%s contains U+%04X at byte %d — likely a mangled escape sequence",
					name, r, i)

				break
			}
		}
	}
}

// Every embedded text asset must be valid UTF-8, since they are served with a
// utf-8 charset and rendered into a page that declares one.
func TestAssetsAreValidUTF8(t *testing.T) {
	for name, asset := range v2Assets {
		if isTextAsset(name) && !utf8.Valid(asset.raw) {
			t.Errorf("%s is not valid UTF-8", name)
		}
	}
}

// isTextAsset reports whether a file is one whose bytes are meant to be read.
func isTextAsset(name string) bool {
	for _, ext := range []string{".css", ".js", ".html", ".json", ".svg"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}

	return false
}
