package api

import "testing"

// TestSubtleEqual checks the switch to crypto/subtle.ConstantTimeCompare
// preserved ordinary string-equality semantics (see
// CODE_REVIEW_FINDINGS.md's "a hand-rolled constant-time compare, should
// use crypto/subtle").
func TestSubtleEqual(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"", "", true},
		{"secret", "secret", true},
		{"secret", "SECRET", false},
		{"secret", "secre", false},
		{"secret", "secrets", false},
		{"", "secret", false},
	}

	for _, c := range cases {
		if got := subtleEqual(c.a, c.b); got != c.want {
			t.Errorf("subtleEqual(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
