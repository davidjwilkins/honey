package accesslog

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMiddlewareLogsRequests(t *testing.T) {
	var out bytes.Buffer
	handler := Middleware(New(&out, JSON), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Honey rewrites the request it's given; the log should show the original
		r.URL.Path = "/rewritten"
		r.RequestURI = "/rewritten"
		w.Header().Set("X-Honey-Cache", "HIT")
		io.WriteString(w, "hello")
	}))
	r := httptest.NewRequest(http.MethodGet, "/page?p=1", nil)
	r.RemoteAddr = "203.0.113.7:51234"
	r.Header.Set("X-Forwarded-For", "198.51.100.2")
	r.Header.Set("X-Honey-Secret", "do-not-log-this-secret")
	handler.ServeHTTP(httptest.NewRecorder(), r)

	assert.NotContains(t, out.String(), "do-not-log-this-secret", "request headers mustn't be logged")
	var line map[string]interface{}
	require.NoError(t, json.Unmarshal(out.Bytes(), &line))
	assert.Equal(t, "request", line["msg"])
	assert.Equal(t, "GET", line["method"])
	assert.Equal(t, "/page?p=1", line["uri"])
	assert.Equal(t, float64(200), line["status"])
	assert.Equal(t, float64(5), line["bytes"])
	assert.Equal(t, "HIT", line["cache"])
	assert.Equal(t, "203.0.113.7:51234", line["remote"])
	assert.Equal(t, "198.51.100.2", line["forwarded_for"])
	assert.Contains(t, line, "duration_ms")
	assert.Contains(t, line, "time")
}

func TestMiddlewareTextFormat(t *testing.T) {
	var out bytes.Buffer
	handler := Middleware(New(&out, Text), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("PURGE", "/page", nil))
	line := out.String()
	assert.Equal(t, 1, strings.Count(line, "\n"), "one line per request")
	for _, field := range []string{"msg=request", "method=PURGE", "uri=/page", "status=403", `cache=""`} {
		assert.Contains(t, line, field)
	}
	assert.NotContains(t, line, "forwarded_for", "only logged when sent")
}
