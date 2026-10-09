// Package singleflight lets concurrent requests for the same resource
// share a single backend request, to avoid flooding the backend when
// the cache is empty (e.g. after the cache has been cleared, or right
// after a server has come online).
//
// One requester - the leader - fetches the resource, and finishes the
// Flight with a Result.  Everyone else waits for the Result and then
// writes their own response from it, so a requester's ResponseWriter is
// only ever used by its own goroutine, and no lock is held while anyone
// waits.
package singleflight

import (
	"sync"

	"github.com/davidjwilkins/honey/cache"
)

// Result is what the leader of a Flight tells the requests waiting on it.
// Exactly one of its fields should be set.
type Result struct {
	// Response is the response to share with the waiting requests (if it
	// suits them - e.g. its Vary headers match theirs).
	Response cache.Response
	// Stale is set if Response is a stale response, served because the
	// backend errored.
	Stale bool
	// Bypass means the response can't be shared (e.g. it is too large to
	// cache), and each request should fetch it from the backend itself.
	Bypass bool
	// Retry means there is no response (e.g. the leader's client went away
	// before it arrived), and the requests should try again.
	Retry bool
	// StatusCode is the error status to respond with if the backend
	// couldn't be reached and there is no response to share.
	StatusCode int
}

// A Flight is a single backend request which other requests wait for.
type Flight struct {
	done   chan struct{}
	once   sync.Once
	result Result
}

// Done returns a channel which is closed when the Flight has finished.
func (f *Flight) Done() <-chan struct{} {
	return f.done
}

// Result returns the Flight's Result.  It must only be called once Done
// is closed.
func (f *Flight) Result() Result {
	return f.result
}

// A Group holds the Flights in progress, by key.  The zero Group is ready
// to use.
type Group struct {
	mu      sync.Mutex
	flights map[string]*Flight
}

// Join returns the Flight in progress for key, or else starts one.  If it
// starts one, leader is true, and the caller must Finish it.
func (g *Group) Join(key string) (f *Flight, leader bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if f, found := g.flights[key]; found {
		return f, false
	}
	if g.flights == nil {
		g.flights = make(map[string]*Flight)
	}
	f = &Flight{done: make(chan struct{})}
	g.flights[key] = f
	return f, true
}

// InFlight returns whether there is a Flight in progress for key.
func (g *Group) InFlight(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, found := g.flights[key]
	return found
}

// Finish ends Flight f, which was started for key, releasing everyone
// waiting on it with result.  Requests which Join key afterwards start a
// new Flight.  Only the first call to Finish for a Flight has any effect,
// so a leader can safely Finish its Flight again as a fallback.
func (g *Group) Finish(key string, f *Flight, result Result) {
	f.once.Do(func() {
		g.mu.Lock()
		// Only remove f: once it has been removed, a new Flight may have
		// been started for the same key.
		if g.flights[key] == f {
			delete(g.flights, key)
		}
		g.mu.Unlock()
		f.result = result
		close(f.done)
	})
}
