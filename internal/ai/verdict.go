package ai

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// Action is the model's recommendation. It deliberately mirrors
// strategy.Action's values so the detector can convert without a lookup table,
// while keeping this package independent of the strategy package.
type Action string

const (
	Hold Action = "HOLD"
	Buy  Action = "BUY"
	Sell Action = "SELL"
)

// Verdict is one model opinion about one symbol.
type Verdict struct {
	Action     Action    `json:"action"`
	Confidence float64   `json:"confidence"` // 0..1
	Reason     string    `json:"reason"`
	Model      string    `json:"model"`
	At         time.Time `json:"at"`
}

// DefaultSystemPrompt instructs the model to behave like one more detector: it
// gets normalised indicator readings and returns a single JSON object. It is
// told to prefer HOLD, because an LLM that feels obliged to have an opinion on
// every bar is an expensive random number generator.
const DefaultSystemPrompt = `You are a technical-analysis assistant inside an automated trading bot.
You are given normalised indicator readings for one instrument on its current timeframe.

Weigh trend (ema spread, adx/di, regression trend), momentum (rsi, macd, stochastic),
location (bollinger position, range position) and participation (relative volume).
Trade only when several readings agree; when they conflict or the picture is unclear,
answer HOLD. HOLD is the correct answer most of the time.

Reply with ONE JSON object and nothing else:
{"action":"BUY"|"SELL"|"HOLD","confidence":<0..1>,"reason":"<max 20 words>"}

confidence is how strongly the readings support the action: use 0 for HOLD,
below 0.4 for a weak lean, above 0.7 only when the readings clearly agree.
Do not mention that you are an AI. Do not add text outside the JSON object.`

// maxReasonLen truncates the model's free-text reason before it reaches trade
// records and the UI, so a model that ignores the word limit cannot bloat every
// stored decision.
const maxReasonLen = 160

// parseVerdict extracts the JSON verdict from a raw model answer. Models wrap
// JSON in prose or code fences often enough that the object is located rather
// than assumed to be the whole string. An unparseable or unknown action is an
// error: the caller turns that into HOLD, never a trade.
func parseVerdict(raw string) (Verdict, error) {
	obj := extractJSONObject(raw)
	if obj == "" {
		return Verdict{}, fmt.Errorf("no JSON object in model answer: %s", snippet([]byte(raw)))
	}

	// Confidence is decoded loosely: models emit 0.7, "0.7" and occasionally 70.
	var body struct {
		Action     string          `json:"action"`
		Confidence json.RawMessage `json:"confidence"`
		Reason     string          `json:"reason"`
	}

	if err := json.Unmarshal([]byte(obj), &body); err != nil {
		return Verdict{}, fmt.Errorf("decode verdict: %w (%s)", err, snippet([]byte(obj)))
	}

	action, err := parseAction(body.Action)
	if err != nil {
		return Verdict{}, err
	}

	v := Verdict{
		Action:     action,
		Confidence: clamp01(parseConfidence(body.Confidence)),
		Reason:     strings.TrimSpace(body.Reason),
	}

	if action == Hold {
		v.Confidence = 0
	}

	v.Reason = truncateReason(v.Reason)

	return v, nil
}

// truncateReason caps a model's free-text reason at maxReasonLen bytes,
// cutting on a rune boundary. A byte-offset slice (the previous
// implementation) can land mid-rune for any non-ASCII reason — a model
// replying in a language with multibyte characters, or one that quotes a
// currency symbol like "€" — producing invalid UTF-8 that json.Marshal then
// silently mangles to U+FFFD on its way into trade records and storage.
func truncateReason(s string) string {
	if len(s) <= maxReasonLen {
		return s
	}

	cut := maxReasonLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}

	return s[:cut] + "…"
}

// parseAction maps the model's action word onto an Action, accepting the common
// synonyms instruct models reach for. Anything else is rejected rather than
// guessed at.
func parseAction(s string) (Action, error) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "BUY", "LONG":
		return Buy, nil
	case "SELL", "SHORT", "EXIT":
		return Sell, nil
	case "HOLD", "NEUTRAL", "WAIT", "NONE", "FLAT":
		return Hold, nil
	case "":
		return Hold, errors.New("verdict has no action")
	default:
		return Hold, fmt.Errorf("unknown action %q", s)
	}
}

// parseConfidence reads a confidence that may arrive as a number, a quoted
// number, or a percentage (70 meaning 0.70).
func parseConfidence(raw json.RawMessage) float64 {
	if len(raw) == 0 {
		return 0
	}

	var num float64

	if err := json.Unmarshal(raw, &num); err != nil {
		var s string

		if err := json.Unmarshal(raw, &s); err != nil {
			return 0
		}

		s = strings.TrimSuffix(strings.TrimSpace(s), "%")
		if _, err := fmt.Sscanf(s, "%g", &num); err != nil {
			return 0
		}
	}

	if math.IsNaN(num) || math.IsInf(num, 0) {
		return 0
	}

	if num > 1 { // a percentage
		num /= 100
	}

	return num
}

// extractJSONObject returns the outermost {...} span of s, ignoring braces
// inside string literals so a reason like "broke out of {range}" cannot end the
// object early.
//
//nolint:cyclop // a single-pass scanner; the cases are its state machine
func extractJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}

	depth, inStr, esc := 0, false, false

	for i := start; i < len(s); i++ {
		ch := s[i]

		switch {
		case esc:
			esc = false
		case ch == '\\' && inStr:
			esc = true
		case ch == '"':
			inStr = !inStr
		case inStr:
			// braces inside strings don't nest
		case ch == '{':
			depth++
		case ch == '}':
			if depth--; depth == 0 {
				return s[start : i+1]
			}
		}
	}

	return ""
}

// clamp01 clamps v into [0, 1].
func clamp01(v float64) float64 {
	if v < 0 || math.IsNaN(v) {
		return 0
	}

	if v > 1 {
		return 1
	}

	return v
}
