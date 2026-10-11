package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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
stripCookies = ["track"]

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
	assert.Nil(t, build(cfg, time.Now()).metrics, "metrics are off unless configured")

	cfg, err = config.Parse("[backend]\nuri = \"" + origin.URL + "\"\n[metrics]\nlisten = \"127.0.0.1:0\"\n")
	require.NoError(t, err)
	h := build(cfg, time.Now())
	require.NotNil(t, h.metrics)
	metricsHandler := h.metrics
	proxy := httptest.NewServer(h.handler)
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

func TestHandlerRoutes(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.URL.RequestURI())
	}))
	defer origin.Close()
	cfg, err := config.Parse("[backend]\nuri = \"" + origin.URL + "\"\n[cache]\nqueryParams = []\n" +
		"[[route]]\nmatch = \"/uploads/\"\ndefaultTTL = \"7d\"\n" +
		"[[route]]\nmatch = \"/search\"\nqueryParams = [\"s\"]\n" +
		"[[route]]\nmatch = \"/admin\"\ncache = false\n")
	require.NoError(t, err)
	proxy := httptest.NewServer(newHandler(cfg))
	defer proxy.Close()

	get := func(path string) (*http.Response, string) {
		resp, err := http.Get(proxy.URL + path)
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(body)
	}
	resp, _ := get("/uploads/photo.jpg")
	assert.Equal(t, "public, max-age=604800", resp.Header.Get("Cache-Control"))
	_, body := get("/search?s=honey&x=1")
	assert.Equal(t, "/search?s=honey", body)
	_, body = get("/page?s=honey")
	assert.Equal(t, "/page", body)
	get("/admin")
	resp, _ = get("/admin")
	assert.Equal(t, "NO-CACHE", resp.Header.Get("X-Honey-Cache"))
}

func TestHandlerBypassCookies(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("PHPSESSID"); err == nil {
			io.WriteString(w, "personal")
			return
		}
		io.WriteString(w, "public")
	}))
	defer origin.Close()
	cfg, err := config.Parse("[backend]\nuri = \"" + origin.URL + "\"\n[cache]\nbypassCookies = [\"PHPSESSID\"]\n" +
		"[[route]]\nmatch = \"/uploads/\"\nbypassCookies = []\n")
	require.NoError(t, err)
	proxy := httptest.NewServer(newHandler(cfg))
	defer proxy.Close()

	get := func(path string) (*http.Response, string) {
		req, err := http.NewRequest(http.MethodGet, proxy.URL+path, nil)
		require.NoError(t, err)
		req.Header.Set("Cookie", "PHPSESSID=abc")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(body)
	}
	resp, body := get("/page")
	assert.Equal(t, "NO-CACHE", resp.Header.Get("X-Honey-Cache"))
	assert.Equal(t, "personal", body)
	get("/uploads/cat.jpg")
	resp, _ = get("/uploads/cat.jpg")
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"), "the route doesn't bypass the cache for the cookie")
}

func TestHandlerBrowserCacheControl(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello")
	}))
	defer origin.Close()
	cfg, err := config.Parse("[backend]\nuri = \"" + origin.URL + "\"\n[cache]\nbrowserCacheControl = \"no-cache\"\n" +
		"[[route]]\nmatch = \"/assets/\"\nbrowserCacheControl = \"public, max-age=31536000, immutable\"\n")
	require.NoError(t, err)
	proxy := httptest.NewServer(newHandler(cfg))
	defer proxy.Close()

	for path, want := range map[string]string{"/page": "no-cache", "/assets/app.css": "public, max-age=31536000, immutable"} {
		resp, err := http.Get(proxy.URL + path)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, want, resp.Header.Get("Cache-Control"), path)
	}
}

func TestHandlerAdmin(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello")
	}))
	defer origin.Close()
	cfg, err := config.Parse("[backend]\nuri = \"" + origin.URL + "\"\n")
	require.NoError(t, err)
	assert.Nil(t, build(cfg, time.Now()).admin, "the admin page is off unless configured")

	cfg, err = config.Parse("[backend]\nuri = \"" + origin.URL + "\"\n[admin]\nlisten = \"127.0.0.1:0\"\n")
	require.NoError(t, err)
	h := build(cfg, time.Now())
	require.NotNil(t, h.admin)
	proxy := httptest.NewServer(h.handler)
	defer proxy.Close()
	adminServer := httptest.NewServer(h.admin)
	defer adminServer.Close()

	cacheResult := func() string {
		resp, err := http.Get(proxy.URL + "/blog/post")
		require.NoError(t, err)
		resp.Body.Close()
		return resp.Header.Get("X-Honey-Cache")
	}
	cacheResult()
	require.Equal(t, "HIT", cacheResult())

	req, err := http.NewRequest(http.MethodPost, adminServer.URL+"/purge", strings.NewReader("url=%2Fblog%2F&prefix=1"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", adminServer.URL)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Contains(t, string(body), "Purged 1 cached response for")
	assert.Equal(t, "MISS", cacheResult())

	resp, err = http.Get(adminServer.URL + "/metrics")
	require.NoError(t, err)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Contains(t, string(body), `honey_requests_total{cache="hit"} 1`)
}

func TestHandlerBackendHost(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Host+" "+r.Header.Get("X-Forwarded-Host"))
	}))
	defer origin.Close()
	for setting, want := range map[string]string{
		"":                             "www.example.com www.example.com",
		"host = \"origin.internal\"\n": "origin.internal www.example.com",
	} {
		cfg, err := config.Parse("[backend]\nuri = \"" + origin.URL + "\"\n" + setting)
		require.NoError(t, err)
		proxy := httptest.NewServer(newHandler(cfg))
		req, err := http.NewRequest(http.MethodGet, proxy.URL+"/", nil)
		require.NoError(t, err)
		req.Host = "www.example.com"
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		proxy.Close()
		assert.Equal(t, want, string(body), setting)
	}
}

func TestHandlerWarm(t *testing.T) {
	var hits int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.URL.Path == "/wp-sitemap.xml" {
			io.WriteString(w, `<urlset><url><loc>https://www.example.com/a/</loc></url><url><loc>https://www.example.com/b/</loc></url></urlset>`)
			return
		}
		io.WriteString(w, r.Host+r.URL.Path)
	}))
	defer origin.Close()
	cfg, err := config.Parse("[backend]\nuri = \"" + origin.URL + "\"\n" +
		"[warm]\nsitemap = \"https://www.example.com/wp-sitemap.xml\"\nrate = 100\n" +
		"[metrics]\nlisten = \"127.0.0.1:0\"\n[control]\nsecret = \"0123456789abcdef\"\n")
	require.NoError(t, err)
	h := build(cfg, time.Now())
	require.NotNil(t, h.warmer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.warmer.Run(ctx)

	proxy := httptest.NewServer(h.handler)
	defer proxy.Close()
	request := func(method, path string) *http.Response {
		req, err := http.NewRequest(method, proxy.URL+path, nil)
		require.NoError(t, err)
		req.Host = "www.example.com"
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("X-Honey-Secret", "0123456789abcdef")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp
	}
	waitForHits := func(n int32) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for atomic.LoadInt32(&hits) < n && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		require.Equal(t, n, atomic.LoadInt32(&hits))
	}
	waitForHits(3) // the sitemap and both pages
	for _, path := range []string{"/a/", "/b/"} {
		assert.Equal(t, "HIT", request(http.MethodGet, path).Header.Get("X-Honey-Cache"), path)
	}
	w := httptest.NewRecorder()
	h.metrics.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Contains(t, w.Body.String(), `honey_requests_total{cache="hit"} 2`+"\n", "warming isn't counted as visits")
	assert.Contains(t, w.Body.String(), `honey_requests_total{cache="miss"} 0`+"\n")

	// Purging everything warms the cache again
	request("PURGE", "/*")
	waitForHits(6)
	assert.Equal(t, "HIT", request(http.MethodGet, "/a/").Header.Get("X-Honey-Cache"))
	// but purging a page doesn't
	request("PURGE", "/a/")
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int32(6), atomic.LoadInt32(&hits))
}

func TestHandlerTrustedProxies(t *testing.T) {
	var seen http.Header
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
	}))
	defer origin.Close()
	defer func(w io.Writer) { accessLogOutput = w }(accessLogOutput)

	for _, tc := range []struct {
		name, setting       string
		purge               int
		proto, remoteLogged string
	}{
		// The test's requests come from 127.0.0.1, like a proxy on the
		// same host
		{"behind a proxy", "", http.StatusOK, "https", "203.0.113.9"},
		{"trusting nobody", "trustedProxies = []\n", http.StatusForbidden, "http", "127.0.0.1"},
	} {
		var logs bytes.Buffer
		accessLogOutput = &logs
		cfg, err := config.Parse(tc.setting + "[backend]\nuri = \"" + origin.URL + "\"\n" +
			"[log]\naccess = true\nformat = \"json\"\n[control]\nallowIPs = [\"203.0.113.9\"]\n")
		require.NoError(t, err)
		proxy := httptest.NewServer(newHandler(cfg))
		send := func(method string) int {
			req, err := http.NewRequest(method, proxy.URL+"/page", nil)
			require.NoError(t, err)
			req.Header.Set("X-Forwarded-For", "203.0.113.9")
			req.Header.Set("X-Forwarded-Proto", "https")
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			resp.Body.Close()
			return resp.StatusCode
		}
		send(http.MethodGet)
		assert.Equal(t, tc.proto, seen.Get("X-Forwarded-Proto"), tc.name)
		assert.Contains(t, logs.String(), `"remote":"`+tc.remoteLogged+`"`, tc.name)
		assert.Equal(t, tc.purge, send("PURGE"), "%s: allowIPs is checked against the visitor's address", tc.name)
		proxy.Close()
	}
}

func TestHandlerHosts(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello")
	}))
	defer origin.Close()
	cfg, err := config.Parse("hosts = [\"www.example.com\"]\n[backend]\nuri = \"" + origin.URL + "\"\n")
	require.NoError(t, err)
	proxy := httptest.NewServer(newHandler(cfg))
	defer proxy.Close()

	for host, status := range map[string]int{"www.example.com": http.StatusOK, "evil.example": http.StatusMisdirectedRequest} {
		req, err := http.NewRequest(http.MethodGet, proxy.URL+"/", nil)
		require.NoError(t, err)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, status, resp.StatusCode, host)
	}
}

func TestCacheSurvivesRestart(t *testing.T) {
	var hits int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Cache-Control", "max-age=3600")
		io.WriteString(w, "hello")
	}))
	defer origin.Close()
	path := filepath.Join(t.TempDir(), "cache.snapshot")
	cfg, err := config.Parse("[backend]\nuri = \"" + origin.URL + "\"\n[cache]\npersist = \"" + path + "\"\n")
	require.NoError(t, err)

	cacheResult := func(handler http.Handler) string {
		proxy := httptest.NewServer(handler)
		defer proxy.Close()
		req, err := http.NewRequest(http.MethodGet, proxy.URL+"/page", nil)
		require.NoError(t, err)
		// The new process listens on a different port, but the site's
		// address is the same
		req.Host = "www.example.com"
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		return resp.Header.Get("X-Honey-Cache")
	}

	h := build(cfg, time.Now())
	handler, cacher := h.handler, h.cacher
	assert.Equal(t, "MISS", cacheResult(handler))
	saveCache(cacher, path)

	// a new process, with an empty cache, loads the saved one
	h = build(cfg, time.Now())
	handler, cacher = h.handler, h.cacher
	loaded, err := cacher.LoadFrom(path)
	require.NoError(t, err)
	assert.Equal(t, 1, loaded)
	assert.Equal(t, "HIT", cacheResult(handler))
	assert.Equal(t, int32(1), atomic.LoadInt32(&hits))
}

func TestPersistEvery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.snapshot")
	cacher := build(&config.Config{Backend: &url.URL{Scheme: "http", Host: "127.0.0.1:1"}}, time.Now()).cacher
	stop := persistEvery(cacher, path, 50*time.Millisecond)
	require.Eventually(t, func() bool {
		_, err := os.Stat(path)
		return err == nil
	}, 2*time.Second, 10*time.Millisecond, "the cache should be saved periodically")
	stop()

	// nothing happens without a path or an interval
	persistEvery(cacher, "", time.Millisecond)()
	persistEvery(cacher, path, 0)()
}
