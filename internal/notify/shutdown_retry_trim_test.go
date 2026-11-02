package notify

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// NT3-3: Close returned at once and cmd/bot exits right after it, so a halt
// queued behind an in-flight event was never sent at shutdown — NT2-6's drain
// only held while the process kept running. Close waits for it.
func TestCloseWaitsForTheQueuedHalt(t *testing.T) {
	var halts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond) // a slow sink

		if r.ContentLength > 0 {
			halts.Add(1)
		}
	}))
	defer srv.Close()

	n := New(config.NotifyConfig{Enabled: true, Webhook: srv.URL, Events: []string{"trade", "halt"}})

	n.Send(Event{Kind: KindTrade, Title: "fill"}) // occupies the worker
	time.Sleep(20 * time.Millisecond)
	n.Send(Event{Kind: KindHalt, Title: "drawdown halt"})

	n.Close()

	if halts.Load() < 2 {
		t.Errorf("Close returned with %d deliveries done; the queued halt was still pending", halts.Load())
	}
}

// NT3-4: one 429 lost the halt — no retry, Retry-After ignored. Discord and
// Telegram rate-limit bursts, and the burst before a halt is what puts the
// halt into the window.
func TestAHaltIsRetriedAfterARateLimit(t *testing.T) {
	var calls, delivered atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)

			return
		}

		delivered.Add(1)
	}))
	defer srv.Close()

	n := New(config.NotifyConfig{Enabled: true, Webhook: srv.URL})
	n.Send(Event{Kind: KindHalt, Title: "drawdown halt"})
	n.Close() // waits for the urgent lane, retries included

	if delivered.Load() != 1 {
		t.Errorf("the halt was delivered %d times after one 429 (calls %d), want 1", delivered.Load(), calls.Load())
	}
}

// A routine event is still worth one attempt, not a backlog.
func TestARoutineEventIsNotRetried(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	n := New(config.NotifyConfig{Enabled: true, Webhook: srv.URL, Events: []string{"trade"}})
	n.Send(Event{Kind: KindTrade, Title: "fill"})
	time.Sleep(200 * time.Millisecond)
	n.Close()

	if calls.Load() != 1 {
		t.Errorf("a routine event was sent %d times, want 1", calls.Load())
	}
}

// NT3-5: NT2-1 trimmed Telegram's credentials only. A Pushover token with a
// trailing newline was sent verbatim and rejected on every alert, and a
// whitespace-only one still built a sink that failed forever.
func TestPushoverCredentialsAreTrimmed(t *testing.T) {
	if n := New(config.NotifyConfig{Enabled: true, PushoverToken: "  \n", PushoverUser: "u"}); n != nil {
		n.Close()
		t.Error("a whitespace-only Pushover token built a sink")
	}
}
