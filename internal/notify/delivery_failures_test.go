package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// TestMalformedSinkURLDoesNotLeakTheSecret is the regression for NT2-1. Only
// the transport error went through sanitizeTransportErr; building the request
// for a URL that does not parse — a Telegram token with a trailing newline, a
// Discord URL with a bad %-escape — returned url.Parse's error unsanitized,
// and it embeds the URL, which is the secret. Every alert wrote the token to
// errlog, which the diagnostics tab serves.
func TestMalformedSinkURLDoesNotLeakTheSecret(t *testing.T) {
	t.Parallel()

	const secret = "123456:SECRETTOKEN"

	err := post(context.Background(), http.DefaultClient,
		"https://api.telegram.org/bot"+secret+"\n/sendMessage", "text/plain", nil)
	if err == nil {
		t.Fatal("a malformed URL posted without an error")
	}

	if strings.Contains(err.Error(), "SECRETTOKEN") {
		t.Errorf("error carries the secret: %v", err)
	}
}

// TestARedirectIsNotADelivery is the regression for NT2-2. Go's client follows
// a 301/302 by turning the POST into a body-less GET; a webhook URL written as
// http:// redirected to https://, the GET answered 200, and every alert —
// halts included — was counted as sent while the channel stayed empty.
func TestARedirectIsNotADelivery(t *testing.T) {
	t.Parallel()

	got := &recorder{}
	final := httptest.NewServer(got.handler())
	t.Cleanup(final.Close)

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusMovedPermanently)
	}))
	t.Cleanup(redirect.Close)

	n := New(config.NotifyConfig{Enabled: true, Webhook: redirect.URL})
	t.Cleanup(n.Close)

	n.Send(Event{Kind: KindHalt, Title: "halted"})

	waitFor(t, "the delivery attempt", func() bool { return n.Stats().Failed+n.Stats().Sent > 0 })

	if st := n.Stats(); st.Sent != 0 || st.Failed != 1 {
		t.Errorf("a redirected webhook counted sent=%d failed=%d, want 0/1", st.Sent, st.Failed)
	}

	if got.count() != 0 {
		t.Errorf("the redirect target received %d requests (a body-less GET), want none", got.count())
	}
}

// TestLongMessagesAreCutToTheSinkLimit is the regression for NT2-5: a message
// over a sink's limit (Discord 2000, Telegram 4096, Pushover 1024) is refused
// with a 400 and the alert is lost — a raw broker error body in "instrument not
// tradeable" was enough. It must be shortened instead.
func TestLongMessagesAreCutToTheSinkLimit(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", 5000)

	if got := len([]rune(capRunes(long, discordMaxLen))); got != discordMaxLen {
		t.Errorf("capped to %d runes, want %d", got, discordMaxLen)
	}

	if s := "short"; capRunes(s, discordMaxLen) != s {
		t.Error("a short message was altered")
	}
}

// TestCloseStillDeliversQueuedHalts is the regression for NT2-6. Close runs
// on every notify settings save and at shutdown. When done and a queued halt
// were both ready, the worker's select could pick done and discard the halt
// — a random tie no test can force, so the first half here only pins that a
// halt queued before Close is delivered. The deterministic half: a send after
// Close sat in a buffer no worker reads and was not counted at all; it must
// count as dropped.
func TestCloseStillDeliversQueuedHalts(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())
	t.Cleanup(srv.Close)

	n := &Notifier{
		cfg:     config.NotifyConfig{Enabled: true},
		kinds:   parseKinds(nil),
		hc:      http.DefaultClient,
		queue:   make(chan Event, queueSize),
		urgent:  make(chan Event, urgentQueueSize),
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
		sinks:   []sink{{"webhook", postJSONSink(srv.URL)}},
	}

	n.Send(Event{Kind: KindHalt, Title: "max drawdown"}) // queued, worker not started yet

	// Close now waits for the worker's drain (NT3-3), so it runs beside the
	// worker here; the worker still sees done first.
	closed := make(chan struct{})

	go func() {
		n.Close()
		close(closed)
	}()

	for !isClosed(n.done) {
		time.Sleep(time.Millisecond)
	}

	n.run()
	<-closed

	if rec.count() != 1 {
		t.Errorf("a halt queued before Close was delivered %d times, want 1", rec.count())
	}

	n.Send(Event{Kind: KindHalt, Title: "after close"})

	if n.Stats().Dropped != 1 {
		t.Errorf("a send after Close counted dropped=%d, want 1", n.Stats().Dropped)
	}
}

// isClosed reports whether ch has been closed.
func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
