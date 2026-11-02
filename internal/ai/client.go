package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"
)

// completer sends one system+user exchange to a model and returns its raw text
// answer. The two implementations differ only in wire format.
type completer interface {
	// Name identifies the provider for logs and the /api/ai status endpoint.
	Name() string
	// Complete returns the model's raw answer, which is expected to be (or
	// contain) the JSON verdict object.
	Complete(ctx context.Context, system, user string) (string, error)
}

// maxBody bounds how much of a model response is read. Verdicts are a few
// hundred bytes; anything larger is a malfunctioning endpoint, not an answer.
const maxBody = 1 << 20

// newCompleter builds the provider client for the configured protocol.
func newCompleter(
	provider, baseURL, model, apiKey string,
	temperature float64,
	hc *http.Client,
) (completer, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return nil, errors.New("ai: base_url is empty")
	}

	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", "ollama":
		return &ollamaCompleter{base: base, model: model, temp: temperature, hc: hc}, nil

	case "openai":
		return &openAICompleter{
			base: base, model: model, key: apiKey, temp: temperature, hc: hc,
		}, nil

	default:
		return nil, fmt.Errorf("ai: unknown provider %q", provider)
	}
}

// ----- Ollama (native /api/chat) -----

// chatMessage is one message in a chat response, shared by both providers.
type chatMessage struct {
	Content string `json:"content"`
}

// ollamaChatResponse is Ollama's non-streaming /api/chat reply.
type ollamaChatResponse struct {
	Message chatMessage `json:"message"`
	Error   string      `json:"error"`
}

// ollamaCompleter talks to an Ollama server. It uses the native chat endpoint
// with `format: "json"`, which constrains Ollama's sampler to emit valid JSON —
// materially more reliable than asking a small local model to behave.
type ollamaCompleter struct {
	base  string
	model string
	temp  float64
	hc    *http.Client
}

// Name identifies the provider.
func (*ollamaCompleter) Name() string { return "ollama" }

// Complete posts the exchange to /api/chat and returns the message content.
func (c *ollamaCompleter) Complete(ctx context.Context, system, user string) (string, error) {
	body := map[string]any{
		"model": c.model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"stream":  false,
		"format":  "json",
		"options": map[string]any{"temperature": c.temp},
	}

	var out ollamaChatResponse

	if err := postJSON(ctx, c.hc, c.base+"/api/chat", "", body, &out); err != nil {
		return "", err
	}

	if out.Error != "" {
		// snippet, not the raw field: out.Error is JSON-decoded from a
		// response body that can carry up to maxBody (1MB) — a hostile or
		// malfunctioning endpoint could make its entire error message that
		// large, and this reaches errlog's fixed-size global ring
		// unbounded, same risk the maxBody read cap and postJSON's other
		// two error paths already guard against with snippet(). See
		// CODE_REVIEW_FINDINGS.md's unbounded-provider-error-text finding.
		return "", fmt.Errorf("ollama: %s", snippet([]byte(out.Error)))
	}

	return out.Message.Content, nil
}

// ----- OpenAI-compatible (/v1/chat/completions) -----

// openAIChoice is one completion alternative.
type openAIChoice struct {
	Message chatMessage `json:"message"`
}

// openAIError is the error object OpenAI-compatible APIs return.
type openAIError struct {
	Message string `json:"message"`
}

// openAIChatResponse is the /v1/chat/completions reply.
type openAIChatResponse struct {
	Choices []openAIChoice `json:"choices"`
	Error   openAIError    `json:"error"`
}

// openAICompleter talks to any endpoint implementing OpenAI's chat-completions
// API: OpenAI itself, OpenRouter, vLLM, LM Studio, llama.cpp's server, or
// Ollama's own compatibility shim.
type openAICompleter struct {
	base  string
	model string
	key   string
	temp  float64
	hc    *http.Client
}

// Name identifies the provider.
func (*openAICompleter) Name() string { return "openai" }

// Complete posts the exchange to /v1/chat/completions and returns the first
// choice's message content.
func (c *openAICompleter) Complete(ctx context.Context, system, user string) (string, error) {
	body := map[string]any{
		"model": c.model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"temperature":     c.temp,
		"response_format": map[string]string{"type": "json_object"},
	}

	var out openAIChatResponse

	url := c.base + "/v1/chat/completions"
	// Bases that already point at the versioned root (…/v1) must not get a
	// second /v1 segment.
	if strings.HasSuffix(c.base, "/v1") {
		url = c.base + "/chat/completions"
	}

	if err := postJSON(ctx, c.hc, url, c.key, body, &out); err != nil {
		return "", err
	}

	if out.Error.Message != "" {
		// snippet, not the raw field — see the identical reasoning at the
		// ollama error path above.
		return "", fmt.Errorf("openai: %s", snippet([]byte(out.Error.Message)))
	}

	if len(out.Choices) == 0 {
		return "", errors.New("openai: no choices in response")
	}

	return out.Choices[0].Message.Content, nil
}

// postJSON marshals body, POSTs it (with an optional bearer token) and decodes
// the response into out. A non-2xx status is an error carrying a snippet of the
// body, so a wrong model name or missing key is legible in the error log.
func postJSON(
	ctx context.Context,
	hc *http.Client,
	url, bearer string,
	body, out any,
) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	// The URL in these messages is redacted: an OpenAI-compatible base_url
	// behind basic auth (https://ops:pw@llm.lan) put the password into every
	// error — errlog, /api/errors, the advisor's LastError — because the
	// wrapper re-added the URL net/http had already stripped (EL3-4).
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("call %s: %w", redactURL(url), err)
	}

	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s: http %d: %s", redactURL(url), resp.StatusCode, snippet(data))
	}

	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response: %w (%s)", err, snippet(data))
	}

	return nil
}

// snippet trims a response body to a log-friendly length.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}

	return s
}

// redactURL is raw with any userinfo password replaced, for an error message.
// An unparsable URL is withheld entirely rather than echoed.
func redactURL(raw string) string {
	u, err := neturl.Parse(raw)
	if err != nil {
		return "<unparsable url>"
	}

	return u.Redacted()
}
