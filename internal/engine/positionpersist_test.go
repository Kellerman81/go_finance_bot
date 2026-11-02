package engine

import (
	"sync"
	"testing"
	"time"
)

// rowStore keeps open-position rows like the real table, and can hold the
// first save open so a test can interleave a second writer with it.
type rowStore struct {
	nopStore

	mu      sync.Mutex
	rows    map[string]OpenPosition
	entered chan struct{} // receives once the first save is in progress
	release chan struct{} // the first save completes when this closes
	once    sync.Once
}

func (s *rowStore) SaveOpenPosition(p OpenPosition) error {
	s.once.Do(func() {
		s.entered <- struct{}{}
		<-s.release
	})

	s.mu.Lock()
	defer s.mu.Unlock()

	s.rows[p.Symbol] = p

	return nil
}

func (s *rowStore) DeleteOpenPosition(symbol string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.rows, symbol)

	return nil
}

// SG2-3: writers snapshotted the record under e.mu and wrote the snapshot
// after unlocking. A scale-out's progress save that lost the race to the
// selling fill's delete re-inserted the closed position, and the next start
// restored a phantom holding for stops and max_hold to act on. Whatever the
// order the writes land in, the stored row must match memory.
func TestASlowSaveCannotResurrectAClosedPosition(t *testing.T) {
	e, _ := forgetEngine()
	st := &rowStore{
		rows: map[string]OpenPosition{}, entered: make(chan struct{}), release: make(chan struct{}),
	}
	e.store = st
	e.openPos["X"] = &OpenPosition{Symbol: "X", Qty: 10, EntryPrice: 100}

	saved := make(chan struct{})

	go func() {
		e.markPartialTaken("X", 0) // the save that will be slow
		close(saved)
	}()

	<-st.entered

	closed := make(chan struct{})

	go func() {
		e.recordClose("X", 10) // the fill that closes X, while that save is in flight
		close(closed)
	}()

	// Give the close every chance to land first; under the fix it waits for
	// the in-flight persist instead, which is the point.
	select {
	case <-closed:
	case <-time.After(200 * time.Millisecond):
	}

	close(st.release)
	<-saved
	<-closed

	st.mu.Lock()
	defer st.mu.Unlock()

	if p, ok := st.rows["X"]; ok {
		t.Errorf("a closed position is back in the store (qty %v); a restart would restore it", p.Qty)
	}
}
