// Package events keeps a short log of what Honey has done that is worth
// knowing about - purges, warming the cache, backend errors and the like -
// for the admin page.
package events

import (
	"sync"
	"time"
)

// Kind says what sort of event an Event is
type Kind string

const (
	// Info is something which happened, e.g. Honey starting
	Info Kind = "info"
	// Purge is a purge of the cache
	Purge Kind = "purge"
	// Warm is the cache being warmed
	Warm Kind = "warm"
	// Notice is something which may need looking into, e.g. pages which
	// aren't cached
	Notice Kind = "notice"
	// Error is something going wrong, e.g. the backend failing
	Error Kind = "error"
	// Refused is requests being turned away
	Refused Kind = "refused"
)

// Event is something which happened, Count times (the last of them at
// Time)
type Event struct {
	Time  time.Time
	Kind  Kind
	Text  string
	Count int

	key  string
	data any
}

// Data returns what the update function passed to Add last stored with
// SetData, so that it can build on it (e.g. a list of the paths purged).
func (e *Event) Data() any { return e.data }

// SetData stores data with e, for the next update to it.
func (e *Event) SetData(data any) { e.data = data }

// DefaultMax is how many events a Log keeps by default
const DefaultMax = 100

// Log keeps the most recent events.  Its methods are safe to call
// concurrently, and on a nil Log (which does nothing).
type Log struct {
	mu     sync.Mutex
	max    int
	events []*Event // oldest first
	now    func() time.Time
}

// New returns a Log which keeps the latest max events (DefaultMax, if max
// isn't positive).
func New(max int) *Log {
	if max <= 0 {
		max = DefaultMax
	}
	return &Log{max: max, now: time.Now}
}

// Add records an event of kind, which update sets the Text (and any data)
// of.  If an event with the same (non-empty) key happened within window,
// that one is updated instead - its Count goes up, and it becomes the
// latest - so that e.g. a stream of the same error is one event, not
// the whole log.
func (l *Log) Add(kind Kind, key string, window time.Duration, update func(e *Event)) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	var e *Event
	if key != "" {
		for i := len(l.events) - 1; i >= 0; i-- {
			if l.events[i].key == key {
				if now.Sub(l.events[i].Time) <= window {
					e = l.events[i]
					l.events = append(l.events[:i], l.events[i+1:]...)
				}
				break
			}
		}
	}
	if e == nil {
		e = &Event{Kind: kind, key: key}
	}
	e.Time = now
	e.Count++
	update(e)
	l.events = append(l.events, e)
	if len(l.events) > l.max {
		l.events = l.events[len(l.events)-l.max:]
	}
}

// Recent returns up to n of the latest events, newest first.
func (l *Log) Recent(n int) []Event {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if n > len(l.events) {
		n = len(l.events)
	}
	recent := make([]Event, 0, n)
	for i := len(l.events) - 1; i >= 0 && len(recent) < n; i-- {
		e := *l.events[i]
		e.data = nil
		recent = append(recent, e)
	}
	return recent
}

// Text returns an update function for Add which sets the event's text.
func Text(text string) func(*Event) {
	return func(e *Event) { e.Text = text }
}
