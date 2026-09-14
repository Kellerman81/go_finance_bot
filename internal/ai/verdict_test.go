package ai

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestParseVerdictShapes covers the answer shapes real instruct models produce:
// a bare object, one wrapped in a code fence, one buried in prose, percentage
// and stringified confidences, and action synonyms.
func TestParseVerdictShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		raw    string
		action Action
		conf   float64
	}{
		{"bare", `{"action":"BUY","confidence":0.8,"reason":"trend up"}`, Buy, 0.8},
		{
			"fenced",
			"```json\n{\"action\":\"SELL\",\"confidence\":0.5,\"reason\":\"rsi high\"}\n```",
			Sell,
			0.5,
		},
		{
			"prose",
			`Here is my answer: {"action":"HOLD","confidence":0,"reason":"mixed"} Hope that helps!`,
			Hold,
			0,
		},
		{"percent", `{"action":"BUY","confidence":70,"reason":"x"}`, Buy, 0.7},
		{"string conf", `{"action":"BUY","confidence":"0.42","reason":"x"}`, Buy, 0.42},
		{"synonym long", `{"action":"long","confidence":0.6,"reason":"x"}`, Buy, 0.6},
		{"synonym neutral", `{"action":"neutral","confidence":0.9,"reason":"x"}`, Hold, 0},
		{
			"brace in reason",
			`{"action":"BUY","confidence":0.3,"reason":"broke {range} high"}`,
			Buy,
			0.3,
		},
		{"conf over 1", `{"action":"SELL","confidence":5,"reason":"x"}`, Sell, 0.05},
		{"missing conf", `{"action":"BUY","reason":"x"}`, Buy, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			v, err := parseVerdict(tc.raw)
			if err != nil {
				t.Fatalf("parseVerdict(%q): %v", tc.raw, err)
			}

			if v.Action != tc.action {
				t.Errorf("action = %q, want %q", v.Action, tc.action)
			}

			if diff := v.Confidence - tc.conf; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("confidence = %v, want %v", v.Confidence, tc.conf)
			}
		})
	}
}

// TestParseVerdictRejects checks that garbage never becomes a trade signal: the
// caller turns every error here into HOLD.
func TestParseVerdictRejects(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"",
		"I cannot provide financial advice.",
		`{"action":"MOON","confidence":1}`,
		`{"confidence":0.9,"reason":"no action field"}`,
		`{"action":`,
	} {
		if v, err := parseVerdict(raw); err == nil {
			t.Errorf("parseVerdict(%q) = %+v, want error", raw, v)
		}
	}
}

// TestParseVerdictHoldHasNoConfidence checks a HOLD never carries strength: a
// model that answers HOLD with confidence 0.9 means "confidently nothing".
func TestParseVerdictHoldHasNoConfidence(t *testing.T) {
	t.Parallel()

	v, err := parseVerdict(`{"action":"HOLD","confidence":0.9,"reason":"unclear"}`)
	if err != nil {
		t.Fatal(err)
	}

	if v.Confidence != 0 {
		t.Errorf("HOLD confidence = %v, want 0", v.Confidence)
	}
}

// TestParseVerdictTruncatesReason keeps a rambling model from bloating every
// stored trade record.
func TestParseVerdictTruncatesReason(t *testing.T) {
	t.Parallel()

	long := make([]byte, 500)
	for i := range long {
		long[i] = 'a'
	}

	v, err := parseVerdict(`{"action":"BUY","confidence":0.5,"reason":"` + string(long) + `"}`)
	if err != nil {
		t.Fatal(err)
	}

	if len(v.Reason) > maxReasonLen+4 {
		t.Errorf("reason length = %d, want <= %d", len(v.Reason), maxReasonLen+4)
	}
}

// TestParseVerdictTruncatesOnARuneBoundary reproduces the mid-rune
// truncation finding: a byte-offset slice can land inside a multibyte UTF-8
// rune for any non-ASCII reason, producing invalid UTF-8 that json.Marshal
// silently mangles to U+FFFD on the way into trade records and storage.
// Swept across every byte offset near the cut point so the fix is verified
// regardless of exactly where a multibyte rune straddles maxReasonLen.
func TestParseVerdictTruncatesOnARuneBoundary(t *testing.T) {
	t.Parallel()

	for pad := range 4 { // shifts the multibyte rune across the cut boundary
		filler := strings.Repeat("a", maxReasonLen-2+pad)
		reason := filler + "€€€€€€€€" // multibyte (3-byte) runes straddling the cut

		v, err := parseVerdict(`{"action":"BUY","confidence":0.5,"reason":` +
			strconv.Quote(reason) + `}`)
		if err != nil {
			t.Fatalf("pad=%d: %v", pad, err)
		}

		if !utf8.ValidString(v.Reason) {
			t.Errorf("pad=%d: truncated reason is not valid UTF-8: %q", pad, v.Reason)
		}
	}
}
