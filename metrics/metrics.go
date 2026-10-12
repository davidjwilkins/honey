// Package metrics counts what Honey does, and serves the counts in the
// Prometheus text format (https://prometheus.io/docs/instrumenting/exposition_formats/).
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/davidjwilkins/honey/events"
	"github.com/davidjwilkins/honey/internal/capture"
)

// cacheResults maps the X-Honey-Cache header to the "cache" label of
// honey_requests_total
var cacheResults = map[string]string{
	"HIT":                "hit",
	"MISS":               "miss",
	"MISS (MULTIPLEXED)": "multiplexed",
	"STALE":              "stale",
	"NO-CACHE":           "bypass",
	"BLOCKED":            "blocked",
}

// backendBuckets are the upper bounds, in seconds, of the backend latency
// histogram's buckets
var backendBuckets = []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// BackendBuckets returns the upper bounds of the backend latency
// histogram's buckets (see Snapshot.BackendBuckets): the last bucket has
// no upper bound.
func BackendBuckets() []time.Duration {
	bounds := make([]time.Duration, len(backendBuckets))
	for i, bound := range backendBuckets {
		bounds[i] = time.Duration(bound * float64(time.Second))
	}
	return bounds
}

// historyMinutes is how many minutes of history Snapshot.History has
const historyMinutes = 60

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
	// minutes counts requests by minute, for the last historyMinutes
	// minutes: minute i is at minutes[i%historyMinutes]
	minutes   [historyMinutes]Minute
	lastError *BackendError
	events    *events.Log
	now       func() time.Time
}

// Minute is the requests served in a minute, and how many of them came
// from the cache
type Minute struct {
	Start     time.Time
	Requests  uint64
	FromCache uint64
}

// BackendError is a request which the backend failed
type BackendError struct {
	Time time.Time
	Path string
	// Status is the status code the backend responded with, or 0 if it
	// didn't (see Err)
	Status int
	Err    string
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
		now:       time.Now,
	}
}

// LogTo makes the Recorder log backend failures to l.
func (m *Recorder) LogTo(l *events.Log) {
	m.events = l
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
		rw := capture.NewWriter(w)
		next.ServeHTTP(rw, r)
		result, found := cacheResults[rw.Cache()]
		if !found {
			result = "none"
		}
		start := m.now().Truncate(time.Minute)
		m.mu.Lock()
		m.requests[result]++
		m.responses[statusClass(rw.Status())]++
		minute := &m.minutes[start.Unix()/60%historyMinutes]
		if !minute.Start.Equal(start) {
			*minute = Minute{Start: start}
		}
		minute.Requests++
		if fromCache[result] {
			minute.FromCache++
		}
		m.mu.Unlock()
	})
}

// fromCache are the requests which were served from the cache
var fromCache = map[string]bool{"hit": true, "multiplexed": true, "stale": true}

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
		var failed *BackendError
		if err != nil && r.Context().Err() == nil {
			failed = &BackendError{Path: r.URL.Path, Err: err.Error()}
		} else if err == nil && resp.StatusCode >= 500 {
			failed = &BackendError{Path: r.URL.Path, Status: resp.StatusCode}
		}
		bucket := sort.SearchFloat64s(backendBuckets, elapsed.Seconds())
		m.mu.Lock()
		m.backend[class]++
		m.buckets[bucket]++
		m.latency += elapsed
		if failed != nil {
			failed.Time = m.now()
			m.lastError = failed
		}
		m.mu.Unlock()
		if failed != nil {
			m.logError(failed)
		}
		return resp, err
	})
}

// logError logs a backend failure, combining repeats within a few
// minutes into one event (so that a failing backend doesn't fill the log)
func (m *Recorder) logError(failed *BackendError) {
	key, what := "backend error", "the backend couldn't be reached"
	if failed.Status != 0 {
		key = fmt.Sprintf("backend %d", failed.Status)
		what = fmt.Sprintf("the backend responded %d", failed.Status)
	}
	m.events.Add(events.Error, key, 10*time.Minute, func(e *events.Event) {
		e.Text = fmt.Sprintf("%s, most recently for %s", what, failed.Path)
	})
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// Snapshot is the counts so far
type Snapshot struct {
	// Requests are the requests served, by how the cache handled them:
	// hit, miss, multiplexed, stale, bypass or none
	Requests map[string]uint64
	// Responses are the responses sent, by status class (e.g. 2xx)
	Responses map[string]uint64
	// Backend are the requests sent to the backend, by status class, or
	// "error" if it didn't respond
	Backend map[string]uint64
	// BackendLatency is how long the backend took to start responding to
	// them, in total
	BackendLatency  time.Duration
	BackendInFlight int64
	// BackendBuckets is the backend latency histogram: how many backend
	// requests took up to each of BackendBuckets() (and the rest, in the
	// last bucket)
	BackendBuckets []uint64
	// LastBackendError is the last request the backend failed, if any
	LastBackendError *BackendError
	// History is the requests in each of the last 60 minutes, oldest first
	// and ending with the current minute (so far)
	History []Minute
	// Entries and Bytes are how full the cache is, and MaxBytes its limit
	Entries  int
	Bytes    int64
	MaxBytes int64
}

// Snapshot returns the counts so far.
func (m *Recorder) Snapshot() Snapshot {
	s := Snapshot{
		Requests:        map[string]uint64{},
		Responses:       map[string]uint64{},
		Backend:         map[string]uint64{},
		BackendInFlight: m.inFlight.Load(),
		MaxBytes:        m.maxBytes,
	}
	m.mu.Lock()
	for k, v := range m.requests {
		s.Requests[k] = v
	}
	for k, v := range m.responses {
		s.Responses[k] = v
	}
	for k, v := range m.backend {
		s.Backend[k] = v
	}
	s.BackendLatency = m.latency
	s.BackendBuckets = append([]uint64(nil), m.buckets...)
	if m.lastError != nil {
		lastError := *m.lastError
		s.LastBackendError = &lastError
	}
	current := m.now().Truncate(time.Minute)
	for i := historyMinutes - 1; i >= 0; i-- {
		start := current.Add(-time.Duration(i) * time.Minute)
		minute := m.minutes[start.Unix()/60%historyMinutes]
		if !minute.Start.Equal(start) {
			minute = Minute{Start: start}
		}
		s.History = append(s.History, minute)
	}
	m.mu.Unlock()
	if m.stats != nil {
		s.Entries, s.Bytes = m.stats()
	}
	return s
}

// ServeHTTP writes the metrics in the Prometheus text format.
func (m *Recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	m.mu.Lock()
	counter(&b, "honey_requests_total", "Requests served, by how the cache handled them.", "cache", m.requests,
		"hit", "miss", "multiplexed", "stale", "bypass", "blocked", "none")
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

// BackendQuantile estimates the q quantile (e.g. 0.5 for the median) of
// how long the backend took to start responding, from the histogram, as
// Prometheus's histogram_quantile does: by assuming the requests in each
// bucket are spread evenly through it.  If it falls in the last bucket,
// which has no upper bound, it returns that bucket's lower bound, and
// over.  It returns 0 if there have been no backend requests.
func (s Snapshot) BackendQuantile(q float64) (d time.Duration, over bool) {
	var total uint64
	for _, count := range s.BackendBuckets {
		total += count
	}
	if total == 0 {
		return 0, false
	}
	bounds := BackendBuckets()
	rank := q * float64(total)
	var below uint64
	for i, count := range s.BackendBuckets {
		if count == 0 || float64(below+count) < rank {
			below += count
			continue
		}
		if i >= len(bounds) {
			return bounds[len(bounds)-1], true
		}
		var lower time.Duration
		if i > 0 {
			lower = bounds[i-1]
		}
		fraction := (rank - float64(below)) / float64(count)
		return lower + time.Duration(fraction*float64(bounds[i]-lower)), false
	}
	return bounds[len(bounds)-1], true
}
