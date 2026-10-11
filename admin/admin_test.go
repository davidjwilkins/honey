package admin

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/metrics"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type purgeCall struct {
	url          string
	prefix, soft bool
}

func newAdmin(t *testing.T) (http.Handler, *[]purgeCall) {
	m := metrics.New(func() (int, int64) { return 1234, 3 << 20 }, 256<<20)
	counted := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Honey-Cache", r.URL.Query().Get("cache"))
	}))
	for _, result := range []string{"HIT", "HIT", "HIT", "MISS"} {
		counted.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/?cache="+url.QueryEscape(result), nil))
	}
	var calls []purgeCall
	return Handler(Options{
		Version: "v1.2.3",
		Backend: "https://www.example.com",
		Started: time.Now().Add(-90 * time.Minute),
		Metrics: m,
		Purge: func(rawURL string, prefix, soft bool) (int, error) {
			calls = append(calls, purgeCall{rawURL, prefix, soft})
			if rawURL == "bad" {
				return 0, errors.New(`"bad" isn't a URL or path`)
			}
			return 3, nil
		},
	}), &calls
}

func do(h http.Handler, method, target, host string, form url.Values, headers ...string) *httptest.ResponseRecorder {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, target, body)
	r.Host = host
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestPage(t *testing.T) {
	h, _ := newAdmin(t)
	w := do(h, http.MethodGet, "/", "127.0.0.1:9090", nil)
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	for _, want := range []string{
		"v1.2.3", "https://www.example.com",
		"75.0%",           // hit ratio
		"of 4 requests",   // in total
		"1h 30m",          // uptime
		"3.0 MB",          // cache size
		"of 256.0 MB",     // and limit
		"1,234 entries",   // with separators
		`action="/purge"`, // the purge form
	} {
		assert.Contains(t, body, want)
	}
	assert.Contains(t, w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'")
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	assert.Equal(t, "same-origin", w.Header().Get("Referrer-Policy"), "no-referrer would stop the page's forms working")

	w = do(h, http.MethodGet, "/stats", "127.0.0.1:9090", nil)
	assert.Contains(t, w.Body.String(), "75.0%")
	assert.NotContains(t, w.Body.String(), "<html", "it's the stats section only")

	for _, path := range []string{"/admin.css", "/admin.js"} {
		w = do(h, http.MethodGet, path, "localhost:9090", nil)
		assert.Equal(t, http.StatusOK, w.Code, path)
	}
	assert.Equal(t, http.StatusNotFound, do(h, http.MethodGet, "/nope", "localhost", nil).Code)
}

func TestPageWithNothingCounted(t *testing.T) {
	h := Handler(Options{Version: "dev", Started: time.Now(), Metrics: metrics.New(nil, 1<<20)})
	w := do(h, http.MethodGet, "/", "127.0.0.1", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "of 0 requests")
}

func TestOnlyServedForIPsAndLocalhost(t *testing.T) {
	h, _ := newAdmin(t)
	for _, host := range []string{"127.0.0.1:9090", "10.1.2.3", "[::1]:9090", "localhost:9090", "LOCALHOST"} {
		assert.Equal(t, http.StatusOK, do(h, http.MethodGet, "/", host, nil).Code, host)
	}
	for _, host := range []string{"evil.example:9090", "localhost.evil.example", ""} {
		assert.Equal(t, http.StatusMisdirectedRequest, do(h, http.MethodGet, "/", host, nil).Code, host)
	}
	w := do(h, http.MethodGet, "/metrics", "honey.internal:9090", nil)
	assert.Equal(t, http.StatusOK, w.Code, "Prometheus may scrape by hostname")
	assert.Contains(t, w.Body.String(), `honey_requests_total{cache="hit"} 3`)
}

func TestPurge(t *testing.T) {
	h, calls := newAdmin(t)
	form := url.Values{"url": {"/blog/<script>"}, "prefix": {"1"}}
	w := do(h, http.MethodPost, "/purge", "127.0.0.1:9090", form, "Origin", "http://127.0.0.1:9090")
	require.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, []purgeCall{{"/blog/<script>", true, false}}, *calls)

	result := w.Header().Get("Location")
	require.True(t, strings.HasPrefix(result, "/?result="), result)
	body := do(h, http.MethodGet, result, "127.0.0.1:9090", nil).Body.String()
	assert.Contains(t, body, "Purged 3 cached responses for")
	assert.Contains(t, body, "/blog/&lt;script&gt;")
	assert.Contains(t, body, "and everything under it")
	assert.NotContains(t, do(h, http.MethodGet, result, "127.0.0.1:9090", nil).Body.String(), "Purged 3",
		"the result is only shown once, so reloading doesn't show it again")

	w = do(h, http.MethodPost, "/purge", "127.0.0.1:9090", url.Values{"url": {"/page"}, "soft": {"1"}}, "Origin", "http://127.0.0.1:9090")
	body = do(h, http.MethodGet, w.Header().Get("Location"), "127.0.0.1:9090", nil).Body.String()
	assert.Contains(t, body, "Expired 3 cached responses for")
	assert.Contains(t, body, "refreshed the next time")
	assert.Equal(t, purgeCall{"/page", false, true}, (*calls)[1])

	w = do(h, http.MethodPost, "/purge", "127.0.0.1:9090", url.Values{"url": {"bad"}}, "Sec-Fetch-Site", "same-origin")
	require.Equal(t, http.StatusSeeOther, w.Code)
	body = do(h, http.MethodGet, w.Header().Get("Location"), "127.0.0.1:9090", nil).Body.String()
	assert.Contains(t, body, "Couldn&#39;t purge: &#34;bad&#34; isn&#39;t a URL or path")

	w = do(h, http.MethodPost, "/purge", "127.0.0.1:9090", url.Values{"url": {" "}}, "Origin", "http://127.0.0.1:9090")
	body = do(h, http.MethodGet, w.Header().Get("Location"), "127.0.0.1:9090", nil).Body.String()
	assert.Contains(t, body, "Enter a URL or path to purge.")
	assert.Len(t, *calls, 3)

	assert.Equal(t, http.StatusMethodNotAllowed, do(h, http.MethodGet, "/purge", "127.0.0.1", nil).Code)
}

func TestPurgeOnlyFromThePage(t *testing.T) {
	h, calls := newAdmin(t)
	form := url.Values{"url": {"/"}, "prefix": {"1"}}
	for _, headers := range [][]string{
		{},
		{"Origin", "https://evil.example"},
		{"Origin", "http://127.0.0.1:9091"},
		{"Origin", "null"},
		{"Sec-Fetch-Site", "cross-site"},
	} {
		w := do(h, http.MethodPost, "/purge", "127.0.0.1:9090", form, headers...)
		assert.Equal(t, http.StatusForbidden, w.Code, headers)
	}
	assert.Empty(t, *calls)
}

func TestResultsAreBounded(t *testing.T) {
	a := &admin{opts: Options{Purge: func(string, bool, bool) (int, error) { return 1, nil }}, results: map[string]outcome{}}
	// e.g. a script posting without following the redirects
	for i := 0; i < maxResults*3; i++ {
		r := httptest.NewRequest(http.MethodPost, "/purge", strings.NewReader("url=/"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		a.purge(httptest.NewRecorder(), r)
		assert.LessOrEqual(t, len(a.results), maxResults)
	}
}

func TestFormat(t *testing.T) {
	assert.Equal(t, "512 B", formatBytes(512))
	assert.Equal(t, "1.5 KB", formatBytes(1536))
	assert.Equal(t, "2.0 GB", formatBytes(2<<30))
	assert.Equal(t, "12ms", formatDuration(12*time.Millisecond))
	assert.Equal(t, "2.5s", formatDuration(2500*time.Millisecond))
	assert.Equal(t, "3m 4s", formatDuration(184*time.Second))
	assert.Equal(t, "3 days", formatDuration(80*time.Hour))
	assert.Equal(t, "1", formatCount(1))
	assert.Equal(t, "999", formatCount(uint64(999)))
	assert.Equal(t, "1,000", formatCount(int64(1000)))
	assert.Equal(t, "12,345,678", formatCount(12345678))
}
