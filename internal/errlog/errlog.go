// Package errlog is a small process-wide ring buffer of errors from outbound API
// calls (Finnhub, Saxo, Alpaca, Yahoo, Trading 212, …), surfaced in the web UI
// so the user can see what's failing without reading server logs.
package errlog

import (
	"fmt"
	"sync"
	"time"
)

// Entry is one recorded error.
type Entry struct {
	Time    time.Time `json:"time"`
	Source  string    `json:"source"`
	Message string    `json:"message"`
}

const maxEntries = 300

// maxPerSource bounds how much of the ring any single source may occupy —
// less than a third of it. Without this, a single flaky dependency (Yahoo
// hammering 429s, an FX lookup down, several notify sinks failing at once)
// could fill the entire 300-entry ring with its own errors and evict every
// other source's entries first, before the global cap even had a chance to
// bind — including sources that only fail rarely and whose one error is the
// one an operator actually needed to see. See CODE_REVIEW_FINDINGS.md's
// errlog-per-source-cap finding.
const maxPerSource = 100

var (
	mu      sync.Mutex
	entries []Entry
)

// Record appends an error from the given source (e.g. "trading212").
func Record(source, message string) {
	mu.Lock()
	defer mu.Unlock()

	entries = append(entries, Entry{Time: time.Now(), Source: source, Message: message})

	// Drop this source's own oldest entry once it exceeds its per-source
	// share, so it never crowds out other sources' entries — see
	// maxPerSource.
	count, oldestIdx := 0, -1

	for i, e := range entries {
		if e.Source == source {
			count++

			if oldestIdx == -1 {
				oldestIdx = i
			}
		}
	}

	if count > maxPerSource && oldestIdx >= 0 {
		entries = append(entries[:oldestIdx], entries[oldestIdx+1:]...)
	}

	if len(entries) > maxEntries {
		entries = entries[len(entries)-maxEntries:]
	}
}

// Recordf is Record with formatting.
func Recordf(source, format string, args ...any) {
	Record(source, fmt.Sprintf(format, args...))
}

// Recent returns recorded errors, newest first.
func Recent() []Entry {
	mu.Lock()
	defer mu.Unlock()

	out := make([]Entry, len(entries))
	for i, e := range entries {
		out[len(entries)-1-i] = e
	}

	return out
}

// Clear empties the buffer.
func Clear() {
	mu.Lock()

	entries = nil
	mu.Unlock()
}
