// Package metrics counts what Honey does, and serves the counts in the
// Prometheus text format (https://prometheus.io/docs/instrumenting/exposition_formats/).
package metrics

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// cacheResults maps the X-Honey-Cache header to the "cache" label of
// honey_requests_total
var cacheResults = map[string]string{
	"HIT":                "hit",
	"MISS":               "miss",
	"MISS (MULTIPLEXED)": "multiplexed",
	"STALE":              "stale",
	"NO-CACHE":           "bypass",
}

// backendBuckets are the upper bounds, in seconds, of the backend latency
// histogram's buckets
var backendBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// Recorder counts requests and backend requests.  Its methods are safe to
// call concurrently.
type Recorder struct {
	stats    func() (entries int, bytes int64)
	maxBytes int64

	mu        sync.Mutex
	requests  map[string]uint64 // by cache label
	responses map[string]uint64 // by status class
	backend   map[string]uint64 // by status class, or "error"
	buckets   []uint64          // backend latency histogram, not cumulative
	latency   time.Duration     // total backend latency
	inFlight  atomic.Int64
}

// New returns a Recorder which reports the cache's size using stats (e.g.
// a cacher's Stats method), and its size limit as maxBytes.
func New(stats func() (entries int, bytes int64), maxBytes int64) *Recorder {
	return &Recorder{
		stats:     stats,
		maxBytes:  maxBytes,
		requests:  map[string]uint64{},
		responses: map[string]uint64{},
		backend:   map[string]uint64{},
		buckets:   make([]uint64, len(backendBuckets)+1),
	}
}

func statusClass(statusCode int) string {
	if statusCode < 100 || statusCode > 599 {
		return "other"
	}
	return fmt.Sprintf("%dxx", statusCode/100)
}

// Middleware counts each response next writes, by how the cache handled
// it (its X-Honey-Cache header) and its status.
func (m *Recorder) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &responseWriter{ResponseWriter: w}
		next.ServeHTTP(rw, r)
		if !rw.wroteHeader {
			// net/http sends a 200 if nothing was written
			rw.record(http.StatusOK)
		}
		result, found := cacheResults[rw.cache]
		if !found {
			result = "none"
		}
		m.mu.Lock()
		m.requests[result]++
		m.responses[statusClass(rw.status)]++
		m.mu.Unlock()
	})
}

type responseWriter struct {
	http.ResponseWriter
	wroteHeader bool
	status      int
	cache       string
}

func (w *responseWriter) record(status int) {
	w.wroteHeader = true
	w.status = status
	w.cache = w.Header().Get("X-Honey-Cache")
}

func (w *responseWriter) WriteHeader(status int) {
	if !w.wroteHeader {
		w.record(status)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.record(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Flush lets streamed responses (e.g. large files) be flushed through
func (w *responseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack lets the proxy take over the connection, e.g. for websockets
func (w *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("metrics: the ResponseWriter can't be hijacked")
	}
	if !w.wroteHeader {
		w.record(http.StatusSwitchingProtocols)
	}
	return h.Hijack()
}

// Unwrap lets http.ResponseController reach the underlying ResponseWriter
func (w *responseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// Transport counts the requests sent to the backend through next, by
// their status (or "error"), and how long the backend took to respond.
func (m *Recorder) Transport(next http.RoundTripper) http.RoundTripper {
	return roundTripper(func(r *http.Request) (*http.Response, error) {
		m.inFlight.Add(1)
		defer m.inFlight.Add(-1)
		start := time.Now()
		resp, err := next.RoundTrip(r)
		elapsed := time.Since(start)
		class := "error"
		if err == nil {
			class = statusClass(resp.StatusCode)
		}
		bucket := sort.SearchFloat64s(backendBuckets, elapsed.Seconds())
		m.mu.Lock()
		m.backend[class]++
		m.buckets[bucket]++
		m.latency += elapsed
		m.mu.Unlock()
		return resp, err
	})
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// ServeHTTP writes the metrics in the Prometheus text format.
func (m *Recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	m.mu.Lock()
	counter(&b, "honey_requests_total", "Requests served, by how the cache handled them.", "cache", m.requests,
		"hit", "miss", "multiplexed", "stale", "bypass", "none")
	counter(&b, "honey_responses_total", "Responses sent, by status code class.", "code", m.responses,
		"1xx", "2xx", "3xx", "4xx", "5xx")
	counter(&b, "honey_backend_requests_total", "Requests sent to the backend, by status code class, or error if it didn't respond.", "code", m.backend,
		"2xx", "3xx", "4xx", "5xx", "error")
	b.WriteString("# HELP honey_backend_response_seconds How long the backend took to start responding.\n")
	b.WriteString("# TYPE honey_backend_response_seconds histogram\n")
	var cumulative uint64
	for i, bound := range backendBuckets {
		cumulative += m.buckets[i]
		fmt.Fprintf(&b, "honey_backend_response_seconds_bucket{le=\"%g\"} %d\n", bound, cumulative)
	}
	cumulative += m.buckets[len(backendBuckets)]
	fmt.Fprintf(&b, "honey_backend_response_seconds_bucket{le=\"+Inf\"} %d\n", cumulative)
	fmt.Fprintf(&b, "honey_backend_response_seconds_sum %g\n", m.latency.Seconds())
	fmt.Fprintf(&b, "honey_backend_response_seconds_count %d\n", cumulative)
	m.mu.Unlock()

	gauge(&b, "honey_backend_requests_in_flight", "Requests waiting for the backend to respond.", m.inFlight.Load())
	if m.stats != nil {
		entries, bytes := m.stats()
		gauge(&b, "honey_cache_entries", "Entries in the cache.", int64(entries))
		gauge(&b, "honey_cache_bytes", "Approximate memory used by the cache.", bytes)
	}
	gauge(&b, "honey_cache_max_bytes", "The cache's size limit.", m.maxBytes)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Write([]byte(b.String()))
}

// counter writes a counter with one label, including a zero for each of
// labels which hasn't been counted, so that the series always exist.
func counter(b *strings.Builder, name, help, label string, counts map[string]uint64, labels ...string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	for _, value := range labels {
		fmt.Fprintf(b, "%s{%s=%q} %d\n", name, label, value, counts[value])
	}
	for value, count := range counts {
		if !contains(labels, value) {
			fmt.Fprintf(b, "%s{%s=%q} %d\n", name, label, value, count)
		}
	}
}

func gauge(b *strings.Builder, name, help string, value int64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, value)
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
