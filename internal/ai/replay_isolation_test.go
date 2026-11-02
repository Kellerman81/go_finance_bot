package ai

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// AI3-1: on the bar clock a second blocking replay of the same symbol — the
// next optimizer trial, the next walk-forward window — starts at a bar EARLIER
// than the cached verdict. Both ages went negative: nothing was due and the
// verdict read as fresh, so the previous run's LAST-bar verdict answered every
// bar of the new run with no model call. A verdict from the future is no
// verdict.
func TestASecondReplayDoesNotReuseALaterVerdict(t *testing.T) {
	var calls atomic.Int64

	a := testAdvisor(t, config.AIConfig{Blocking: true, RefreshInterval: 15 * time.Minute, MaxAge: time.Hour},
		func(_ context.Context, f Features) (Verdict, error) {
			calls.Add(1)
			// Encode which bar the verdict was formed on.
			return Verdict{Action: Buy, Confidence: 0.9, Reason: f.At.Format("15:04")}, nil
		})

	base := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)
	run := func() []string {
		var reasons []string
		for i := range 10 {
			f := Features{Symbol: "X", RSI: float64(20 + 5*i), At: base.Add(time.Duration(i) * 20 * time.Minute)}
			v, ok := a.Verdict("X", f)
			if !ok {
				reasons = append(reasons, "none")
				continue
			}
			reasons = append(reasons, v.Reason)
		}
		return reasons
	}

	first := run()
	c1 := calls.Load()
	second := run()
	c2 := calls.Load() - c1

	if c2 != c1 {
		t.Errorf("second identical replay made %d model calls, want %d", c2, c1)
	}

	for i, r := range second {
		if r != first[i] {
			t.Errorf("bar %d of run 2 got verdict formed at %s, want %s (lookahead)", i, r, first[i])
		}
	}
}

// ST3-1's mechanism at the ai level: Fresh shares the model client but not the
// cache — a replay's verdicts never reach the live advisor's.
func TestAFreshAdvisorHasItsOwnCache(t *testing.T) {
	live := testAdvisor(t, config.AIConfig{Blocking: true, RefreshInterval: time.Minute, MaxAge: time.Hour},
		func(_ context.Context, f Features) (Verdict, error) {
			return Verdict{Action: Buy, Confidence: 0.9, Reason: f.At.Format("15:04")}, nil
		})

	at := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)
	if _, ok := live.Verdict("X", Features{Symbol: "X", At: at}); !ok {
		t.Fatal("setup: no live verdict")
	}

	replay := live.Fresh()
	replay.call = live.call // the stand-in model, as Fresh shares the real client

	if _, ok := replay.cached("X", at); ok {
		t.Error("a fresh advisor started with the live advisor's verdict")
	}

	replay.Verdict("X", Features{Symbol: "X", RSI: 70, At: at.Add(-72 * time.Hour)})

	if v, _ := live.cached("X", at); v.Reason != "09:00" {
		t.Errorf("a replay changed the live verdict to %q", v.Reason)
	}
}
