package market

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"
)

// Mock is an offline DataProvider that generates a random-walk price stream and
// synthetic historical candles. It is used when no Finnhub key is configured so
// the bot is runnable end-to-end without network access.
type Mock struct {
	out      chan Quote
	interval time.Duration

	mu     sync.Mutex
	prices map[string]float64
	last   map[string]Quote // latest emitted tick per symbol (for LastQuote)
	closed bool
	done   chan struct{}
}

// NewMock returns a Mock provider emitting a tick per symbol every interval.
func NewMock(interval time.Duration) *Mock {
	if interval <= 0 {
		interval = time.Second
	}
	m := &Mock{
		out:      make(chan Quote, 256),
		interval: interval,
		prices:   make(map[string]float64),
		last:     make(map[string]Quote),
		done:     make(chan struct{}),
	}
	go m.run()
	return m
}

func (m *Mock) Quotes() <-chan Quote { return m.out }

// LastQuote returns the most recent tick emitted for symbol.
func (m *Mock) LastQuote(symbol string) (Quote, bool) {
	m.mu.Lock()
	q, ok := m.last[symbol]
	m.mu.Unlock()
	return q, ok
}

func (m *Mock) Subscribe(symbols ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("provider closed")
	}
	for _, s := range symbols {
		if _, ok := m.prices[s]; !ok {
			// Seed a deterministic-ish starting price per symbol.
			m.prices[s] = 50 + rand.Float64()*200
		}
	}
	return nil
}

func (m *Mock) Unsubscribe(symbols ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range symbols {
		delete(m.prices, s)
	}
	return nil
}

func (m *Mock) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	close(m.done)
	return nil
}

func (m *Mock) run() {
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-m.done:
			close(m.out)
			return
		case <-t.C:
			m.mu.Lock()
			snapshot := make(map[string]Quote, len(m.prices))
			now := time.Now()
			for s, p := range m.prices {
				// Gaussian-ish random walk, ~0.3% step.
				np := p * (1 + (rand.Float64()-0.5)*0.006)
				m.prices[s] = np
				q := Quote{Symbol: s, Price: round2(np), Volume: 1, Time: now}
				snapshot[s] = q
				m.last[s] = q
			}
			m.mu.Unlock()
			for _, q := range snapshot {
				select {
				case m.out <- q:
				case <-m.done:
					close(m.out)
					return
				default:
				}
			}
		}
	}
}

// Candles synthesises a deterministic random-walk history ending near the
// symbol's current price, useful for indicator warmup in offline mode.
func (m *Mock) Candles(_ context.Context, symbol string, res Resolution, from, to time.Time) ([]Candle, error) {
	// Deterministic per symbol (independent of the live, globally-seeded price)
	// so backtests/optimisation are reproducible across runs.
	step := resolutionDuration(res)
	span := to.Sub(from)
	n := int(span / step) // honour the requested range instead of a fixed count
	if n < 2 {
		n = 2
	}
	if n > 500000 {
		n = 500000 // guard against pathological ranges
	}
	rng := rand.New(rand.NewSource(int64(hash(symbol))))
	price := 50 + rng.Float64()*200 // deterministic base price from the symbol
	p := price * 0.9
	out := make([]Candle, n)
	for i := 0; i < n; i++ {
		open := p
		p = p * (1 + (rng.Float64()-0.48)*0.01)
		high := math.Max(open, p) * (1 + rng.Float64()*0.003)
		low := math.Min(open, p) * (1 - rng.Float64()*0.003)
		out[i] = Candle{
			Symbol: symbol,
			Open:   round2(open),
			High:   round2(high),
			Low:    round2(low),
			Close:  round2(p),
			Volume: 1000 + rng.Float64()*5000,
			Time:   from.Add(time.Duration(i) * step),
		}
	}
	return out, nil
}

// resolutionDuration maps a candle resolution to its bar interval.
func resolutionDuration(res Resolution) time.Duration {
	switch res {
	case Res5Min:
		return 5 * time.Minute
	case Res15Min:
		return 15 * time.Minute
	case Res1Hour:
		return time.Hour
	case Res1Day:
		return 24 * time.Hour
	default:
		return time.Minute
	}
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func hash(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}
