package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/broker"
)

// resolvingBroker records which symbols were resolved, and can refuse one.
type resolvingBroker struct {
	broker.Broker

	mu       sync.Mutex
	asked    []string
	refuse   map[string]bool
	blockFor time.Duration
}

func (b *resolvingBroker) ResolveSymbol(ctx context.Context, symbol string) (string, error) {
	if b.blockFor > 0 {
		select {
		case <-time.After(b.blockFor):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.asked = append(b.asked, symbol)

	if b.refuse[symbol] {
		return "", errors.New("no instrument")
	}

	return symbol + "_X_EQ", nil
}

func (b *resolvingBroker) seen() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]string(nil), b.asked...)
}

func (b *resolvingBroker) Name() string { return "resolving" }

// resolveEngine builds an engine over a broker that resolves symbols.
func resolveEngine(brk broker.Broker) *Engine {
	return &Engine{
		brk: brk,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// Every watched symbol must be resolved before the first portfolio sync.
//
// The broker maps its own ticker back to the bot's symbol from a table filled
// in when a symbol is resolved — which used to happen only when an order was
// placed. That table is in memory, so a restart left a long-held position
// reporting under the broker's name, matching no open-position record: the ROI
// ladder and max_hold switched off for it, its trailing peak restarted, and a
// protective exit would have tried to sell a name the broker cannot resolve.
func TestResolveTickersCoversEverySymbol(t *testing.T) {
	t.Parallel()

	brk := &resolvingBroker{}
	e := resolveEngine(brk)

	e.resolveTickers([]string{"PRAJL.XC", "MRVL", "NVDA"})

	seen := brk.seen()
	if len(seen) != 3 {
		t.Fatalf("resolved %v, want all three symbols", seen)
	}
}

// A symbol this broker does not offer is still worth tracking — signals, charts,
// a paper position — so a refusal must not stop the rest.
func TestResolveTickersSurvivesARefusal(t *testing.T) {
	t.Parallel()

	brk := &resolvingBroker{refuse: map[string]bool{"UNKNOWN": true}}
	e := resolveEngine(brk)

	e.resolveTickers([]string{"UNKNOWN", "MRVL"})

	if seen := brk.seen(); len(seen) != 2 {
		t.Errorf("resolved %v; a refusal must not abort the pass", seen)
	}
}

// A broker with no resolver is not an error: most are addressed by plain ticker.
func TestResolveTickersIgnoresBrokersWithoutAResolver(t *testing.T) {
	t.Parallel()

	e := resolveEngine(plainBroker{})

	e.resolveTickers([]string{"AAPL"}) // must not panic
}

// plainBroker has no ResolveSymbol.
type plainBroker struct{ broker.Broker }

func (plainBroker) Name() string { return "plain" }

// Start-up must not hang on a broker that is slow or down.
//
// Not parallel: it shortens the package-level timeout.
func TestResolveTickersIsBounded(t *testing.T) {
	restore := resolveTimeout
	resolveTimeout = 200 * time.Millisecond

	t.Cleanup(func() { resolveTimeout = restore })

	brk := &resolvingBroker{blockFor: 5 * time.Second}
	e := resolveEngine(brk)

	done := make(chan struct{})

	go func() {
		e.resolveTickers([]string{"SLOW"})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("resolveTickers did not return within its own timeout; " +
			"a broker that hangs would prevent the bot from starting")
	}
}
