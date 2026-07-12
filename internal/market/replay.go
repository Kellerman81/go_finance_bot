package market

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Replay is an offline DataProvider backed by preloaded candle history. It serves
// that history through Candles and, when an emit interval is set, streams each
// symbol's candle closes as live ticks — so recorded/CSV data can drive the
// engine through exactly the same interface as a live feed. Feed it data from
// backtest.LoadCSV (or any map of candles).
type Replay struct {
	interval time.Duration // tick cadence; 0 = history-only (no streaming)
	out      chan Quote

	mu         sync.Mutex
	data       map[string][]Candle
	subscribed map[string]struct{}
	cursor     map[string]int
	last       map[string]Quote
	closed     bool
	done       chan struct{}
}

// NewReplay builds a replay provider over the given per-symbol candle history.
// If interval > 0 it emits one tick per subscribed symbol every interval,
// advancing through that symbol's candles; interval == 0 serves history only.
func NewReplay(data map[string][]Candle, interval time.Duration) *Replay {
	r := &Replay{
		interval:   interval,
		out:        make(chan Quote, 256),
		data:       data,
		subscribed: make(map[string]struct{}),
		cursor:     make(map[string]int),
		last:       make(map[string]Quote),
		done:       make(chan struct{}),
	}
	if r.data == nil {
		r.data = map[string][]Candle{}
	}
	if interval > 0 {
		go r.run()
	}
	return r
}

func (r *Replay) Quotes() <-chan Quote { return r.out }

func (r *Replay) LastQuote(symbol string) (Quote, bool) {
	r.mu.Lock()
	q, ok := r.last[symbol]
	r.mu.Unlock()
	return q, ok
}

func (r *Replay) Subscribe(symbols ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return fmt.Errorf("provider closed")
	}
	for _, s := range symbols {
		if s != "" {
			r.subscribed[s] = struct{}{}
		}
	}
	return nil
}

func (r *Replay) Unsubscribe(symbols ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range symbols {
		delete(r.subscribed, s)
	}
	return nil
}

// Candles returns the preloaded history for symbol within [from, to]. The
// resolution is ignored — the data is served at whatever resolution it was
// loaded — so callers should load at the resolution they intend to replay.
func (r *Replay) Candles(_ context.Context, symbol string, _ Resolution, from, to time.Time) ([]Candle, error) {
	r.mu.Lock()
	all := r.data[symbol]
	r.mu.Unlock()
	return sliceByTime(all, from, to), nil
}

func (r *Replay) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	close(r.done)
	return nil
}

// run advances each subscribed symbol's cursor once per interval, emitting the
// candle's close as a tick until that symbol's data is exhausted.
func (r *Replay) run() {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-r.done:
			close(r.out)
			return
		case <-t.C:
			r.mu.Lock()
			batch := make([]Quote, 0, len(r.subscribed))
			for s := range r.subscribed {
				candles := r.data[s]
				i := r.cursor[s]
				if i >= len(candles) {
					continue
				}
				c := candles[i]
				r.cursor[s] = i + 1
				q := Quote{Symbol: s, Price: c.Close, Volume: c.Volume, Time: c.Time}
				r.last[s] = q
				batch = append(batch, q)
			}
			r.mu.Unlock()
			for _, q := range batch {
				select {
				case r.out <- q:
				case <-r.done:
					close(r.out)
					return
				default:
				}
			}
		}
	}
}
