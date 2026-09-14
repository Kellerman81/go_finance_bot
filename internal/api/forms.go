package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Form posts from the v2 interface.
//
// Each of these decodes the submitted fields by their declared type and hands
// the result to the JSON handler that already exists — so validation, redaction
// and the live-apply semantics are the same code the API uses, not a parallel
// copy. The response is a plain sentence the client shows next to the button.

// handleV2Form dispatches a form post to the endpoint behind it.
func (s *Server) handleV2Form(c *gin.Context) {
	name := strings.TrimPrefix(c.Param("name"), "/")
	t := translator(langOf(c))

	switch {
	case name == "watchlist":
		s.formWatchlist(c, t)

	case strings.HasPrefix(name, "settings/"):
		section := strings.TrimPrefix(name, "settings/")

		fields, ok := sectionFields(section)
		if !ok {
			c.String(http.StatusNotFound, "unknown settings section %q", section)

			return
		}

		s.replayAsJSON(c, t, fields, "/api/settings/"+section, s.putSettings,
			gin.Params{{Key: "section", Value: section}})

	case name == "strategy":
		s.replayAsJSON(c, t, strategyFields, "/api/strategy", s.putStrategy, nil)

	case name == "limits":
		s.replayAsJSON(c, t, limitFields, "/api/limits", s.putLimits, nil)

	case name == "sizing":
		s.replayAsJSON(c, t, sizingFields, "/api/sizing", s.putSizing, nil)

	case name == "behavior":
		s.replayAsJSON(c, t, behaviorFields, "/api/behavior", s.putBehavior, nil)

	default:
		c.String(http.StatusNotFound, "unknown form %q", name)
	}
}

// sectionFields looks up a settings section's declarations.
func sectionFields(name string) ([]field, bool) {
	for _, sec := range settingsSections {
		if sec.Name == name {
			return sec.Fields, true
		}
	}

	return nil, false
}

// replayAsJSON turns the form into the JSON body an existing handler expects,
// runs that handler against a throwaway context, and reports its answer.
//
// Reusing the handler rather than reimplementing it is the whole point. Every
// rule about what a valid setting is — the ladder that may not sell more than
// the position, the scale-in cap, the redaction marker that means "keep what you
// have" — lives in one place and keeps applying.
func (*Server) replayAsJSON(
	c *gin.Context,
	t func(string, ...any) string,
	fields []field,
	path string,
	handler gin.HandlerFunc,
	params gin.Params,
) {
	body, err := decodeForm(c, fields)
	if err != nil {
		c.String(http.StatusBadRequest, "%v", err)

		return
	}

	raw, err := json.Marshal(body)
	if err != nil {
		c.String(http.StatusInternalServerError, "%v", err)

		return
	}

	inner, recorder := replayContext(c, path, raw, params)

	handler(inner)

	c.Status(recorder.Code)
	c.String(recorder.Code, "%s", summarise(recorder, t))
}

// replayContext builds a gin context carrying a JSON body, plus the recorder its
// response lands in.
//
// Every replayed endpoint is a PUT; the method is fixed rather than passed so a
// caller cannot accidentally replay one as something else.
func replayContext(
	c *gin.Context,
	path string,
	raw []byte,
	params gin.Params,
) (*gin.Context, *responseRecorder) {
	req, _ := http.NewRequestWithContext(
		c.Request.Context(), http.MethodPut, path, bytes.NewReader(raw))

	req.Header.Set("Content-Type", "application/json")

	recorder := &responseRecorder{header: http.Header{}, Code: http.StatusOK}

	inner, _ := gin.CreateTestContext(recorder)

	inner.Request = req
	inner.Params = params

	return inner, recorder
}

// summarise turns a handler's JSON response into one line for the operator.
func summarise(rec *responseRecorder, t func(string, ...any) string) string {
	var payload struct {
		Error   string `json:"error"`
		Note    string `json:"note"`
		Applied string `json:"applied"`
	}

	_ = json.Unmarshal(rec.Body.Bytes(), &payload)

	// Handlers that return their own JSON shape report a failed save in a
	// header instead of the body; both end up in the same line of text here.
	note := payload.Note
	if warn := rec.header.Get(saveWarningHeader); warn != "" {
		note = joinNote(note, warn)
	}

	switch {
	case payload.Error != "":
		return payload.Error
	case rec.Code >= http.StatusBadRequest:
		return strings.TrimSpace(rec.Body.String())
	case note != "":
		return t("msg.applied") + " — " + note
	default:
		return t("msg.saved")
	}
}

// responseRecorder captures a handler's response without writing it out.
type responseRecorder struct {
	Code   int
	Body   bytes.Buffer
	header http.Header
}

// Header returns the recorder's header map.
func (r *responseRecorder) Header() http.Header { return r.header }

// Write records the body.
func (r *responseRecorder) Write(b []byte) (int, error) { return r.Body.Write(b) }

// WriteHeader records the status.
func (r *responseRecorder) WriteHeader(code int) { r.Code = code }

// formWatchlist adds symbols. It has its own handler because the API takes a
// JSON array where the form offers a comma-separated field.
func (s *Server) formWatchlist(c *gin.Context, t func(string, ...any) string) {
	symbols := splitStrings(strings.ToUpper(c.PostForm("symbols")))
	if len(symbols) == 0 {
		c.String(http.StatusBadRequest, "%s", t("watch.addPlaceholder"))

		return
	}

	if err := s.eng.AddSymbols(symbols...); err != nil {
		c.String(http.StatusBadRequest, "%v", err)

		return
	}

	// A pick from the search results already carries its company name, so
	// recording it here saves the background resolver a lookup — and means the
	// row reads properly the moment it appears rather than one refresh later.
	if name := strings.TrimSpace(c.PostForm("name")); name != "" && len(symbols) == 1 {
		s.eng.SetSymbolName(symbols[0], name)
	}

	c.String(http.StatusOK, "%s", t("msg.added", "symbols", strings.Join(symbols, ", ")))
}
