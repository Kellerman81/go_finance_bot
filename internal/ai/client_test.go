package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// capture records the request one of the completers made, so the test can
// assert the wire shape each provider expects.
type capture struct {
	path string
	auth string
	body map[string]any
}

// stubServer serves reply at any path and records the request into got.
func stubServer(t *testing.T, got *capture, status int, reply string) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)

			got.path = r.URL.Path
			got.auth = r.Header.Get("Authorization")

			_ = json.Unmarshal(b, &got.body)

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)

			_, _ = io.WriteString(w, reply)
		}),
	)
	t.Cleanup(srv.Close)

	return srv
}

// TestOllamaCompleter checks the native Ollama chat request shape (JSON-mode,
// non-streaming) and that the message content comes back.
func TestOllamaCompleter(t *testing.T) {
	t.Parallel()

	var got capture

	srv := stubServer(t, &got, http.StatusOK,
		`{"message":{"role":"assistant","content":"{\"action\":\"BUY\",\"confidence\":0.6}"}}`)

	cl, err := newCompleter("ollama", srv.URL, "llama3.2", "", 0, srv.Client())
	if err != nil {
		t.Fatal(err)
	}

	out, err := cl.Complete(t.Context(), "sys", "user")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out, `"action":"BUY"`) {
		t.Errorf("content = %q", out)
	}

	if got.path != "/api/chat" {
		t.Errorf("path = %q, want /api/chat", got.path)
	}

	if got.body["format"] != "json" {
		t.Errorf(
			"format = %v, want json (constrains the sampler to valid JSON)",
			got.body["format"],
		)
	}

	if got.body["stream"] != false {
		t.Errorf("stream = %v, want false", got.body["stream"])
	}

	if got.body["model"] != "llama3.2" {
		t.Errorf("model = %v", got.body["model"])
	}
}

// TestOpenAICompleter checks the OpenAI-compatible request shape, the bearer
// token and JSON response-format request.
func TestOpenAICompleter(t *testing.T) {
	t.Parallel()

	var got capture

	srv := stubServer(t, &got, http.StatusOK,
		`{"choices":[{"message":{"content":"{\"action\":\"SELL\",\"confidence\":0.4}"}}]}`)

	cl, err := newCompleter("openai", srv.URL, "gpt-4o-mini", "secret", 0.2, srv.Client())
	if err != nil {
		t.Fatal(err)
	}

	out, err := cl.Complete(t.Context(), "sys", "user")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out, `"action":"SELL"`) {
		t.Errorf("content = %q", out)
	}

	if got.path != "/v1/chat/completions" {
		t.Errorf("path = %q", got.path)
	}

	if got.auth != "Bearer secret" {
		t.Errorf("auth = %q", got.auth)
	}
}

// TestOpenAICompleterVersionedBase checks a base URL that already ends in /v1
// does not get a second /v1 segment.
func TestOpenAICompleterVersionedBase(t *testing.T) {
	t.Parallel()

	var got capture

	srv := stubServer(t, &got, http.StatusOK, `{"choices":[{"message":{"content":"{}"}}]}`)

	cl, err := newCompleter("openai", srv.URL+"/v1", "m", "", 0, srv.Client())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := cl.Complete(t.Context(), "s", "u"); err != nil {
		t.Fatal(err)
	}

	if got.path != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", got.path)
	}
}

// TestCompleterErrors checks that HTTP failures and API-level error payloads
// surface as errors rather than empty answers.
func TestCompleterErrors(t *testing.T) {
	t.Parallel()

	t.Run("http status", func(t *testing.T) {
		t.Parallel()

		var got capture

		srv := stubServer(t, &got, http.StatusNotFound, `{"error":"model not found"}`)

		cl, _ := newCompleter("ollama", srv.URL, "nope", "", 0, srv.Client())
		if _, err := cl.Complete(t.Context(), "s", "u"); err == nil {
			t.Fatal("want error for HTTP 404")
		}
	})

	t.Run("api error field", func(t *testing.T) {
		t.Parallel()

		var got capture

		srv := stubServer(t, &got, http.StatusOK, `{"error":"model \"x\" not found"}`)

		cl, _ := newCompleter("ollama", srv.URL, "x", "", 0, srv.Client())
		if _, err := cl.Complete(t.Context(), "s", "u"); err == nil {
			t.Fatal("want error for API error payload")
		}
	})

	t.Run("huge api error field is not embedded unbounded", func(t *testing.T) {
		t.Parallel()

		// Reproduces the "unbounded provider error text (up to 1MB) can
		// flood the 300-entry global errlog ring" finding: a hostile or
		// malfunctioning endpoint returns a giant "error" message inside an
		// otherwise-200 response. The error postJSON/Complete return must
		// not embed that verbatim.
		huge := strings.Repeat("x", 500_000)

		var got capture

		srv := stubServer(t, &got, http.StatusOK, `{"error":"`+huge+`"}`)

		cl, _ := newCompleter("ollama", srv.URL, "x", "", 0, srv.Client())

		_, err := cl.Complete(t.Context(), "s", "u")
		if err == nil {
			t.Fatal("want error for API error payload")
		}

		if len(err.Error()) > 1000 {
			t.Errorf("error message is %d bytes, want it capped to a log-friendly "+
				"length (snippet's 200-byte trim), not the full payload", len(err.Error()))
		}
	})

	t.Run("no choices", func(t *testing.T) {
		t.Parallel()

		var got capture

		srv := stubServer(t, &got, http.StatusOK, `{"choices":[]}`)

		cl, _ := newCompleter("openai", srv.URL, "x", "", 0, srv.Client())
		if _, err := cl.Complete(t.Context(), "s", "u"); err == nil {
			t.Fatal("want error for empty choices")
		}
	})
}

// TestNewCompleterRejectsUnknown checks configuration mistakes fail loudly at
// construction rather than silently at the first trade decision.
func TestNewCompleterRejectsUnknown(t *testing.T) {
	t.Parallel()

	if _, err := newCompleter("gemini", "http://x", "m", "", 0, http.DefaultClient); err == nil {
		t.Error("want error for unknown provider")
	}

	if _, err := newCompleter("ollama", "  ", "m", "", 0, http.DefaultClient); err == nil {
		t.Error("want error for empty base_url")
	}
}

// TestCompleteRespectsContext checks a slow model does not hang the caller past
// its deadline.
func TestCompleteRespectsContext(t *testing.T) {
	t.Parallel()

	// Answers well after the client's deadline, but in bounded time so the test
	// server can still shut down cleanly.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)

		_, _ = io.WriteString(w, `{"message":{"content":"{}"}}`)
	}))
	t.Cleanup(srv.Close)

	cl, _ := newCompleter("ollama", srv.URL, "m", "", 0, srv.Client())

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	if _, err := cl.Complete(ctx, "s", "u"); err == nil {
		t.Fatal("want error when the context expires")
	}
}
