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
		cfg:    cfg,
		kinds:  parseKinds(cfg.Events),
		hc:     &http.Client{Timeout: timeoutOr(cfg.Timeout)},
		queue:  make(chan Event, queueSize),
		urgent: make(chan Event, urgentQueueSize),
		done:   make(chan struct{}),
	}

	if u := strings.TrimSpace(cfg.Webhook); u != "" {
		n.sinks = append(n.sinks, sink{"webhook", postJSONSink(u)})
	}

	if u := strings.TrimSpace(cfg.DiscordWebhook); u != "" {
		n.sinks = append(n.sinks, sink{"discord", discordSink(u)})
	}

	if cfg.TelegramToken != "" && cfg.TelegramChatID != "" {
		n.sinks = append(n.sinks, sink{
			"telegram",
			telegramSink(cfg.TelegramToken, cfg.TelegramChatID),
		})
	}

	if cfg.PushoverToken != "" && cfg.PushoverUser != "" {
		n.sinks = append(n.sinks, sink{"pushover", pushoverSink(cfg, pushoverEndpoint)})
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
			return
		case e := <-n.urgent:
			n.deliver(e)
		case e := <-n.queue:
			n.deliver(e)
		}
	}
}

// deliver sends one event to every sink, recording failures in the error log
// rather than retrying: a notification is worth one attempt, not a backlog.
//
// Each sink gets its own fresh timeout rather than sharing one context across
// the loop: a shared deadline lets a first slow sink consume the whole
// budget, leaving every sink queued behind it an already-expired context that
// fails instantly regardless of its own health — one dead webhook would have
// silently taken every other configured sink down with it.
func (n *Notifier) deliver(e Event) {
	for _, s := range n.sinks {
		ctx, cancel := context.WithTimeout(context.Background(), timeoutOr(n.cfg.Timeout))

		err := s.send(ctx, n.hc, e)

		cancel()

		if err != nil {
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

// Close stops the delivery worker. Safe to call more than once, and nil-safe.
func (n *Notifier) Close() {
	if n == nil {
		return
	}

	n.closeOnce.Do(func() { close(n.done) })
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
		body, err := json.Marshal(map[string]string{"content": format(e)})
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
		form.Set("text", format(e))
		form.Set("disable_web_page_preview", "true")

		return post(ctx, hc, endpoint, "application/x-www-form-urlencoded",
			[]byte(form.Encode()))
	}
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
		form.Set("title", pushoverTitle(e))
		form.Set("message", e.Text)
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
		return err
	}

	req.Header.Set("Content-Type", contentType)

	resp, err := hc.Do(req)
	if err != nil {
		return sanitizeTransportErr(err)
	}

	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))

		return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
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
