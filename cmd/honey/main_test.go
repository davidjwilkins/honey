package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/cache"
	"github.com/davidjwilkins/honey/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandlerAppliesConfig(t *testing.T) {
	var hits int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.SetCookie(w, &http.Cookie{Name: "site_lang_id", Value: "1"})
		http.SetCookie(w, &http.Cookie{Name: "tracking", Value: "1"})
		io.WriteString(w, "hello")
	}))
	defer origin.Close()

	cfg, err := config.Parse(`
[backend]
uri = "` + origin.URL + `"

[cache]
allowedCookies = ["site_lang_id"]

[[route]]
match = "/wp-admin"
cache = false
`)
	require.NoError(t, err)
	proxy := httptest.NewServer(newHandler(cfg))
	defer proxy.Close()

	get := func(path string) *http.Response {
		resp, err := http.Get(proxy.URL + path)
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		assert.Equal(t, "hello", string(body))
		return resp
	}

	resp := get("/page")
	assert.Equal(t, "MISS", resp.Header.Get("X-Honey-Cache"))
	require.Len(t, resp.Cookies(), 1, "only allowed cookies should pass through the cache")
	assert.Equal(t, "site_lang_id", resp.Cookies()[0].Name)
	assert.Equal(t, "HIT", get("/page").Header.Get("X-Honey-Cache"))

	assert.Equal(t, "NO-CACHE", get("/wp-admin/edit.php").Header.Get("X-Honey-Cache"))
	assert.Equal(t, "NO-CACHE", get("/wp-admin/edit.php").Header.Get("X-Honey-Cache"))
	assert.Equal(t, int32(3), atomic.LoadInt32(&hits))

	get("/style.css")
	assert.Equal(t, "HIT", get("/style.css").Header.Get("X-Honey-Cache"), "static files should be cached by default")
}

func TestHandlerCanSkipStaticFiles(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello")
	}))
	defer origin.Close()
	cfg, err := config.Parse("[backend]\nuri = \"" + origin.URL + "\"\n[cache]\nstaticFiles = false\n")
	require.NoError(t, err)
	proxy := httptest.NewServer(newHandler(cfg))
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/style.css")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, "NO-CACHE", resp.Header.Get("X-Honey-Cache"))
}

func TestHandlerBrotliSetting(t *testing.T) {
	page := strings.Repeat("<p>Some compressible page content.</p>\n", 200)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, page)
	}))
	defer origin.Close()

	for setting, expected := range map[string]string{
		"":                             "br",
		"brotli = false":               "gzip",
		"brotli = false\ngzip = false": "",
	} {
		cfg, err := config.Parse("[backend]\nuri = \"" + origin.URL + "\"\n[cache]\n" + setting + "\n")
		require.NoError(t, err)
		proxy := httptest.NewServer(newHandler(cfg))
		req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/page", nil)
		req.Header.Set("Accept-Encoding", "gzip, br")
		resp, err := (&http.Transport{}).RoundTrip(req)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, expected, resp.Header.Get("Content-Encoding"), setting)
		proxy.Close()
	}
}

func TestHandlerControl(t *testing.T) {
	var hits int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, fmt.Sprint(atomic.AddInt32(&hits, 1)))
	}))
	defer origin.Close()
	cfg, err := config.Parse("[backend]\nuri = \"" + origin.URL + "\"\n[control]\nsecret = \"0123456789abcdef\"\n")
	require.NoError(t, err)
	proxy := httptest.NewServer(newHandler(cfg))
	defer proxy.Close()

	do := func(method string, headers ...string) (int, string) {
		req, _ := http.NewRequest(method, proxy.URL+"/page", nil)
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(body)
	}
	_, body := do(http.MethodGet)
	assert.Equal(t, "1", body)
	_, body = do(http.MethodGet, "Cache-Control", "no-cache")
	assert.Equal(t, "1", body, "untrusted no-cache is ignored")
	status, _ := do("PURGE")
	assert.Equal(t, http.StatusForbidden, status)
	status, _ = do("PURGE", "X-Honey-Secret", "0123456789abcdef")
	assert.Equal(t, http.StatusOK, status)
	_, body = do(http.MethodGet)
	assert.Equal(t, "2", body, "the purged page is fetched again")
}

func TestHandlerBackendTimeout(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer origin.Close()
	cfg, err := config.Parse("[backend]\nuri = \"" + origin.URL + "\"\ntimeout = \"1s\"\n")
	require.NoError(t, err)
	proxy := httptest.NewServer(newHandler(cfg))
	defer proxy.Close()

	start := time.Now()
	resp, err := http.Get(proxy.URL + "/page")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusGatewayTimeout, resp.StatusCode)
	assert.Less(t, time.Since(start), 3*time.Second)
}

func TestHandlerMetrics(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello")
	}))
	defer origin.Close()

	cfg, err := config.Parse("[backend]\nuri = \"" + origin.URL + "\"\n")
	require.NoError(t, err)
	_, metricsHandler := newHandlers(cfg)
	assert.Nil(t, metricsHandler, "metrics are off unless configured")

	cfg, err = config.Parse("[backend]\nuri = \"" + origin.URL + "\"\n[metrics]\nlisten = \"127.0.0.1:0\"\n")
	require.NoError(t, err)
	handler, metricsHandler := newHandlers(cfg)
	require.NotNil(t, metricsHandler)
	proxy := httptest.NewServer(handler)
	defer proxy.Close()
	for i := 0; i < 3; i++ {
		resp, err := http.Get(proxy.URL + "/page")
		require.NoError(t, err)
		resp.Body.Close()
	}
	req, _ := http.NewRequest("PURGE", proxy.URL+"/page", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	w := httptest.NewRecorder()
	metricsHandler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, line := range []string{
		`honey_requests_total{cache="miss"} 1`,
		`honey_requests_total{cache="hit"} 2`,
		`honey_requests_total{cache="none"} 1`,
		`honey_responses_total{code="4xx"} 1`,
		`honey_backend_requests_total{code="2xx"} 1`,
		"honey_cache_entries 1",
		fmt.Sprintf("honey_cache_max_bytes %d", cache.DefaultMaxBytes),
	} {
		assert.Contains(t, w.Body.String(), line+"\n")
	}
}

func TestHandlerQueryParams(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.RequestURI())
		mu.Unlock()
		io.WriteString(w, "hello")
	}))
	defer origin.Close()
	cfg, err := config.Parse("[backend]\nuri = \"" + origin.URL + "\"\n[cache]\nqueryParams = [\"p\"]\n")
	require.NoError(t, err)
	proxy := httptest.NewServer(newHandler(cfg))
	defer proxy.Close()

	for _, query := range []string{"?p=1&utm_source=a", "?utm_source=b&p=1", "?p=1"} {
		resp, err := http.Get(proxy.URL + "/page" + query)
		require.NoError(t, err)
		resp.Body.Close()
	}
	assert.Equal(t, []string{"/page?p=1"}, seen)
}

func TestHandlerAccessLog(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello")
	}))
	defer origin.Close()
	var out bytes.Buffer
	accessLogOutput = &out
	defer func() { accessLogOutput = os.Stdout }()

	cfg, err := config.Parse("[backend]\nuri = \"" + origin.URL + "\"\n")
	require.NoError(t, err)
	proxy := httptest.NewServer(newHandler(cfg))
	resp, err := http.Get(proxy.URL + "/page")
	require.NoError(t, err)
	resp.Body.Close()
	proxy.Close()
	assert.Empty(t, out.String(), "access logging is off unless configured")

	cfg, err = config.Parse("[backend]\nuri = \"" + origin.URL + "\"\n[log]\naccess = true\nformat = \"json\"\n")
	require.NoError(t, err)
	proxy = httptest.NewServer(newHandler(cfg))
	defer proxy.Close()
	for i := 0; i < 2; i++ {
		resp, err := http.Get(proxy.URL + "/page?p=1")
		require.NoError(t, err)
		resp.Body.Close()
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Len(t, lines, 2)
	assert.Contains(t, lines[0], `"uri":"/page?p=1"`)
	assert.Contains(t, lines[0], `"cache":"MISS"`)
	assert.Contains(t, lines[1], `"cache":"HIT"`)
}

func TestHandlerStallTimeout(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "20")
		io.WriteString(w, "first half")
		w.(http.Flusher).Flush()
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer origin.Close()
	cfg, err := config.Parse("[backend]\nuri = \"" + origin.URL + "\"\nstallTimeout = \"1s\"\n")
	require.NoError(t, err)
	proxy := httptest.NewServer(newHandler(cfg))
	defer proxy.Close()

	start := time.Now()
	resp, err := http.Get(proxy.URL + "/page")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusGatewayTimeout, resp.StatusCode)
	assert.Less(t, time.Since(start), 3*time.Second)
}
