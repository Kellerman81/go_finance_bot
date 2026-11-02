// Package notify delivers bot events to the places a human actually looks:
// a generic webhook, a Discord channel, or a Telegram chat.
//
// The design constraint is that notification must never affect trading.
// Deliveries are queued on a buffered channel and sent by a single background
// worker; when the queue is full, events are dropped rather than allowed to
// block the caller. A trading loop that stalls because a chat server is slow
// would be a far worse failure than a missed message.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/errlog"
)

// Kind classifies an event so the config can subscribe to some and not others.
type Kind string

const (
	// KindTrade is a filled order.
	KindTrade Kind = "trade"
	// KindBlocked is an order refused by risk limits or protections.
	KindBlocked Kind = "blocked"
	// KindHalt is a circuit breaker or protection halting automated trading.
	KindHalt Kind = "halt"
	// KindError is an upstream API failure.
	KindError Kind = "error"
	// KindAudit is an unsupervised change the bot made to its own live
	// config (e.g. the scheduled auto-optimizer writing a new
	// strategy_overrides entry) — distinct from Halt (which implies trading
	// stopped) and routine enough not to need urgent-queue priority, but
	// important enough to default ON: an operator who never touched
	// notify.events should still be told the bot rewrote its own trading
	// parameters without asking.
	KindAudit Kind = "audit"
)

// Event is one notifiable occurrence.
type Event struct {
	Kind     Kind      `json:"kind"`
	Time     time.Time `json:"time"`
	Title    string    `json:"title"`
	Text     string    `json:"text"`
	Symbol   string    `json:"symbol,omitempty"`
	Notional float64   `json:"notional,omitempty"`
}

// queueSize is how many routine events may be pending delivery. Small on
// purpose: if the sink is this far behind, the backlog is stale anyway.
const queueSize = 128

// urgentQueueSize bounds the halt lane. Halts are rare — this is not a
// backlog, just enough room that a halt is never dropped for arriving a
// moment before its own sink catches up.
const urgentQueueSize = 8

// Notifier fans one event out to every configured sink.
type Notifier struct {
	cfg   config.NotifyConfig
	kinds map[Kind]bool
	hc    *http.Client

	// queue carries routine events (trade, blocked, error). urgent carries
	// halts, on its own small lane so a flood of routine events (e.g. one
	// KindError per symbol per eval tick during a broker outage, which fills
	// queue in well under a minute at default throughput) can never delay or
	// drop the one alert an unattended bot exists to send. run drains urgent
	// first. See CODE_REVIEW_FINDINGS.md's "halt notification is exactly as
	// droppable as a routine trade notification".
	queue  chan Event
	urgent chan Event
	// sinks are the configured delivery targets, resolved once at construction.
	sinks []sink

	closeOnce sync.Once
	done      chan struct{}
	// stopped is closed by the worker once it has drained the urgent lane;
	// Close waits on it (NT3-3). sendMu serialises Send's "not closed yet"
	// check and enqueue against Close, so nothing lands in a buffer after
	// the worker stopped reading it.
	stopped chan struct{}
	sendMu  sync.RWMutex

	mu      sync.Mutex
	sent    int64
	dropped int64
	failed  int64
}

// sink delivers one event to one destination.
type sink struct {
	name string
	send func(ctx context.Context, hc *http.Client, e Event) error
}

// New builds a Notifier and starts its delivery worker. It returns nil when
// notifications are disabled or no sink is configured — every method tolerates a
// nil receiver, so callers need no special case.
func New(cfg config.NotifyConfig) *Notifier {
	if !cfg.Enabled {
		return nil
	}

	n := &Notifier{
		cfg:   cfg,
		kinds: parseKinds(cfg.Events),
		hc: &http.Client{
			Timeout: timeoutOr(cfg.Timeout),
			// A redirect is a failure, not a delivery. Go's default follows a
			// 301/302 by turning the POST into a body-less GET — a
			// discord_webhook written as http:// redirects to https://, the
			// GET on a webhook URL answers 200, and every alert (halts
			// included) was counted as sent while the channel stayed empty
			// (NT2-2). The 3xx now reaches post's non-2xx check.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		queue:   make(chan Event, queueSize),
		urgent:  make(chan Event, urgentQueueSize),
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
	}

	if u := strings.TrimSpace(cfg.Webhook); u != "" {
		n.sinks = append(n.sinks, sink{"webhook", postJSONSink(u)})
	}

	if u := strings.TrimSpace(cfg.DiscordWebhook); u != "" {
		n.sinks = append(n.sinks, sink{"discord", discordSink(u)})
	}

	// Trimmed: a token with a trailing newline (an env var, a YAML block
	// scalar) made an invalid URL, so every alert failed — and the parse
	// error, which embeds the URL with the token in it, went to errlog
	// (NT2-1).
	tok, chat := strings.TrimSpace(cfg.TelegramToken), strings.TrimSpace(cfg.TelegramChatID)
	if tok != "" && chat != "" {
		n.sinks = append(n.sinks, sink{"telegram", telegramSink(tok, chat)})
	}

	// Trimmed like Telegram's (NT2-1): a trailing newline made Pushover
	// reject every alert as an invalid token, and a whitespace-only value
	// still built a sink (NT3-5).
	po := cfg
	po.PushoverToken, po.PushoverUser = strings.TrimSpace(
		cfg.PushoverToken,
	), strings.TrimSpace(
		cfg.PushoverUser,
	)

	if po.PushoverToken != "" && po.PushoverUser != "" {
		n.sinks = append(n.sinks, sink{"pushover", pushoverSink(po, pushoverEndpoint)})
	}

	if u := strings.TrimSpace(cfg.AppriseURL); u != "" {
		n.sinks = append(n.sinks, sink{"apprise", appriseSink(cfg, u)})
	}

	if len(n.sinks) == 0 {
		return nil
	}

	go n.run()

	return n
}

// timeoutOr returns d, or a sane default when unset.
func timeoutOr(d time.Duration) time.Duration {
	if d <= 0 {
		return 10 * time.Second
	}

	return d
}

// parseKinds builds the subscribed-kind set, defaulting to everything except
// per-order blocks (which are routine and would be noisy).
func parseKinds(events []string) map[Kind]bool {
	if len(events) == 0 {
		return map[Kind]bool{KindTrade: true, KindHalt: true, KindError: true, KindAudit: true}
	}

	set := make(map[Kind]bool, len(events))
	for _, e := range events {
		set[Kind(strings.ToLower(strings.TrimSpace(e)))] = true
	}

	return set
}

// Send queues an event for delivery. It never blocks: a full queue drops the
// event and counts it. Nil-safe.
//
//nolint:cyclop // filter, suppress, route and enqueue — each a single guard
func (n *Notifier) Send(e Event) {
	if n == nil || !n.kinds[e.Kind] {
		return
	}

	// Routine small fills are suppressed; anything that halts trading or errors
	// is always worth waking someone for.
	if e.Kind == KindTrade && n.cfg.MinNotional > 0 && e.Notional < n.cfg.MinNotional {
		return
	}

	if e.Time.IsZero() {
		e.Time = time.Now()
	}

	q := n.queue
	if e.Kind == KindHalt {
		q = n.urgent
	}

	n.sendMu.RLock()
	defer n.sendMu.RUnlock()

	// After Close nothing will ever deliver it: count it as dropped rather
	// than let it sit in a buffer no worker reads (NT2-6).
	select {
	case <-n.done:
		n.mu.Lock()

		n.dropped++
		n.mu.Unlock()

		return

	default:
	}

	select {
	case q <- e:
	default:
		n.mu.Lock()

		n.dropped++
		n.mu.Unlock()
	}
}

// Sendf queues an event with a formatted body.
func (n *Notifier) Sendf(kind Kind, title, format string, args ...any) {
	n.Send(Event{Kind: kind, Title: title, Text: fmt.Sprintf(format, args...)})
}

// run is the single delivery worker. It always drains urgent (halts) before
// touching queue (everything else), so a routine-event flood that fills
// queue cannot delay a halt behind it.
func (n *Notifier) run() {
	for {
		select {
		case e := <-n.urgent:
			n.deliver(e)

			continue

		default:
		}

		select {
		case <-n.done:
			n.drainUrgent()
			n.countUndelivered()
			close(n.stopped)

			return
		case e := <-n.urgent:
			n.deliver(e)
		case e := <-n.queue:
			n.deliver(e)
		}
	}
}

// drainUrgent delivers whatever is still queued on the urgent lane when the
// worker stops. Close runs on every notify settings save and at shutdown, and
// a halt or an "unknown order" alert queued just before it was discarded
// (NT2-6). Routine events are still dropped: the old worker exiting must not
// delay the settings save or the shutdown behind a slow sink's backlog.
func (n *Notifier) drainUrgent() {
	for {
		select {
		case e := <-n.urgent:
			n.deliver(e)
		default:
			return
		}
	}
}

// countUndelivered counts the routine events still queued when the worker
// stops as dropped: they will never be sent, and Stats should say so.
func (n *Notifier) countUndelivered() {
	for {
		select {
		case <-n.queue:
			n.mu.Lock()

			n.dropped++
			n.mu.Unlock()
		default:
			return
		}
	}
}

// deliver sends one event to every sink, recording failures in the error log.
// A routine notification is worth one attempt, not a backlog; a halt is
// retried on a rate limit or a server error (sendWithRetry).
//
// Each sink gets its own fresh timeout rather than sharing one context across
// the loop: a shared deadline lets a first slow sink consume the whole
// budget, leaving every sink queued behind it an already-expired context that
// fails instantly regardless of its own health — one dead webhook would have
// silently taken every other configured sink down with it.
func (n *Notifier) deliver(e Event) {
	for _, s := range n.sinks {
		if err := n.sendWithRetry(s, e); err != nil {
			n.mu.Lock()

			n.failed++
			n.mu.Unlock()

			errlog.Recordf("notify", "%s: %v", s.name, err)

			continue
		}

		n.mu.Lock()

		n.sent++
		n.mu.Unlock()
	}
}

// Close stops the delivery worker and waits — bounded — for it to deliver the
// urgent lane. It used to return at once, and cmd/bot exits right after: a
// halt queued just before shutdown was never sent, and NT2-6's drain only held
// while the process kept running (NT3-3). Safe to call more than once, and
// nil-safe.
func (n *Notifier) Close() {
	if n == nil {
		return
	}

	n.closeOnce.Do(func() {
		n.sendMu.Lock()
		close(n.done)
		n.sendMu.Unlock()
	})

	// One attempt per sink for the event in flight, and the urgent lane's
	// retries on top: a bound, so a dead sink cannot hold a shutdown.
	grace := timeoutOr(n.cfg.Timeout) * time.Duration(len(n.sinks)+1) * haltAttempts

	select {
	case <-n.stopped:
	case <-time.After(grace):
	}
}

// haltAttempts / haltRetryCap bound a halt's retries: Discord and Telegram
// rate-limit bursts, and the burst just before a halt (several stop-outs in
// one tick) is what puts the halt itself into a 429 — lost, counted only as
// failed (NT3-4).
const (
	haltAttempts = 3
	haltRetryCap = 30 * time.Second
)

// sendWithRetry sends e to s once, or for a halt up to haltAttempts times on
// a 429 or 5xx, waiting what Retry-After asks (at most haltRetryCap).
func (n *Notifier) sendWithRetry(s sink, e Event) error {
	var err error

	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), timeoutOr(n.cfg.Timeout))
		err = s.send(ctx, n.hc, e)

		cancel()

		var se *statusError
		if err == nil || e.Kind != KindHalt || attempt >= haltAttempts ||
			!errors.As(err, &se) || !se.retryable() {
			return err
		}

		time.Sleep(se.wait(attempt))
	}
}

// statusError is a sink's non-2xx answer.
type statusError struct {
	code       int
	body       string
	retryAfter time.Duration
}

func (e *statusError) Error() string { return fmt.Sprintf("http %d: %s", e.code, e.body) }

// retryable reports a rate limit or a server-side failure.
func (e *statusError) retryable() bool {
	return e.code == http.StatusTooManyRequests || e.code >= 500
}

// wait is how long to pause before the next attempt: Retry-After when the
// sink sent one, else a second per attempt so far — capped either way.
func (e *statusError) wait(attempt int) time.Duration {
	d := e.retryAfter
	if d <= 0 {
		d = time.Duration(attempt) * time.Second
	}

	return min(d, haltRetryCap)
}

// Stats reports delivery counters, exposed at GET /api/notify so a silently
// broken webhook is visible.
type Stats struct {
	Enabled bool     `json:"enabled"`
	Sinks   []string `json:"sinks"`
	Sent    int64    `json:"sent"`
	Failed  int64    `json:"failed"`
	Dropped int64    `json:"dropped"`
}

// Stats returns a snapshot of the counters.
func (n *Notifier) Stats() Stats {
	if n == nil {
		return Stats{}
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	s := Stats{Enabled: true, Sent: n.sent, Failed: n.failed, Dropped: n.dropped}
	for _, sk := range n.sinks {
		s.Sinks = append(s.Sinks, sk.name)
	}

	return s
}

// ----- sinks -----

// postJSONSink posts the event verbatim as JSON — the generic escape hatch for
// Slack-compatible endpoints, automation tools and homegrown scripts.
func postJSONSink(endpoint string) func(context.Context, *http.Client, Event) error {
	return func(ctx context.Context, hc *http.Client, e Event) error {
		body, err := json.Marshal(struct {
			Event
			// text duplicates the message for sinks (Slack, Mattermost) that
			// expect that field and ignore everything else.
			Message string `json:"text"`
		}{Event: e, Message: e.Title + " — " + e.Text})
		if err != nil {
			return err
		}

		return post(ctx, hc, endpoint, "application/json", body)
	}
}

// discordSink posts a plain message to a Discord channel webhook.
func discordSink(endpoint string) func(context.Context, *http.Client, Event) error {
	return func(ctx context.Context, hc *http.Client, e Event) error {
		body, err := json.Marshal(map[string]string{"content": capRunes(format(e), discordMaxLen)})
		if err != nil {
			return err
		}

		return post(ctx, hc, endpoint, "application/json", body)
	}
}

// telegramSink posts a message to a Telegram chat via the Bot API.
func telegramSink(token, chatID string) func(context.Context, *http.Client, Event) error {
	endpoint := "https://api.telegram.org/bot" + token + "/sendMessage"

	return func(ctx context.Context, hc *http.Client, e Event) error {
		form := url.Values{}
		form.Set("chat_id", chatID)
		form.Set("text", capRunes(format(e), telegramMaxLen))
		form.Set("disable_web_page_preview", "true")

		return post(ctx, hc, endpoint, "application/x-www-form-urlencoded",
			[]byte(form.Encode()))
	}
}

// Per-sink message limits. A longer message is refused by the sink (HTTP 400)
// and the alert is lost — a broker error body embedded in "instrument not
// tradeable", or a first screener pass adding 100+ symbols, went over them
// (NT2-5).
const (
	discordMaxLen      = 2000
	telegramMaxLen     = 4096
	pushoverMaxMessage = 1024
	pushoverMaxTitle   = 250
)

// capRunes cuts s to at most n runes, marking the cut, so a long message is
// shortened rather than rejected.
func capRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}

	return string(r[:n-1]) + "…"
}

// pushoverEndpoint is the Pushover message API.
const pushoverEndpoint = "https://api.pushover.net/1/messages.json"

// pushoverMaxPriority is the highest priority this package will send. Pushover
// also defines 2 (emergency, repeats until acknowledged), which needs retry and
// expire parameters plus an acknowledgement flow — a trading bot that can wake
// you every 30 seconds until you tap a button is a footgun, so 1 is the ceiling.
const pushoverMaxPriority = 1

// pushoverSink sends a push notification to phones via Pushover.
//
// Unlike the chat sinks it carries a real title and priority, because that is
// what a phone does something useful with: a halt can bypass a quiet-hours
// setting while a routine fill does not.
// The endpoint is a parameter rather than a constant reference so a test can
// point it at a stub and assert the request this code really builds.
func pushoverSink(
	cfg config.NotifyConfig,
	endpoint string,
) func(context.Context, *http.Client, Event) error {
	return func(ctx context.Context, hc *http.Client, e Event) error {
		form := url.Values{}
		form.Set("token", cfg.PushoverToken)
		form.Set("user", cfg.PushoverUser)
		form.Set("title", capRunes(pushoverTitle(e), pushoverMaxTitle))
		form.Set("message", capRunes(e.Text, pushoverMaxMessage))
		form.Set("priority", strconv.Itoa(pushoverPriority(cfg.PushoverPriority, e.Kind)))

		if e.Text == "" {
			form.Set("message", e.Title) // Pushover rejects an empty message
		}

		if d := strings.TrimSpace(cfg.PushoverDevice); d != "" {
			form.Set("device", d)
		}

		if !e.Time.IsZero() {
			form.Set("timestamp", strconv.FormatInt(e.Time.Unix(), 10))
		}

		return post(ctx, hc, endpoint, "application/x-www-form-urlencoded",
			[]byte(form.Encode()))
	}
}

// pushoverTitle prefixes the event title with its icon, so a glance at the
// notification says what kind of thing happened.
func pushoverTitle(e Event) string {
	if icon := kindIcon(e.Kind); icon != "" {
		return icon + " " + e.Title
	}

	return e.Title
}

// pushoverPriority raises anything that stopped the bot (a halt) or that it
// could not do (an error) one level above the configured baseline, and clamps
// the result to Pushover's accepted range.
func pushoverPriority(base int, kind Kind) int {
	p := base
	if kind == KindHalt || kind == KindError {
		p++
	}

	if p > pushoverMaxPriority {
		p = pushoverMaxPriority
	}

	if p < -2 {
		p = -2
	}

	return p
}

// appriseSink posts to an Apprise API server, which fans the message out to
// whatever that server is configured for.
//
// This is the sink that stops this package growing an adapter per service:
// Apprise already speaks email, Matrix, ntfy, Gotify, Signal, SMS and a hundred
// others, so one HTTP call here reaches all of them. Two deployment shapes are
// supported — a stored configuration (POST /notify/{key}) or the stateless
// endpoint (POST /notify) with the service URLs supplied per request.
func appriseSink(
	cfg config.NotifyConfig,
	endpoint string,
) func(context.Context, *http.Client, Event) error {
	services := strings.TrimSpace(cfg.AppriseServices)
	tag := strings.TrimSpace(cfg.AppriseTag)

	return func(ctx context.Context, hc *http.Client, e Event) error {
		payload := map[string]string{
			"title": e.Title,
			"body":  e.Text,
			"type":  appriseType(e.Kind),
		}

		if payload["body"] == "" {
			payload["body"] = e.Title
		}

		if services != "" {
			payload["urls"] = services
		}

		if tag != "" {
			payload["tag"] = tag
		}

		body, err := json.Marshal(payload)
		if err != nil {
			return err
		}

		return post(ctx, hc, endpoint, "application/json", body)
	}
}

// appriseType maps an event kind onto Apprise's notification types, which many
// of its downstream services render differently (colour, icon, urgency).
func appriseType(kind Kind) string {
	switch kind {
	case KindTrade:
		return "success"
	case KindBlocked:
		return "warning"
	case KindHalt, KindError:
		return "failure"
	case KindAudit:
		return "info"
	}

	return "info"
}

// kindIcon returns the emoji marking an event kind.
func kindIcon(kind Kind) string {
	return map[Kind]string{
		KindTrade: "✅", KindBlocked: "🚫", KindHalt: "⛔", KindError: "⚠️", KindAudit: "🔧",
	}[kind]
}

// format renders an event as the one-line message a chat sink shows.
func format(e Event) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s %s", kindIcon(e.Kind), e.Title)

	if e.Text != "" {
		fmt.Fprintf(&b, "\n%s", e.Text)
	}

	return b.String()
}

// post sends a body and treats any non-2xx as an error, carrying a snippet so a
// bad token or a revoked webhook is legible in the error log.
func post(ctx context.Context, hc *http.Client, endpoint, contentType string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		// A *url.Error too, from parsing the endpoint — and it embeds the URL,
		// which for most sinks IS the secret. Only the transport error was
		// sanitized before, so a malformed sink URL leaked its token into
		// errlog on every alert (NT2-1).
		return sanitizeTransportErr(err)
	}

	req.Header.Set("Content-Type", contentType)

	resp, err := hc.Do(req)
	if err != nil {
		return sanitizeTransportErr(err)
	}

	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))

		se := &statusError{code: resp.StatusCode, body: strings.TrimSpace(string(snippet))}
		if secs, err := strconv.Atoi(
			strings.TrimSpace(resp.Header.Get("Retry-After")),
		); err == nil &&
			secs > 0 {
			se.retryAfter = time.Duration(secs) * time.Second
		}

		return se
	}

	return nil
}

// sanitizeTransportErr strips the request URL from an http.Client transport
// failure before it is returned. hc.Do wraps a DNS/TCP/TLS/timeout failure in
// a *url.Error whose Error() string embeds the full request URL verbatim —
// for a webhook sink that URL often carries the secret itself (a Telegram bot
// token, a Discord webhook path, an Apprise service URL). This error is
// recorded into the process-wide errlog ring (see the Send loop below), which
// is served by an unauthenticated-by-default API endpoint, so leaving the URL
// in place is a real credential leak, not just noise. See
// CODE_REVIEW_FINDINGS.md CRIT-12.
func sanitizeTransportErr(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return fmt.Errorf("%s request failed: %w", uerr.Op, uerr.Err)
	}

	return err
}
