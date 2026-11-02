package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// redirectTransport sends every request to a test server, keeping the path,
// so a sink with a hard-coded endpoint can be checked end to end.
type redirectTransport struct{ target *url.URL }

func (rt redirectTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.URL.Scheme = rt.target.Scheme
	r2.URL.Host = rt.target.Host

	return http.DefaultTransport.RoundTrip(r2)
}

// TestTelegramSinkPostsTheBotAPIForm guards the Telegram sink's wire format,
// which nothing else exercises (TestTelegramAndDiscordSinks only reaches the
// Discord half): the Bot API wants the token in the PATH and a form-encoded
// chat_id/text body, and an over-long message is refused with a 400 — the
// alert is then lost, so the text must arrive capped (NT2-5).
func TestTelegramSinkPostsTheBotAPIForm(t *testing.T) {
	t.Parallel()

	var (
		gotPath string
		gotCT   string
		gotForm url.Values
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotForm, _ = url.ParseQuery(string(b))
	}))
	t.Cleanup(srv.Close)

	target, _ := url.Parse(srv.URL)
	hc := &http.Client{Transport: redirectTransport{target}}

	send := telegramSink("123:SECRET", "-42")

	err := send(context.Background(), hc,
		Event{Kind: KindHalt, Title: "trading halted", Text: strings.Repeat("x", 3*telegramMaxLen)})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	if gotPath != "/bot123:SECRET/sendMessage" {
		t.Errorf("path = %q, want the token in the Bot API path", gotPath)
	}

	if gotCT != "application/x-www-form-urlencoded" {
		t.Errorf("content type = %q", gotCT)
	}

	if gotForm.Get("chat_id") != "-42" || gotForm.Get("disable_web_page_preview") != "true" {
		t.Errorf("form = %v, want chat_id and disable_web_page_preview", gotForm)
	}

	text := gotForm.Get("text")
	if !strings.Contains(text, "trading halted") {
		t.Errorf("text = %.60q…, want the title", text)
	}

	if n := len([]rune(text)); n > telegramMaxLen {
		t.Errorf("text is %d runes, want <= %d (telegramMaxLen)", n, telegramMaxLen)
	}
}
