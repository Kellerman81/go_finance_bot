package engine

import (
	"time"

	"github.com/Kellerman81/go_finance_bot/internal/strategy"
)

// EventType classifies an engine event.
type EventType string

const (
	EventSignal EventType = "signal" // a symbol's signal action changed
	EventTrade  EventType = "trade"  // a trade record was produced
)

// Event is a push notification of engine activity, delivered to subscribers.
type Event struct {
	Type   EventType        `json:"type"`
	Time   time.Time        `json:"time"`
	Symbol string           `json:"symbol,omitempty"`
	Signal *strategy.Signal `json:"signal,omitempty"`
	Trade  *TradeRecord     `json:"trade,omitempty"`
}

// Subscribe registers a listener for engine events and returns its channel plus
// a cancel func that unsubscribes and closes the channel. Events are dropped for
// a subscriber whose buffer is full, so a slow consumer never blocks the engine.
// buffer < 1 defaults to 16.
func (e *Engine) Subscribe(buffer int) (<-chan Event, func()) {
	if buffer < 1 {
		buffer = 16
	}
	ch := make(chan Event, buffer)
	e.subsMu.Lock()
	if e.subs == nil {
		e.subs = make(map[int]chan Event)
	}
	id := e.nextSubID
	e.nextSubID++
	e.subs[id] = ch
	e.subsMu.Unlock()

	return ch, func() {
		e.subsMu.Lock()
		if c, ok := e.subs[id]; ok {
			delete(e.subs, id)
			close(c)
		}
		e.subsMu.Unlock()
	}
}

// emit delivers ev to every subscriber, dropping it for any whose buffer is full
// (non-blocking) so publishing never stalls the engine.
func (e *Engine) emit(ev Event) {
	e.subsMu.Lock()
	for _, ch := range e.subs {
		select {
		case ch <- ev:
		default: // slow consumer — drop rather than block
		}
	}
	e.subsMu.Unlock()
}
