package engine

import (
	"sync"
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/market"
)

// barStore holds per-symbol rolling candle history in fixed-capacity ring
// buffers plus the in-progress bar builder, behind its own lock. Keeping this
// state off the engine's main mutex means the hot per-tick ingest path does not
// contend with strategy evaluation or API snapshot reads, and the ring gives
// O(1) appends with no reallocation or memory growth once full.
type barStore struct {
	mu       sync.RWMutex
	capacity int
	interval time.Duration
	rings    map[string]*ring
	builders map[string]*barBuilder
}

func newBarStore(capacity int, interval time.Duration) *barStore {
	if capacity < 1 {
		capacity = 1
	}
	if interval <= 0 {
		interval = time.Minute
	}
	return &barStore{
		capacity: capacity,
		interval: interval,
		rings:    make(map[string]*ring),
		builders: make(map[string]*barBuilder),
	}
}

// add registers a symbol (idempotent), creating its ring and bar builder.
func (s *barStore) add(symbol string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rings[symbol]; !ok {
		s.rings[symbol] = newRing(s.capacity)
	}
	if _, ok := s.builders[symbol]; !ok {
		s.builders[symbol] = newBarBuilder(symbol, s.interval)
	}
}

// remove drops all bar state for a symbol.
func (s *barStore) remove(symbol string) {
	s.mu.Lock()
	delete(s.rings, symbol)
	delete(s.builders, symbol)
	s.mu.Unlock()
}

// has reports whether the symbol is registered.
func (s *barStore) has(symbol string) bool {
	s.mu.RLock()
	_, ok := s.rings[symbol]
	s.mu.RUnlock()
	return ok
}

// ingest applies a tick to the symbol's bar builder, pushing a completed bar
// into the ring when the tick rolls the bar over. It reports whether a bar
// completed. Unknown symbols are ignored.
func (s *barStore) ingest(symbol string, price float64, ts time.Time) (completed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.builders[symbol]
	if !ok {
		return false
	}
	if done, ready := b.add(price, ts); ready {
		if r, ok := s.rings[symbol]; ok {
			r.push(done)
		}
		return true
	}
	return false
}

// seed replaces a symbol's completed-bar history from warmup candles, keeping
// at most the ring capacity (the most recent bars).
func (s *barStore) seed(symbol string, candles []market.Candle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rings[symbol]
	if !ok {
		r = newRing(s.capacity)
		s.rings[symbol] = r
	}
	r.reset(candles)
}

// series returns a symbol's completed bars plus its in-progress bar, in
// chronological order, as a fresh slice safe to hand to the strategy.
func (s *barStore) series(symbol string) []market.Candle {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.rings[symbol]
	if !ok {
		return nil
	}
	out := make([]market.Candle, 0, r.len()+1)
	out = r.appendTo(out)
	if b, ok := s.builders[symbol]; ok {
		if cur, has := b.current(); has {
			out = append(out, cur)
		}
	}
	return out
}

// ring is a fixed-capacity circular buffer of candles. Once full, pushing
// overwrites the oldest entry — O(1) and allocation-free.
type ring struct {
	buf  []market.Candle
	head int // index of the next write
	n    int // number of valid entries (<= cap)
}

func newRing(capacity int) *ring {
	if capacity < 1 {
		capacity = 1
	}
	return &ring{buf: make([]market.Candle, capacity)}
}

func (r *ring) push(c market.Candle) {
	r.buf[r.head] = c
	r.head = (r.head + 1) % len(r.buf)
	if r.n < len(r.buf) {
		r.n++
	}
}

func (r *ring) len() int { return r.n }

// appendTo appends the ring's entries in chronological (oldest→newest) order to
// dst and returns the extended slice.
func (r *ring) appendTo(dst []market.Candle) []market.Candle {
	if r.n == 0 {
		return dst
	}
	start := (r.head - r.n + len(r.buf)) % len(r.buf)
	for i := 0; i < r.n; i++ {
		dst = append(dst, r.buf[(start+i)%len(r.buf)])
	}
	return dst
}

// reset refills the ring from candles, retaining only the most recent capacity.
func (r *ring) reset(candles []market.Candle) {
	if len(candles) > len(r.buf) {
		candles = candles[len(candles)-len(r.buf):]
	}
	r.head, r.n = 0, 0
	for _, c := range candles {
		r.push(c)
	}
}
