package metrics

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func scrape(t *testing.T, m *Recorder) string {
	t.Helper()
	w := httptest.NewRecorder()
	m.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Contains(t, w.Header().Get("Content-Type"), "text/plain")
	return w.Body.String()
}

func TestMiddlewareCountsCacheResults(t *testing.T) {
	m := New(func() (int, int64) { return 3, 4096 }, 1<<20)
	handler := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/hit":
			w.Header().Set("X-Honey-Cache", "HIT")
			io.WriteString(w, "body")
		case "/multiplexed":
			w.Header().Set("X-Honey-Cache", "MISS (MULTIPLEXED)")
			w.WriteHeader(http.StatusNotModified)
		case "/error":
			http.Error(w, "down", http.StatusBadGateway)
		case "/empty":
		}
	}))
	for _, path := range []string{"/hit", "/hit", "/multiplexed", "/error", "/empty"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	out := scrape(t, m)
	for _, line := range []string{
		`honey_requests_total{cache="hit"} 2`,
		`honey_requests_total{cache="multiplexed"} 1`,
		`honey_requests_total{cache="none"} 2`,
		`honey_requests_total{cache="miss"} 0`,
		`honey_responses_total{code="2xx"} 3`,
		`honey_responses_total{code="3xx"} 1`,
		`honey_responses_total{code="5xx"} 1`,
		"honey_cache_entries 3",
		"honey_cache_bytes 4096",
		"honey_cache_max_bytes 1048576",
		"# TYPE honey_requests_total counter",
		"# TYPE honey_cache_bytes gauge",
	} {
		assert.Contains(t, out, line+"\n")
	}
}

func TestMiddlewareKeepsFlushing(t *testing.T) {
	m := New(nil, 0)
	var flushable bool
	handler := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, flushable = w.(http.Flusher)
		require.NoError(t, http.NewResponseController(w).Flush())
	}))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.True(t, flushable, "streamed responses must still be flushable")
	assert.True(t, w.Flushed)
}

func TestTransportCountsBackendRequests(t *testing.T) {
	m := New(nil, 0)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			time.Sleep(120 * time.Millisecond)
		}
		if r.URL.Path == "/missing" {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer origin.Close()
	client := &http.Client{Transport: m.Transport(http.DefaultTransport)}
	for _, path := range []string{"/", "/slow", "/missing"} {
		resp, err := client.Get(origin.URL + path)
		require.NoError(t, err)
		resp.Body.Close()
	}
	failing := m.Transport(roundTripper(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	}))
	_, err := failing.RoundTrip(httptest.NewRequest(http.MethodGet, "http://backend/", nil))
	assert.Error(t, err)

	out := scrape(t, m)
	for _, line := range []string{
		`honey_backend_requests_total{code="2xx"} 2`,
		`honey_backend_requests_total{code="4xx"} 1`,
		`honey_backend_requests_total{code="error"} 1`,
		`honey_backend_response_seconds_bucket{le="+Inf"} 4`,
		`honey_backend_response_seconds_count 4`,
		"honey_backend_requests_in_flight 0",
	} {
		assert.Contains(t, out, line+"\n")
	}
	assert.Contains(t, out, `honey_backend_response_seconds_bucket{le="0.05"} 3`+"\n", "only /slow took longer than 50ms")
	assert.Contains(t, out, `honey_backend_response_seconds_bucket{le="0.25"} 4`+"\n")
	assert.False(t, strings.Contains(out, "honey_cache_entries"), "cache stats are only reported if there are any")
}

func TestSnapshot(t *testing.T) {
	m := New(func() (int, int64) { return 3, 1024 }, 4096)
	handler := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Honey-Cache", r.URL.Query().Get("cache"))
		w.WriteHeader(http.StatusOK)
	}))
	for _, result := range []string{"HIT", "HIT", "MISS", ""} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/?cache="+url.QueryEscape(result), nil))
	}
	s := m.Snapshot()
	assert.Equal(t, map[string]uint64{"hit": 2, "miss": 1, "none": 1}, s.Requests)
	assert.Equal(t, map[string]uint64{"2xx": 4}, s.Responses)
	assert.Equal(t, 3, s.Entries)
	assert.Equal(t, int64(1024), s.Bytes)
	assert.Equal(t, int64(4096), s.MaxBytes)
	s.Requests["hit"] = 100
	assert.Equal(t, uint64(2), m.Snapshot().Requests["hit"], "it's a copy")
}

func TestHistory(t *testing.T) {
	m := New(nil, 0)
	now := time.Date(2026, 10, 12, 21, 30, 15, 0, time.UTC)
	m.now = func() time.Time { return now }
	handler := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Honey-Cache", r.URL.Query().Get("cache"))
	}))
	serve := func(result string) {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/?cache="+url.QueryEscape(result), nil))
	}
	serve("HIT")
	serve("MISS")
	now = now.Add(2 * time.Minute)
	serve("STALE")
	serve("MISS (MULTIPLEXED)")
	serve("NO-CACHE")

	history := m.Snapshot().History
	require.Len(t, history, 60)
	last := history[59]
	assert.Equal(t, now.Truncate(time.Minute), last.Start)
	assert.Equal(t, Minute{Start: last.Start, Requests: 3, FromCache: 2}, last)
	assert.Zero(t, history[58].Requests, "nothing happened in the minute between")
	assert.Equal(t, uint64(2), history[57].Requests)
	assert.Equal(t, uint64(1), history[57].FromCache)

	now = now.Add(time.Hour)
	for _, minute := range m.Snapshot().History {
		assert.Zero(t, minute.Requests, "an hour later, it's all gone")
	}
}

func TestBackendErrors(t *testing.T) {
	m := New(nil, 0)
	log := events.New(10)
	m.LogTo(log)
	ok := m.Transport(roundTripper(func(r *http.Request) (*http.Response, error) {
		status := http.StatusOK
		if r.URL.Path == "/shop/" {
			status = http.StatusBadGateway
		}
		return &http.Response{StatusCode: status, Body: http.NoBody}, nil
	}))
	for _, path := range []string{"/", "/shop/", "/shop/", "/"} {
		_, err := ok.RoundTrip(httptest.NewRequest(http.MethodGet, "http://backend"+path, nil))
		require.NoError(t, err)
	}
	s := m.Snapshot()
	require.NotNil(t, s.LastBackendError)
	assert.Equal(t, "/shop/", s.LastBackendError.Path)
	assert.Equal(t, http.StatusBadGateway, s.LastBackendError.Status)

	failing := m.Transport(roundTripper(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	}))
	failing.RoundTrip(httptest.NewRequest(http.MethodGet, "http://backend/about/", nil))
	assert.Equal(t, "connection refused", m.Snapshot().LastBackendError.Err)

	recent := log.Recent(10)
	require.Len(t, recent, 2)
	assert.Equal(t, "the backend couldn't be reached, most recently for /about/", recent[0].Text)
	assert.Equal(t, "the backend responded 502, most recently for /shop/", recent[1].Text)
	assert.Equal(t, 2, recent[1].Count, "repeats are one event")
}

func TestBackendQuantile(t *testing.T) {
	var s Snapshot
	_, over := s.BackendQuantile(0.5)
	assert.False(t, over)

	// 10ms, 25ms, 50ms, 100ms, 250ms... buckets
	s.BackendBuckets = make([]uint64, len(backendBuckets)+1)
	s.BackendBuckets[3] = 100 // 50-100ms
	s.BackendBuckets[4] = 100 // 100-250ms
	d, over := s.BackendQuantile(0.5)
	assert.False(t, over)
	assert.Equal(t, 100*time.Millisecond, d)
	d, _ = s.BackendQuantile(0.75)
	assert.Equal(t, 175*time.Millisecond, d)
	d, _ = s.BackendQuantile(0.25)
	assert.Equal(t, 75*time.Millisecond, d)

	s.BackendBuckets[len(backendBuckets)] = 300 // over 30s
	d, over = s.BackendQuantile(0.99)
	assert.True(t, over)
	assert.Equal(t, 30*time.Second, d)
}
