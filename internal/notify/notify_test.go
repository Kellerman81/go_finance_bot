package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
)

// recorder collects the bodies a sink received.
type recorder struct {
	mu     sync.Mutex
	bodies []string
	paths  []string
	status int
}

// handler serves the recorder as an HTTP endpoint.
func (r *recorder) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)

		r.mu.Lock()

		r.bodies = append(r.bodies, string(b))
		r.paths = append(r.paths, req.URL.Path)
		status := r.status
		r.mu.Unlock()

		if status == 0 {
			status = http.StatusOK
		}

		w.WriteHeader(status)
	})
}

// count returns how many requests landed.
func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.bodies)
}

// all returns the received bodies.
func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.bodies...)
}

// waitFor polls until cond holds or the deadline passes — deliveries happen on
// a background worker, so a test must wait for them rather than assume.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", what)
}

// TestWebhookDelivery checks an event reaches a generic webhook as JSON.
func TestWebhookDelivery(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())

	t.Cleanup(srv.Close)

	n := New(config.NotifyConfig{Enabled: true, Webhook: srv.URL})
	if n == nil {
		t.Fatal("New returned nil for a configured webhook")
	}

	t.Cleanup(n.Close)

	n.Send(Event{Kind: KindTrade, Title: "BUY AAPL", Text: "qty 10 @ 100", Notional: 1000})

	waitFor(t, "webhook delivery", func() bool { return rec.count() == 1 })

	var got map[string]any
	if err := json.Unmarshal([]byte(rec.all()[0]), &got); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}

	if got["kind"] != "trade" || got["title"] != "BUY AAPL" {
		t.Errorf("payload = %v, want the event fields", got)
	}

	if txt, _ := got["text"].(string); !strings.Contains(txt, "BUY AAPL") {
		t.Errorf("text field = %q, want a rendered message for Slack-style sinks", txt)
	}
}

// TestTelegramAndDiscordSinks checks both chat sinks are called with a rendered
// message, at their respective endpoints.
func TestTelegramAndDiscordSinks(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())

	t.Cleanup(srv.Close)

	n := New(config.NotifyConfig{Enabled: true, DiscordWebhook: srv.URL})
	if n == nil {
		t.Fatal("New returned nil for a configured discord webhook")
	}

	t.Cleanup(n.Close)

	n.Send(Event{Kind: KindHalt, Title: "trading halted", Text: "max drawdown 21%"})

	waitFor(t, "discord delivery", func() bool { return rec.count() == 1 })

	var got map[string]string
	if err := json.Unmarshal([]byte(rec.all()[0]), &got); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}

	if !strings.Contains(got["content"], "trading halted") ||
		!strings.Contains(got["content"], "max drawdown") {
		t.Errorf("discord content = %q, want title and text", got["content"])
	}
}

// pushoverForm sends one event through the real Pushover sink, pointed at a
// stub, and returns the form it posted.
func pushoverForm(t *testing.T, cfg config.NotifyConfig, e Event) url.Values {
	t.Helper()

	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())

	t.Cleanup(srv.Close)

	if err := pushoverSink(cfg, srv.URL)(t.Context(), srv.Client(), e); err != nil {
		t.Fatalf("pushover send: %v", err)
	}

	form, err := url.ParseQuery(rec.all()[0])
	if err != nil {
		t.Fatalf("body is not form-encoded: %v", err)
	}

	return form
}

// TestPushoverSink checks the Pushover form shape: credentials, a titled
// message, a device restriction and a timestamp.
func TestPushoverSink(t *testing.T) {
	t.Parallel()

	form := pushoverForm(t, config.NotifyConfig{
		PushoverToken: "app-token", PushoverUser: "user-key", PushoverDevice: "phone",
	}, Event{
		Kind: KindTrade, Title: "BUY AAPL", Text: "qty 10 @ 100",
		Time: time.Unix(1767225600, 0),
	})

	if form.Get("token") != "app-token" || form.Get("user") != "user-key" {
		t.Errorf("credentials = %q/%q, want app-token/user-key",
			form.Get("token"), form.Get("user"))
	}

	if !strings.Contains(form.Get("title"), "BUY AAPL") {
		t.Errorf("title = %q, want the event title", form.Get("title"))
	}

	if form.Get("message") != "qty 10 @ 100" {
		t.Errorf("message = %q, want the event text", form.Get("message"))
	}

	if form.Get("device") != "phone" {
		t.Errorf("device = %q, want the configured phone", form.Get("device"))
	}

	if form.Get("timestamp") != "1767225600" {
		t.Errorf("timestamp = %q, want the event time", form.Get("timestamp"))
	}
}

// TestPushoverPriorityEscalates checks a halt or an error outranks a routine
// fill, and that emergency priority is unreachable.
func TestPushoverPriorityEscalates(t *testing.T) {
	t.Parallel()

	if got := pushoverPriority(0, KindTrade); got != 0 {
		t.Errorf("trade priority = %d, want the 0 baseline", got)
	}

	if got := pushoverPriority(0, KindHalt); got != 1 {
		t.Errorf("halt priority = %d, want one above the baseline", got)
	}

	if got := pushoverPriority(0, KindError); got != 1 {
		t.Errorf("error priority = %d, want one above the baseline", got)
	}

	if got := pushoverPriority(1, KindHalt); got != pushoverMaxPriority {
		t.Errorf("priority = %d, want it capped at %d: emergency priority needs a "+
			"retry/ack flow and must not be reachable", got, pushoverMaxPriority)
	}

	if got := pushoverPriority(-2, KindTrade); got != -2 {
		t.Errorf("priority = %d, want the -2 floor respected", got)
	}
}

// TestPushoverAlwaysHasAMessage checks a body-less event still sends: Pushover
// rejects an empty message outright, which would silently lose the alert.
func TestPushoverAlwaysHasAMessage(t *testing.T) {
	t.Parallel()

	form := pushoverForm(t,
		config.NotifyConfig{PushoverToken: "t", PushoverUser: "u"},
		Event{Kind: KindHalt, Title: "trading halted"})

	if form.Get("message") == "" {
		t.Error("message is empty; Pushover would reject the request")
	}

	if form.Get("priority") != "1" {
		t.Errorf("priority = %q, want a halt escalated to 1", form.Get("priority"))
	}
}

// TestAppriseStoredConfig checks the stored-configuration shape: title, body
// and a semantic type, with no service URLs.
func TestAppriseStoredConfig(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())

	t.Cleanup(srv.Close)

	n := New(config.NotifyConfig{Enabled: true, AppriseURL: srv.URL + "/notify/mykey"})
	if n == nil {
		t.Fatal("New returned nil for a configured Apprise URL")
	}

	t.Cleanup(n.Close)

	n.Send(Event{Kind: KindHalt, Title: "trading halted", Text: "max drawdown 21%"})

	waitFor(t, "apprise delivery", func() bool { return rec.count() == 1 })

	var got map[string]string
	if err := json.Unmarshal([]byte(rec.all()[0]), &got); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}

	if got["title"] != "trading halted" || got["body"] != "max drawdown 21%" {
		t.Errorf("payload = %v, want the title and body carried separately", got)
	}

	if got["type"] != "failure" {
		t.Errorf("type = %q, want failure for a halt", got["type"])
	}

	if _, ok := got["urls"]; ok {
		t.Error("service urls were sent to a stored-configuration endpoint")
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()

	if rec.paths[0] != "/notify/mykey" {
		t.Errorf("path = %q, want the configured stored-config path", rec.paths[0])
	}
}

// TestAppriseStatelessCarriesServices checks the stateless shape: the service
// URLs and tag travel with each request.
func TestAppriseStatelessCarriesServices(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())

	t.Cleanup(srv.Close)

	n := New(config.NotifyConfig{
		Enabled: true, AppriseURL: srv.URL + "/notify",
		AppriseServices: "ntfy://topic,mailto://user:pw@example.com",
		AppriseTag:      "trading",
	})
	t.Cleanup(n.Close)

	n.Send(Event{Kind: KindTrade, Title: "BUY AAPL", Text: "qty 10"})

	waitFor(t, "apprise delivery", func() bool { return rec.count() == 1 })

	var got map[string]string
	if err := json.Unmarshal([]byte(rec.all()[0]), &got); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(got["urls"], "ntfy://topic") {
		t.Errorf("urls = %q, want the configured services", got["urls"])
	}

	if got["tag"] != "trading" {
		t.Errorf("tag = %q, want trading", got["tag"])
	}

	if got["type"] != "success" {
		t.Errorf("type = %q, want success for a fill", got["type"])
	}
}

// TestAppriseTypeMapping checks each event kind maps to the Apprise type that
// downstream services render appropriately.
func TestAppriseTypeMapping(t *testing.T) {
	t.Parallel()

	for kind, want := range map[Kind]string{
		KindTrade: "success", KindBlocked: "warning",
		KindHalt: "failure", KindError: "failure", KindAudit: "info",
	} {
		if got := appriseType(kind); got != want {
			t.Errorf("appriseType(%q) = %q, want %q", kind, got, want)
		}
	}
}

// TestIncompleteSinksAreNotBuilt checks a half-configured sink is ignored rather
// than producing a notifier that fails on every send.
func TestIncompleteSinksAreNotBuilt(t *testing.T) {
	t.Parallel()

	for name, cfg := range map[string]config.NotifyConfig{
		"pushover without user":  {Enabled: true, PushoverToken: "t"},
		"pushover without token": {Enabled: true, PushoverUser: "u"},
		"telegram without chat":  {Enabled: true, TelegramToken: "t"},
	} {
		if n := New(cfg); n != nil {
			n.Close()
			t.Errorf("%s built a sink", name)
		}
	}

	// Both halves present => a sink exists.
	n := New(config.NotifyConfig{Enabled: true, PushoverToken: "t", PushoverUser: "u"})
	if n == nil {
		t.Fatal("a fully configured Pushover sink was not built")
	}

	t.Cleanup(n.Close)

	if s := n.Stats(); len(s.Sinks) != 1 || s.Sinks[0] != "pushover" {
		t.Errorf("sinks = %v, want [pushover]", s.Sinks)
	}
}

// TestKindFiltering checks only subscribed kinds are delivered.
func TestKindFiltering(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())

	t.Cleanup(srv.Close)

	n := New(config.NotifyConfig{
		Enabled: true, Webhook: srv.URL, Events: []string{"halt"},
	})
	t.Cleanup(n.Close)

	n.Send(Event{Kind: KindTrade, Title: "BUY AAPL"})
	n.Send(Event{Kind: KindHalt, Title: "halted"})

	waitFor(t, "halt delivery", func() bool { return rec.count() >= 1 })
	time.Sleep(20 * time.Millisecond) // give any wrong delivery a chance to arrive

	if got := rec.count(); got != 1 {
		t.Errorf("delivered %d events, want only the subscribed halt", got)
	}

	if !strings.Contains(rec.all()[0], "halted") {
		t.Errorf("delivered the wrong event: %s", rec.all()[0])
	}
}

// TestKindAuditDefaultsOnAndHasAnIcon reproduces the two smallest but easiest
// to silently forget parts of adding a new Kind: parseKinds' default
// subscription (an operator who never touched notify.events should still be
// told the bot rewrote its own config unsupervised) and kindIcon (every
// existing Kind has one; a missing entry silently prints nothing rather than
// failing to compile).
func TestKindAuditDefaultsOnAndHasAnIcon(t *testing.T) {
	t.Parallel()

	if !parseKinds(nil)[KindAudit] {
		t.Error("KindAudit is not in the default subscription set")
	}

	if kindIcon(KindAudit) == "" {
		t.Error("KindAudit has no icon")
	}
}

// TestKindAuditDoesNotBumpPushoverPriority: unlike KindHalt/KindError, an
// audit event (an unsupervised config change, not an emergency) must not
// raise Pushover priority above the operator's own configured baseline.
func TestKindAuditDoesNotBumpPushoverPriority(t *testing.T) {
	t.Parallel()

	for _, base := range []int{-1, 0, 1} {
		if got := pushoverPriority(base, KindAudit); got != base {
			t.Errorf("pushoverPriority(%d, KindAudit) = %d, want unchanged", base, got)
		}
	}
}

// TestMinNotionalSuppressesSmallFills checks noise control applies to fills but
// never to halts.
func TestMinNotionalSuppressesSmallFills(t *testing.T) {
	t.Parallel()

	rec := &recorder{}
	srv := httptest.NewServer(rec.handler())

	t.Cleanup(srv.Close)

	n := New(config.NotifyConfig{
		Enabled: true, Webhook: srv.URL, MinNotional: 500,
		Events: []string{"trade", "halt"},
	})
	t.Cleanup(n.Close)

	n.Send(Event{Kind: KindTrade, Title: "small", Notional: 100})
	n.Send(Event{Kind: KindHalt, Title: "halted"})

	waitFor(t, "halt delivery", func() bool { return rec.count() >= 1 })
	time.Sleep(20 * time.Millisecond)

	if got := rec.count(); got != 1 {
		t.Errorf("delivered %d, want the halt only (the small fill is below min_notional)", got)
	}
}

// TestSendNeverBlocks checks a saturated queue drops events rather than
// stalling the caller — a slow chat server must never slow the trading loop.
func TestSendNeverBlocks(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-block
	}))

	t.Cleanup(func() { close(block); srv.Close() })

	n := New(config.NotifyConfig{
		Enabled: true, Webhook: srv.URL, Timeout: 50 * time.Millisecond,
	})
	t.Cleanup(n.Close)

	done := make(chan struct{})

	go func() {
		defer close(done)

		for i := range queueSize * 3 {
			n.Send(Event{Kind: KindTrade, Title: "e", Notional: float64(i)})
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Send blocked when the sink was stalled; it must drop instead")
	}

	waitFor(t, "dropped counter", func() bool { return n.Stats().Dropped > 0 })
}

// TestHaltBypassesAFullRoutineQueue reproduces the "a halt notification is
// exactly as droppable as a routine trade notification" finding: a broker
// outage generates one KindError per symbol per eval tick, filling the
// (small, deliberately bounded) routine queue well within a minute at
// default throughput. Before the fix, the one alert an unattended bot exists
// to send — a drawdown/daily-loss halt — competed for the very same slots
// and could be silently dropped right when it mattered most. Halts now have
// their own small lane that the delivery worker always drains first.
func TestHaltBypassesAFullRoutineQueue(t *testing.T) {
	rec := &recorder{}

	var (
		mu    sync.Mutex
		first = true
	)

	block := make(chan struct{})

	// Exactly the first request blocks — holding the single delivery worker
	// mid-delivery so nothing behind it can be sent until the test releases
	// it. Every later request completes immediately.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		isFirst := first
		first = false
		mu.Unlock()

		if isFirst {
			<-block
		}

		rec.handler().ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		select {
		case <-block: // already closed by the test body
		default:
			close(block)
		}
	})

	n := New(config.NotifyConfig{Enabled: true, Webhook: srv.URL, Timeout: 2 * time.Second})
	t.Cleanup(n.Close)

	n.Send(Event{Kind: KindTrade, Title: "in-flight"})

	// Let the worker actually pick that event up (and start blocking on it)
	// before the flood behind it, or the flood could race to be the one that
	// blocks instead.
	time.Sleep(20 * time.Millisecond)

	for range queueSize + 5 {
		n.Send(Event{Kind: KindTrade, Title: "flood"})
	}

	waitFor(t, "the flood to overflow the routine queue", func() bool {
		return n.Stats().Dropped > 0
	})

	n.Send(Event{Kind: KindHalt, Title: "trading halted"})

	close(block)

	waitFor(t, "the halt to be delivered", func() bool {
		for _, b := range rec.all() {
			if strings.Contains(b, "trading halted") {
				return true
			}
		}

		return false
	})

	bodies := rec.all()
	if len(bodies) < 2 {
		t.Fatalf("expected at least 2 deliveries, got %d", len(bodies))
	}

	if !strings.Contains(bodies[1], "trading halted") {
		t.Errorf(
			"the halt was not delivered right after the in-flight request — "+
				"a backlog of routine events got priority instead: %s",
			bodies[1],
		)
	}
}

// The urgent lane is bounded too, so an unbounded halt storm cannot leak
// memory — it just drops, same as the routine lane.
func TestUrgentQueueAlsoDropsWhenOverflowed(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-block
	}))
	t.Cleanup(func() { close(block); srv.Close() })

	n := New(config.NotifyConfig{
		Enabled: true, Webhook: srv.URL, Timeout: 50 * time.Millisecond,
	})
	t.Cleanup(n.Close)

	for range urgentQueueSize + 5 {
		n.Send(Event{Kind: KindHalt, Title: "halted"})
	}

	waitFor(t, "dropped counter", func() bool { return n.Stats().Dropped > 0 })
}

// TestASlowSinkDoesNotPoisonTheNextSinksTimeout reproduces the "one context
// deadline is shared across all configured notify sinks" finding: deliver
// used to build a single context.WithTimeout and pass it to every sink in
// the loop, so a first sink that consumed the whole timeout left every
// remaining sink an already-expired context — its request would fail
// instantly, before ever reaching the network, regardless of that sink's own
// health. Each sink must get its own fresh budget.
func TestASlowSinkDoesNotPoisonTheNextSinksTimeout(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-block // never responds within the configured timeout
	}))
	t.Cleanup(func() { close(block); slow.Close() })

	healthy := &recorder{}
	healthySrv := httptest.NewServer(healthy.handler())
	t.Cleanup(healthySrv.Close)

	n := New(config.NotifyConfig{
		Enabled:        true,
		Webhook:        slow.URL,       // sink 1: exhausts its own timeout
		DiscordWebhook: healthySrv.URL, // sink 2: must still get a full budget
		Timeout:        60 * time.Millisecond,
	})
	t.Cleanup(n.Close)

	n.Send(Event{Kind: KindHalt, Title: "trading halted"})

	waitFor(t, "the healthy sink to be reached despite the slow one ahead of it",
		func() bool { return healthy.count() > 0 })
}

// TestFailedDeliveryIsCounted checks a rejecting endpoint is recorded rather
// than silently ignored.
func TestFailedDeliveryIsCounted(t *testing.T) {
	t.Parallel()

	rec := &recorder{status: http.StatusForbidden}
	srv := httptest.NewServer(rec.handler())

	t.Cleanup(srv.Close)

	n := New(config.NotifyConfig{Enabled: true, Webhook: srv.URL})
	t.Cleanup(n.Close)

	n.Send(Event{Kind: KindTrade, Title: "BUY AAPL"})

	waitFor(t, "failure counter", func() bool { return n.Stats().Failed > 0 })

	if n.Stats().Sent != 0 {
		t.Error("a 403 was counted as sent")
	}
}

// TestDisabledAndUnconfigured checks both "off" paths produce a nil notifier
// that is safe to use.
func TestDisabledAndUnconfigured(t *testing.T) {
	t.Parallel()

	if n := New(config.NotifyConfig{Enabled: false, Webhook: "http://x"}); n != nil {
		t.Error("disabled config built a notifier")
	}

	if n := New(config.NotifyConfig{Enabled: true}); n != nil {
		t.Error("enabled config with no sink built a notifier")
	}

	var n *Notifier

	n.Send(Event{Kind: KindTrade}) // must not panic
	n.Sendf(KindHalt, "t", "%s", "x")
	n.Close()

	if s := n.Stats(); s.Enabled {
		t.Error("nil notifier reports enabled")
	}
}

// TestTransportErrorDoesNotLeakTheSecretURL reproduces CRIT-12: a webhook URL
// commonly carries the secret itself (a Telegram bot token, a Discord webhook
// path). hc.Do wraps a connection failure in a *url.Error whose Error()
// string embeds that URL verbatim — and this error is what post() returns and
// what ends up in the process-wide errlog ring, which an unauthenticated (by
// default) API endpoint serves. The error message must not contain the
// secret path/token component of the URL.
func TestTransportErrorDoesNotLeakTheSecretURL(t *testing.T) {
	t.Parallel()

	const secretPath = "/bot7712345:AAF-super-secret-token/sendMessage"

	// No listener on this address: hc.Do fails at connect time with a
	// *url.Error wrapping the dial failure, exactly like a DNS/TCP/TLS
	// failure against a real webhook host would.
	hc := &http.Client{Timeout: 2 * time.Second}
	endpoint := "http://127.0.0.1:1" + secretPath

	err := post(t.Context(), hc, endpoint, "application/json", []byte(`{}`))
	if err == nil {
		t.Fatal("want an error connecting to a closed port")
	}

	if strings.Contains(err.Error(), secretPath) {
		t.Errorf("error leaked the secret URL path: %v", err)
	}

	if strings.Contains(err.Error(), endpoint) {
		t.Errorf("error leaked the full endpoint URL: %v", err)
	}
}

// TestSanitizeTransportErrPassesThroughNonURLErrors checks the helper only
// touches *url.Error, so an ordinary error (e.g. from building the request)
// is returned unchanged rather than obscured.
func TestSanitizeTransportErrPassesThroughNonURLErrors(t *testing.T) {
	t.Parallel()

	plain := &json.SyntaxError{}

	if got := sanitizeTransportErr(plain); got != error(plain) {
		t.Errorf("sanitizeTransportErr altered a non-*url.Error: %v", got)
	}
}
