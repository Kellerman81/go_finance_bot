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

// maxPerSource bounds how much of the ring any single source may occupy — a
// third of it. Without this, a single flaky dependency (Yahoo
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

	recordLocked(source, message)
}

// recordLocked is Record's body. Caller holds mu.
func recordLocked(source, message string) {
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

	// When the ring itself is full, the entry to drop is the oldest one of the
	// source holding the MOST entries, not the oldest overall. Three sources
	// at their per-source cap fill all 300 slots — the normal case in a
	// network outage, when yahoo, the broker, notify, ai and ml fail together
	// — and dropping the oldest overall then evicted the one entry of a
	// source that fails rarely, the one an operator most needed (EL2-2).
	if len(entries) > maxEntries {
		dropOldestOfBusiestSource()
	}
}

// dropOldestOfBusiestSource removes the oldest entry of the source with the
// most entries. Caller holds mu.
func dropOldestOfBusiestSource() {
	counts := make(map[string]int)

	busiest := ""

	for _, e := range entries {
		counts[e.Source]++

		if counts[e.Source] > counts[busiest] {
			busiest = e.Source
		}
	}

	for i, e := range entries {
		if e.Source == busiest {
			entries = append(entries[:i], entries[i+1:]...)

			return
		}
	}
}

// RecordOnce is Record for a message that describes a standing condition —
// a config typo, say — rather than an event: it is recorded only if the ring
// does not already hold an identical (source, message) entry. Code that runs
// once per optimizer trial or settings apply otherwise fills its source's
// share with copies of one line and hides every other message (EL2-3).
//
// The scan and the append are one critical section: as two, concurrent
// callers (parallel optimizer trials building strategies, syncPortfolio from
// order goroutines and the eval loop) could both find the line absent and
// both append it (EL3-6).
func RecordOnce(source, message string) {
	mu.Lock()
	defer mu.Unlock()

	for _, e := range entries {
		if e.Source == source && e.Message == message {
			return
		}
	}

	recordLocked(source, message)
}

// RecordOncef is RecordOnce with formatting.
func RecordOncef(source, format string, args ...any) {
	RecordOnce(source, fmt.Sprintf(format, args...))
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
