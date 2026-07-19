package engine

import (
	"context"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// fakeHistory is a HistorySource returning a single candle at a fixed price.
type fakeHistory struct {
	price  float64
	err    error
	called int
}

func (f *fakeHistory) Candles(
	_ context.Context,
	symbol string,
	res market.Resolution,
	from, to time.Time,
) ([]market.Candle, error) {
	f.called++
	if f.err != nil {
		return nil, f.err
	}
	if f.price <= 0 {
		return nil, nil
	}
	return []market.Candle{{Symbol: symbol, Close: f.price, Time: time.Now()}}, nil
}

func TestConfirmVenuePrice(t *testing.T) {
	cfg := config.EngineConfig{VenuePriceCheck: true, MaxPriceDeviation: 0.02}

	// Within tolerance: returns the venue price (not the signal price) for sizing.
	e := &Engine{cfg: cfg, history: &fakeHistory{price: 101}}
	if px, _, ok := e.confirmVenuePrice("AAPL", 100); !ok || px != 101 {
		t.Errorf("within tolerance => px %.2f ok %v, want 101/true", px, ok)
	}

	// Beyond tolerance (5% > 2%): blocked.
	e = &Engine{cfg: cfg, history: &fakeHistory{price: 105}}
	if _, reason, ok := e.confirmVenuePrice("AAPL", 100); ok || reason == "" {
		t.Errorf("5%% move => ok %v reason %q, want blocked with reason", ok, reason)
	}

	// No venue price available: blocked.
	e = &Engine{cfg: cfg, history: &fakeHistory{price: 0}}
	if _, _, ok := e.confirmVenuePrice("AAPL", 100); ok {
		t.Error("missing venue price should block the buy")
	}

	// Check disabled: passes the signal price straight through, no fetch.
	fh := &fakeHistory{price: 999}
	e = &Engine{cfg: config.EngineConfig{VenuePriceCheck: false}, history: fh}
	if px, _, ok := e.confirmVenuePrice("AAPL", 100); !ok || px != 100 {
		t.Errorf("disabled => px %.2f ok %v, want 100/true", px, ok)
	}
	if fh.called != 0 {
		t.Errorf("disabled check should not fetch a venue price (called %d)", fh.called)
	}

	// Deviation veto disabled (0): resizes to the venue price even on a big move.
	e = &Engine{
		cfg:     config.EngineConfig{VenuePriceCheck: true, MaxPriceDeviation: 0},
		history: &fakeHistory{price: 130},
	}
	if px, _, ok := e.confirmVenuePrice("AAPL", 100); !ok || px != 130 {
		t.Errorf("no-veto => px %.2f ok %v, want 130/true", px, ok)
	}
}
