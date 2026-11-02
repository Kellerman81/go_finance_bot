package ai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// EL3-4: an OpenAI-compatible base_url behind basic auth carries a password
// in its userinfo, and the client's errors re-added the URL net/http had
// stripped — every 502 or timeout put the password into errlog,
// /api/errors and the advisor's LastError.
func TestAClientErrorDoesNotCarryTheURLPassword(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	url := strings.Replace(srv.URL, "http://", "http://ops:HUNTER2@", 1) + "/v1/chat/completions"

	var out map[string]any

	err := postJSON(context.Background(), srv.Client(), url, "", map[string]string{}, &out)
	if err == nil {
		t.Fatal("a 502 returned no error")
	}

	if strings.Contains(err.Error(), "HUNTER2") {
		t.Errorf("the error carries the base_url password: %v", err)
	}
}

// AI3-3: a BUY whose confidence read as 0 — an explicit 0, a negative, or a
// decimal comma ("0,8", which Sscanf read as 0) — passed min_confidence 0 and
// counted as a full vote in majority/unanimous mode. It is not a vote.
func TestAZeroConfidenceBuyIsNotAVote(t *testing.T) {
	t.Parallel()

	for _, conf := range []string{`0`, `-0.4`, `"0,8"`, `"7/10"`} {
		raw := `{"action":"BUY","confidence":` + conf + `,"reason":"x"}`
		if v, err := parseVerdict(raw); err == nil && v.Action != Hold {
			t.Errorf("confidence %s gave %s at %.2f, want no vote", conf, v.Action, v.Confidence)
		}
	}
}
