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

var (
	mu      sync.Mutex
	entries []Entry
)

// Record appends an error from the given source (e.g. "trading212").
func Record(source, message string) {
	mu.Lock()
	entries = append(entries, Entry{Time: time.Now(), Source: source, Message: message})
	if len(entries) > maxEntries {
		entries = entries[len(entries)-maxEntries:]
	}
	mu.Unlock()
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
