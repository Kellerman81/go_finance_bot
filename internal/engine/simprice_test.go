package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/notify"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// thinListingEngine builds an engine holding a symbol that warmup seeded from
// provider history but whose live feed has never ticked — the shape of the
// reported case (NTA.F, UNPRF: thin Frankfurt / OTC listings).
func thinListingEngine(t *testing.T) *Engine {
	t.Helper()

	e, _ := forgetEngine()
	e.brk = &fakeRealBroker{cash: 100000}
	e.rm = risk.New(config.Limits{
		MaxOrderValue: 1e9, MaxPerPosition: 1e9, MaxTotalInvested: 1e9,
		MaxDailySpend: 1e9, MaxOpenPositions: 10,
	}, config.CostsConfig{})
	e.pf = portfolio.New()
	e.prices = NewPriceCache()
	e.barsStore = newBarStore(maxBars, time.Minute)
	e.signals = make(map[string]strategy.Signal)
	e.spreads = make(map[string]float64)
	e.cfg = config.EngineConfig{EvalInterval: 30 * time.Second}

	e.barsStore.add("NTA.F")
	e.barsStore.seed("NTA.F", []market.Candle{
		{Symbol: "NTA.F", Open: 157, High: 158, Low: 156, Close: 157.32,
			Time: time.Now().Add(-2 * time.Hour)},
	})

	return e
}

// TestReferencePriceFallsBackToBarHistory is the core of the TM-1 fix: the
// engine prices an order from the tick cache when it has one and from the last
// bar otherwise, so the simulated broker can fill exactly what the engine is
// willing to authorise.
func TestReferencePriceFallsBackToBarHistory(t *testing.T) {
	e := thinListingEngine(t)

	if _, ok := e.prices.Get("NTA.F"); ok {
		t.Fatal("precondition: the tick cache must be empty for this symbol")
	}

	px, ok := e.ReferencePrice("NTA.F")
	if !ok || px != 157.32 {
		t.Errorf("ReferencePrice = %v, %v; want the last bar's close 157.32, true", px, ok)
	}

	// A live tick, once one arrives, outranks the bar history.
	e.prices.Set("NTA.F", 160)

	if px, ok := e.ReferencePrice("NTA.F"); !ok || px != 160 {
		t.Errorf("ReferencePrice = %v, %v; want the live tick 160, true", px, ok)
	}

	// A symbol with neither is still unpriceable, rather than reporting 0 as a
	// usable price.
	if px, ok := e.ReferencePrice("NOSUCH"); ok {
		t.Errorf("ReferencePrice for an unknown symbol = %v, true; want ok=false", px)
	}
}

// A warmup close must NOT be written back into the tick cache: checkExits uses
// PriceCache.GetFresh precisely to refuse a stale price when deciding a
// stop-loss, and an hours-old bar close masquerading as a fresh tick would
// defeat it.
func TestReferencePriceDoesNotPoisonTheTickCache(t *testing.T) {
	e := thinListingEngine(t)

	if _, ok := e.ReferencePrice("NTA.F"); !ok {
		t.Fatal("expected a reference price from the bar history")
	}

	if _, ok := e.prices.Get("NTA.F"); ok {
		t.Error("ReferencePrice seeded the tick cache; a stale bar close would then " +
			"read as a fresh tick to checkExits' GetFresh staleness guard")
	}
}

// TestTestModeFillsASymbolWithHistoryButNoTicks reproduces the reported bug end
// to end: every order for such a symbol used to be rejected with "sim: no price
// for X", once per evaluation, forever — nothing ever filled, and each attempt
// raised its own notification.
func TestTestModeFillsASymbolWithHistoryButNoTicks(t *testing.T) {
	e := thinListingEngine(t)

	if err := e.EnableTestMode(context.Background(), time.Hour); err != nil {
		t.Fatalf("EnableTestMode: %v", err)
	}

	sig := strategy.Signal{
		Symbol: "NTA.F", Action: strategy.Buy, Strength: 0.9,
		Price: 157.32, Reason: "ema:BUY(0.90)",
	}

	for range 4 {
		e.act(context.Background(), sig, nil, e.tradingOn())
	}

	var filled, errored int

	for _, tr := range e.trades {
		switch tr.Status {
		case "filled":
			filled++
		case "error":
			errored++
			t.Logf("error row: %s %s %q", tr.Symbol, tr.Side, tr.Err)
		}
	}

	if errored > 0 {
		t.Errorf("%d order(s) failed; the sim must be able to price what the engine "+
			"authorised (it priced it from the same bar history)", errored)
	}

	if filled != 1 {
		t.Errorf("filled buys = %d, want exactly 1 — the first fills, and the pyramid "+
			"guard blocks the rest", filled)
	}
}

// countingNotifier is a Notifier wired to a local webhook that records how many
// events actually reached a sink.
func countingNotifier(t *testing.T) (*notify.Notifier, func() int) {
	t.Helper()

	var (
		mu   sync.Mutex
		hits int
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	n := notify.New(config.NotifyConfig{
		Enabled: true, Webhook: srv.URL, Timeout: 2 * time.Second,
	})
	if n == nil {
		t.Fatal("notifier not built")
	}

	t.Cleanup(n.Close)

	return n, func() int {
		mu.Lock()
		defer mu.Unlock()

		return hits
	}
}

// TestRepeatingOrderFailureIsAnnouncedOnce reproduces the notification half of
// the report: a failing order is retried every evaluation for as long as the
// signal holds, and every attempt used to raise its own notification — "sim: no
// price for NTA.F" arriving every 30 seconds, indefinitely. The trade log must
// still record each attempt; only the notification is suppressed.
func TestRepeatingOrderFailureIsAnnouncedOnce(t *testing.T) {
	e, _ := forgetEngine()

	n, hits := countingNotifier(t)
	e.SetNotifier(n)

	same := TradeRecord{
		Time: time.Now(), Symbol: "NTA.F", Side: broker.Buy, Qty: 0.95,
		Status: "error", Err: "sim: no price for NTA.F",
	}

	for range 5 {
		e.record(same)
	}

	if len(e.trades) != 5 {
		t.Errorf("trade log has %d rows, want 5 — the log must record every attempt", len(e.trades))
	}

	waitForNotifications(t, hits, 1)

	if got := hits(); got != 1 {
		t.Errorf("sent %d notifications for the same repeating failure, want 1", got)
	}

	// A DIFFERENT failure for the same symbol is news and must be announced.
	changed := same
	changed.Err = "sim: insufficient cash (need 150.00, have 12.00)"
	e.record(changed)

	waitForNotifications(t, hits, 2)

	if got := hits(); got != 2 {
		t.Errorf("sent %d notifications, want 2 — a different error must still be announced", got)
	}

	// After a success the condition has evidently cleared, so a recurrence is
	// news again rather than a continuation.
	e.record(TradeRecord{
		Time: time.Now(), Symbol: "NTA.F", Side: broker.Buy, Qty: 0.95,
		Price: 157.32, Status: "filled",
	})
	e.record(same)

	waitForNotifications(t, hits, 4) // the fill, then the recurrence

	if got := hits(); got != 4 {
		t.Errorf("sent %d notifications, want 4 (fill + re-announced failure after recovery)", got)
	}
}

// A failure for a different symbol is always its own announcement.
func TestOrderFailureSuppressionIsPerSymbol(t *testing.T) {
	e, _ := forgetEngine()

	n, hits := countingNotifier(t)
	e.SetNotifier(n)

	for _, sym := range []string{"NTA.F", "UNPRF", "NTA.F", "UNPRF"} {
		e.record(TradeRecord{
			Time: time.Now(), Symbol: sym, Side: broker.Sell,
			Status: "error", Err: "sim: no price for " + sym,
		})
	}

	waitForNotifications(t, hits, 2)

	if got := hits(); got != 2 {
		t.Errorf("sent %d notifications, want 2 (one per symbol)", got)
	}
}

// waitForNotifications waits for the notifier's async queue to deliver at least
// want events, so a count assertion is not racing the worker.
func waitForNotifications(t *testing.T, hits func() int, want int) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if hits() >= want {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}
}
