package multiplexer

import (
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/davidjwilkins/honey/cache"
)

type multiplexer struct {
	cacher    cache.Cacher
	requests  []request
	response  cache.Response
	done      bool
	cacheable bool
	sync.WaitGroup
	sync.RWMutex
}

type request struct {
	writer  http.ResponseWriter
	request *http.Request
}

// A Multiplexer is used to prevent flooding the remote
// server with requests when the cache is empty (e.g.
// after the cache has been cleared, or right after a
// server has come online).
//
// AddWriter should add a ResponseWriter to be written to.
//
// Write should write the response to all writers.
//
// Cacheable will return whether the response provided
// to Write was eligible to be used for writing (e.g. the
// response did not contain the Private cache-control directive)
//
// Wait should block until Write has been called and completed.
type Multiplexer interface {
	AddWriter(w http.ResponseWriter, r *http.Request)
	Write(r cache.Response) bool
	Cacheable() (bool, error)
	Wait()
}

// NewMultiplexer will create a new default multiplexer to be used for
// all requests for which cacher provides the same hash.
func NewMultiplexer(cacher cache.Cacher, r *http.Request) Multiplexer {
	return &multiplexer{
		cacher:    cacher,
		requests:  []request{},
		done:      false,
		cacheable: true,
	}
}

// AddWriter add a ResponseWriter to be written to when Write is called.
// If Write has already been called, it will call it again.
func (m *multiplexer) AddWriter(w http.ResponseWriter, r *http.Request) {
	m.Lock()
	m.requests = append(m.requests, request{w, r})
	done := m.done
	m.Add(1)
	m.Unlock()
	if done {
		m.Write(m.response)
	}
}

// Cacheable returns whether the response provided to Write was eligible to
// be used to write to all ResponseWriters.  If returns an error if Write
// has not yet been called.
func (m *multiplexer) Cacheable() (bool, error) {
	if m.response == nil {
		return m.cacheable, errors.New("No response yet")
	}
	return m.cacheable, nil
}

// Write will write the response to all ResponseWriters added
// via AddWriter.  It will return true if it was able to write
// the response (e.g. Cache-Control was not set to private or
// no-store), and true otherwise.  It will set the X-Honey-Cache
// header to MISS (MULTIPLEXED), indicating that the request was
// not in the cache, but that this response was not (initially)
// for this request.
func (m *multiplexer) Write(r cache.Response) bool {
	m.Lock()
	defer func() {
		m.response = r
		m.done = true
		m.requests = []request{}
		m.Unlock()
	}()

	if cc := r.Header().Get("Cache-Control"); strings.Contains(cc, "private") ||
		strings.Contains(cc, "no-store") || r.Header().Get("vary") == "*" {
		m.cacheable = false
		for range m.requests {
			m.Done()
		}
		return false
	}
	for _, req := range m.requests {
		go func(req request) {
			for key, values := range r.Header() {
				for _, value := range values {
					req.writer.Header().Add(key, value)
				}
			}
			req.writer.Header().Set("X-Honey-Cache", "MISS (MULTIPLEXED)")
			req.writer.Header().Set("Age", r.Age())
			if isNotModified(req.request, r) {
				req.writer.WriteHeader(http.StatusNotModified)
			} else {
				req.writer.WriteHeader(r.StatusCode())
				req.writer.Write(r.Body())
			}
			m.Done()
		}(req)
	}
	m.Wait()
	return true
}

func isNotModified(r *http.Request, resp cache.Response) bool {
	return r.Header.Get("If-None-Match") != "" &&
		r.Header.Get("If-None-Match") == resp.Header().Get("Etag")
}
