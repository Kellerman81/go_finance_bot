package market

import (
	"testing"
	"time"
)

// TestReplayHistoryOnlyClosesQuotesOnClose reproduces the "history-only mode
// never starts its producer goroutine, so nothing closes the output channel
// — a real hang if used that way" finding: with interval == 0 (history-only,
// no streaming), Close() must still close the Quotes() channel so a caller
// idiomatically ranging over it (the standard way to drain a channel until
// its producer is done) does not block forever.
func TestReplayHistoryOnlyClosesQuotesOnClose(t *testing.T) {
	r := NewReplay(nil, 0) // interval 0: history-only mode

	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case _, open := <-r.Quotes():
		if open {
			t.Fatal("Quotes() delivered a value instead of being closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Quotes() was never closed after Close() — a range over it would hang forever")
	}
}

// TestReplayStreamingModeStillClosesQuotesOnClose confirms the fix doesn't
// regress the existing streaming (interval > 0) path.
func TestReplayStreamingModeStillClosesQuotesOnClose(t *testing.T) {
	r := NewReplay(nil, 5*time.Millisecond)

	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case _, open := <-r.Quotes():
		if open {
			t.Fatal("Quotes() delivered a value instead of being closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Quotes() was never closed after Close()")
	}
}
