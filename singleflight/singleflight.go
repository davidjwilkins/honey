package singleflight

import (
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/davidjwilkins/honey/cache"
	"github.com/davidjwilkins/honey/utilities"
)

type singleflight struct {
	cacher    cache.Cacher
	requests  []request
	response  cache.Response
	done      bool
	cacheable bool
	handler   func(w http.ResponseWriter, r *http.Request)
	sync.WaitGroup
	sync.RWMutex
}

type request struct {
	writer  http.ResponseWriter
	request *http.Request
}

// A Singleflight is used to prevent flooding the remote
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
type Singleflight interface {
	AddWriter(w http.ResponseWriter, r *http.Request)
	Write(r cache.Response) bool
	Cacheable() (bool, error)
	Wait()
}

// NewSingleflight will create a new default singleflight to be used for
// all requests for which cacher provides the same hash.
func NewSingleflight(cacher cache.Cacher, r *http.Request, handler func(w http.ResponseWriter, r *http.Request)) Singleflight {
	return &singleflight{
		cacher:    cacher,
		requests:  []request{},
		done:      false,
		cacheable: true,
		handler:   handler,
	}
}

// AddWriter add a ResponseWriter to be written to when Write is called.
// If Write has already been called, it will call it again.
func (m *singleflight) AddWriter(w http.ResponseWriter, r *http.Request) {
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
func (m *singleflight) Cacheable() (bool, error) {
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
func (m *singleflight) Write(r cache.Response) bool {
	m.Lock()
	defer func() {
		m.response = r
		m.done = true
		m.requests = []request{}
		m.Unlock()
	}()
	vary := r.Header().Get("Vary")
	if cc := r.Header().Get("Cache-Control"); strings.Contains(cc, "private") ||
		strings.Contains(cc, "no-store") || vary == "*" {
		m.cacheable = false
		go func() {
			for range m.requests {
				m.Done()
			}
		}()
		m.Wait()
		return false
	}
	// Bucket the requests based on whether their headers for the response Vary are the same
	hash := utilities.GetVaryHeadersHash(r.RequestHeaders(), r, m.cacher.AllowedCookies(), vary)
	buckets := make(map[string][]request)
	for _, req := range m.requests {
		h := utilities.GetVaryHeadersHash(req.request.Header, req.request, m.cacher.AllowedCookies(), vary)
		buckets[h] = append(buckets[h], req)
	}
	// Respond to any that match the Vary
	for _, req := range buckets[hash] {
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
	go func() {
		for bucket, requests := range buckets {
			// we've already done the one with the current request's hash
			if bucket == hash {
				continue
			}
			// TODO: GET THE RESPONSE FOR EACH BUCKET
			for _, req := range requests {
				req.request.Header.Set("X-Honey-Vary", bucket)
				m.handler(req.writer, req.request)
				m.Done()
			}
		}
	}()
	m.Wait()

	return true
}

func isNotModified(r *http.Request, resp cache.Response) bool {
	return r.Header.Get("If-None-Match") != "" &&
		r.Header.Get("If-None-Match") == resp.Header().Get("Etag")
}
