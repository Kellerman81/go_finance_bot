package engine

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
	"github.com/Kellerman81/go_finance_bot/internal/config"
	"github.com/Kellerman81/go_finance_bot/internal/market"
	"github.com/Kellerman81/go_finance_bot/internal/portfolio"
	"github.com/Kellerman81/go_finance_bot/internal/risk"
)

// TestRunHaltsOnClosedQuoteStream reproduces the closed-quote-stream
// finding: before this fix, a closed provider quote channel was logged and
// then silently ignored forever — the engine kept ticking its evaluation
// loop against whatever price was last cached, with no operator-visible
// signal that the feed was gone. It must now halt automated trading and
// record it, exactly like a protection halt.
func TestRunHaltsOnClosedQuoteStream(t *testing.T) {
	provider := market.NewMock(time.Hour) // long interval: no ticks of its own during this test
	defer provider.Close()

	pf := portfolio.New()
	pf.SetCash(1000)

	brk := broker.NewSim(1000, func(string) (float64, bool) { return 0, false }, nil)
	rm := risk.New(config.Limits{MaxOpenPositions: 10}, config.CostsConfig{})
	prices := NewPriceCache()

	e := New(
		config.EngineConfig{EvalInterval: time.Hour, TradingEnabled: true},
		config.ExitsConfig{}, config.SafetyConfig{}, config.StrategyConfig{},
		provider, brk, rm, pf, prices, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})

	go func() {
		e.Run(ctx)
		close(done)
	}()

	// Close the provider's quote channel — simulating a feed that has given
	// up for good (e.g. Finnhub's websocket closing, or the process's own
	// Close() being called early).
	_ = provider.Close()

	deadline := time.After(2 * time.Second)

	for {
		if !e.Status().TradingEnabled {
			break
		}

		select {
		case <-deadline:
			t.Fatal("trading was never disabled after the quote stream closed")
		case <-time.After(10 * time.Millisecond):
		}
	}

	found := false

	for _, tr := range e.Trades() {
		if tr.Status == "halted" {
			found = true

			break
		}
	}

	if !found {
		t.Error("no \"halted\" trade record was written for the closed quote stream")
	}

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}
