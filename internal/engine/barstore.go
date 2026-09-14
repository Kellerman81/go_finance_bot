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

// newBarStore creates a store of per-symbol candle rings and bar builders.
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

// ingest applies a tick to the symbol's bar builder, pushing any bars it
// completed into the ring. It reports whether at least one bar completed.
// Unknown symbols are ignored.
//
// More than one bar can complete at once: a quiet stretch is back-filled with
// flat bars so the series stays evenly spaced in time. See barBuilder.add.
func (s *barStore) ingest(symbol string, price, volume float64, ts time.Time) (completed bool) {
	if !plausibleTickTime(ts) {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	b, ok := s.builders[symbol]
	if !ok {
		return false
	}

	done := b.add(price, volume, ts)
	if len(done) == 0 {
		return false
	}

	if r, ok := s.rings[symbol]; ok {
		for _, c := range done {
			r.push(c)
		}
	}

	return true
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
	return s.tail(symbol, 0, nil)
}

// tail appends a symbol's most recent bars to dst and returns the extended
// slice, oldest→newest, including the in-progress bar. n <= 0 means all of them.
//
// The bounded form exists because several callers need only a short window — an
// ATR over ~40 bars, say — and taking a copy of the full ring (up to a week of
// one-minute candles) to read it is pure waste. Passing dst lets a caller in a
// loop keep one buffer instead of allocating per call.
func (s *barStore) tail(symbol string, n int, dst []market.Candle) []market.Candle {
	s.mu.RLock()
	defer s.mu.RUnlock()

	r, ok := s.rings[symbol]
	if !ok {
		return dst
	}

	var current market.Candle

	hasCurrent := false

	if b, ok := s.builders[symbol]; ok {
		current, hasCurrent = b.current()
	}

	// The in-progress bar counts toward n, so a caller asking for 40 bars gets
	// 40 bars and not 41.
	want := allEntries
	if n > 0 {
		want = n
		if hasCurrent {
			want--
		}
	}

	if cap(dst) == 0 {
		size := r.len() + 1
		if n > 0 && n < size {
			size = n
		}

		dst = make([]market.Candle, 0, size)
	}

	dst = r.appendLastTo(dst, want)

	if hasCurrent {
		dst = append(dst, current)
	}

	return dst
}

// plausibleTickTime rejects a tick whose timestamp cannot be describing a
// trade at all.
//
// Feeds do send these: Finnhub occasionally reports t=0, which
// time.UnixMilli turns into 1970-01-01. One such tick used to open a bar
// stamped 1970, and the next real tick then closed it and pushed it into the
// ring — leaving a series whose timestamps are not monotonic. Everything
// downstream assumes they are: market.Resample documents "input is assumed
// chronologically ordered", and internal/autotune anchors its real-vs-backtest
// comparison window on candles[0].Time. See CODE_REVIEW_FINDINGS.md's EN-5.
//
// The test is ts.Unix() > 0, not ts.IsZero(): a Finnhub t=0 becomes
// time.UnixMilli(0) — 1970-01-01, which is a perfectly ordinary non-zero
// time.Time and sails past IsZero. "At or before the Unix epoch" catches both
// that and Go's own zero value (year 1, a large negative Unix seconds), and no
// real trade timestamp is ever in that range.
//
// Only that is rejected, deliberately — NOT a window around the wall clock. market.Replay streams historical candles as live quotes stamped with
// their original times (see Replay.run), so any "too old" bound would refuse
// an entire replay session. Every other out-of-order case is caught where it
// can be judged without a clock: barBuilder.add drops a tick older than the
// bar currently open. The residual gap is a far-FUTURE garbage timestamp on
// the very first tick for a symbol, which has no prior bar to be compared
// against and would then pin that symbol's builder; no feed has been observed
// doing this, and catching it would need the wall-clock bound replay rules out.
func plausibleTickTime(ts time.Time) bool { return ts.Unix() > 0 }

// ring is a fixed-capacity circular buffer of candles. Once full, pushing
// overwrites the oldest entry — O(1) and allocation-free.
type ring struct {
	buf  []market.Candle
	head int // index of the next write
	n    int // number of valid entries (<= cap)
}

// newRing allocates a ring with the given capacity (minimum 1).
func newRing(capacity int) *ring {
	if capacity < 1 {
		capacity = 1
	}

	return &ring{buf: make([]market.Candle, capacity)}
}

// push appends a candle, overwriting the oldest entry when full.
func (r *ring) push(c market.Candle) {
	r.buf[r.head] = c
	r.head = (r.head + 1) % len(r.buf)

	if r.n < len(r.buf) {
		r.n++
	}
}

// len reports the number of valid entries.
func (r *ring) len() int { return r.n }

// appendTo appends the ring's entries in chronological (oldest→newest) order to
// dst and returns the extended slice.
func (r *ring) appendTo(dst []market.Candle) []market.Candle {
	return r.appendLastTo(dst, allEntries)
}

// allEntries asks appendLastTo for everything the ring holds. A negative
// sentinel is needed because 0 has to mean "none": a caller wanting exactly one
// bar, already covered by the in-progress bar, wants nothing from the ring.
const allEntries = -1

// appendLastTo appends the ring's newest n entries in chronological order.
// n < 0 appends everything; n == 0 appends nothing; n beyond what is held
// appends everything.
func (r *ring) appendLastTo(dst []market.Candle, n int) []market.Candle {
	if r.n == 0 || n == 0 {
		return dst
	}

	take := r.n
	if n > 0 && n < take {
		take = n
	}

	start := (r.head - take + len(r.buf)) % len(r.buf)
	for i := range take {
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
