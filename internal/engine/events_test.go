package engine

import (
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

func TestEventBusDeliversAndUnsubscribes(t *testing.T) {
	e := &Engine{}
	ch, cancel := e.Subscribe(4)

	e.emit(Event{Type: EventTrade, Time: time.Now(), Symbol: "AAPL"})
	select {
	case ev := <-ch:
		if ev.Type != EventTrade || ev.Symbol != "AAPL" {
			t.Errorf("got %+v, want trade/AAPL", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("event not delivered")
	}

	// After cancel the channel is closed and no more events arrive.
	cancel()
	if _, ok := <-ch; ok {
		t.Error("channel should be closed after cancel")
	}
	// Emitting after everyone unsubscribed is a no-op (must not panic).
	e.emit(Event{Type: EventSignal})
}

func TestEventBusDropsWhenFull(t *testing.T) {
	e := &Engine{}
	ch, cancel := e.Subscribe(1)
	defer cancel()
	// Fill the buffer, then over-emit — the extra must be dropped, not block.
	for i := 0; i < 5; i++ {
		e.emit(Event{Type: EventSignal})
	}
	if len(ch) != 1 {
		t.Errorf("buffered %d events, want 1 (rest dropped)", len(ch))
	}
}

func TestSetSignalEmitsOnActionChange(t *testing.T) {
	e := &Engine{signals: make(map[string]strategy.Signal)}
	ch, cancel := e.Subscribe(8)
	defer cancel()

	e.setSignal("AAPL", strategy.Signal{Symbol: "AAPL", Action: strategy.Buy})
	e.setSignal("AAPL", strategy.Signal{Symbol: "AAPL", Action: strategy.Buy}) // no change
	e.setSignal("AAPL", strategy.Signal{Symbol: "AAPL", Action: strategy.Sell})

	// Two emissions expected: first BUY (new) and the flip to SELL; the repeat BUY
	// is suppressed.
	got := 0
	for {
		select {
		case <-ch:
			got++
			continue
		case <-time.After(50 * time.Millisecond):
		}
		break
	}
	if got != 2 {
		t.Errorf("emitted %d signal events, want 2 (new + change, repeat suppressed)", got)
	}
}

func TestMetricsSnapshot(t *testing.T) {
	e := &Engine{}
	e.met.onTick(true)
	e.met.onTick(false)
	e.met.onOrder()
	e.met.onEval(2 * time.Millisecond)
	e.met.setOpenOrders(3)

	m := e.Metrics()
	if m.TicksIngested != 2 {
		t.Errorf("ticks = %d, want 2", m.TicksIngested)
	}
	if m.BarsCompleted != 1 {
		t.Errorf("bars = %d, want 1", m.BarsCompleted)
	}
	if m.OrdersSubmitted != 1 {
		t.Errorf("orders = %d, want 1", m.OrdersSubmitted)
	}
	if m.EvalCycles != 1 || m.LastEvalMillis != 2 {
		t.Errorf("eval cycles/ms = %d/%.1f, want 1/2.0", m.EvalCycles, m.LastEvalMillis)
	}
	if m.OpenOrders != 3 {
		t.Errorf("open orders = %d, want 3", m.OpenOrders)
	}
	if m.LastEvalTime.IsZero() {
		t.Error("last eval time should be set")
	}
}
