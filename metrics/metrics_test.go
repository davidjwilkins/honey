package metrics

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
