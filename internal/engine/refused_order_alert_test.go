package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/notify"
)

// canceledBroker confirms every order as Alpaca's "canceled" with nothing filled.
type canceledBroker struct{ rejectingBroker }

func (canceledBroker) SubmitOrder(_ context.Context, o broker.Order) (broker.OrderResult, error) {
	return broker.OrderResult{
		ID: "1", Symbol: o.Symbol, Side: o.Side, Qty: o.Qty, Status: "canceled", Terminal: true,
	}, nil
}

// webhookEventsWith is webhookEvents with a notify config of the test's own, and
// a fixed wait in place of polling for a count.
func webhookEventsWith(t *testing.T, cfg config.NotifyConfig) (*notify.Notifier, func() []map[string]any) {
	t.Helper()

	var (
		mu  sync.Mutex
		got []map[string]any
	)

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)

		var ev map[string]any

		_ = json.Unmarshal(b, &ev)

		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)

	cfg.Enabled = true
	cfg.Webhook = srv.URL

	n := notify.New(cfg)
	t.Cleanup(n.Close)

	return n, func() []map[string]any {
		time.Sleep(300 * time.Millisecond)

		mu.Lock()
		defer mu.Unlock()

		return append([]map[string]any(nil), got...)
	}
}

// NT3-2: a broker-refused stop-loss sell carries the broker's word as its
// status — "rejected", Alpaca's "canceled" — and alert recognised only the
// engine's own "cancelled", so the refusal went out as "✅ SELL X qty 0.0000",
// on every evaluation. The failed stop is the event an operator most needs.
func TestARefusedStopIsAnnouncedAsAFailureOnce(t *testing.T) {
	for _, brk := range []broker.Broker{rejectingBroker{}, canceledBroker{}} {
		e := ledgerEngine(t)
		e.brk = brk

		n, events := webhookEventsWith(t, config.NotifyConfig{})
		e.SetNotifier(n)

		for range 3 {
			e.exitPosition(context.Background(), "X", 2, 120, "stop-loss", true, false, e.pf.Snapshot())
		}

		got := events()
		if len(got) != 1 || got[0]["kind"] == "trade" {
			t.Errorf("%T: %d alerts (first kind %v), want one non-trade alert", brk, len(got), got[0]["kind"])
		}
	}
}

// NT3-2 under min_notional: the refusal has notional 0, and min_notional drops
// small TRADES — so it was suppressed without even a dropped count.
func TestARefusedStopIsAnnouncedUnderMinNotional(t *testing.T) {
	e := ledgerEngine(t)
	e.brk = rejectingBroker{}

	n, events := webhookEventsWith(t, config.NotifyConfig{MinNotional: 50})
	e.SetNotifier(n)

	e.exitPosition(context.Background(), "X", 2, 120, "stop-loss", true, false, e.pf.Snapshot())

	if len(events()) == 0 {
		t.Error("a refused stop-loss produced no notification under min_notional 50")
	}
}
