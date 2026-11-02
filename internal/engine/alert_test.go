package engine

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/notify"
)

// webhookEvents starts a webhook that records every event it receives.
func webhookEvents(t *testing.T) (*notify.Notifier, func() []map[string]any) {
	t.Helper()

	var (
		mu  sync.Mutex
		got []map[string]any
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)

		var ev map[string]any
		_ = json.Unmarshal(b, &ev)

		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)

	n := notify.New(config.NotifyConfig{
		Enabled: true, Webhook: srv.URL,
		Events: []string{"trade", "blocked", "halt", "error", "audit"},
	})
	t.Cleanup(n.Close)

	return n, func() []map[string]any {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			n := len(got)
			mu.Unlock()

			if n > 0 {
				break
			}

			time.Sleep(time.Millisecond)
		}

		mu.Lock()
		defer mu.Unlock()

		return append([]map[string]any(nil), got...)
	}
}

// TestFailedOrderAlertSaysItFailed is the regression for NT2-3: a failed order
// was announced with the same title as a fill and without the broker's error —
// a failed stop-loss sell read "SELL AAPL / qty 10 @ 150.00 / stop_loss".
func TestFailedOrderAlertSaysItFailed(t *testing.T) {
	e, _ := forgetEngine()
	n, events := webhookEvents(t)
	e.SetNotifier(n)

	e.alert(TradeRecord{
		Time: time.Now(), Symbol: "AAPL", Side: broker.Sell, Qty: 10, Price: 150,
		Status: "error", Reason: "stop_loss", Err: "http 400: insufficient position",
	})

	got := events()
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}

	title, _ := got[0]["title"].(string)
	text, _ := got[0]["text"].(string)

	if !strings.Contains(title, "FAILED") || !strings.Contains(text, "insufficient position") {
		t.Errorf("failure alert = %q / %q, want it marked failed and carrying the error", title, text)
	}
}

// TestCancelledOrderIsNotAnnouncedAsAFill is the regression for NT2-4's fill
// half: alert mapped every status it did not name to KindTrade, so a cancel
// went out as a ✅ fill of qty 0, and an unfilled bracket entry as a fill at
// full size and value.
func TestCancelledOrderIsNotAnnouncedAsAFill(t *testing.T) {
	e, _ := forgetEngine()
	n, events := webhookEvents(t)
	e.SetNotifier(n)

	e.alert(TradeRecord{Time: time.Now(), Symbol: "AAPL", Side: broker.Buy, Status: "cancelled"})

	got := events()
	if len(got) != 1 || got[0]["kind"] == "trade" {
		t.Errorf("a cancelled order was announced as %v, want a non-trade kind", got)
	}
}
