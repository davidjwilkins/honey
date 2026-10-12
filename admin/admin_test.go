package admin

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/events"
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
		Hosts:   []string{"www.example.com", "example.com"},
		Started: time.Now().Add(-90 * time.Minute),
		Metrics: m,
		Events:  events.New(10),
		Count: func(rawURL string, prefix bool) (int, error) {
			if rawURL == "bad" {
				return 0, errors.New(`"bad" isn't a URL or path`)
			}
			if prefix {
				return 38, nil
			}
			return 1, nil
		},
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
		"v1.2.3", "https://www.example.com", "hosts www.example.com, example.com",
		"75.0%",           // hit ratio
		"of 4 requests",   // in total
		"1h 30m",          // uptime
		"3.0 MB",          // cache size
		"/ 256.0 MB",      // and limit
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

func TestCount(t *testing.T) {
	h, calls := newAdmin(t)
	for target, want := range map[string]string{
		"/count?url=%2Fblog%2F&prefix=1": `{"count":38}`,
		"/count?url=%2Fblog%2F&prefix=":  `{"count":1}`,
		"/count?url=bad":                 `{"error":"\"bad\" isn't a URL or path"}`,
		"/count?url=+":                   `{"error":"enter a URL or path"}`,
	} {
		w := do(h, http.MethodGet, target, "127.0.0.1:9090", nil)
		assert.Equal(t, http.StatusOK, w.Code, target)
		assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
		assert.JSONEq(t, want, w.Body.String(), target)
	}
	assert.Empty(t, *calls, "counting doesn't purge")
	assert.Equal(t, http.StatusMisdirectedRequest, do(h, http.MethodGet, "/count?url=/", "evil.example", nil).Code,
		"other sites can't read it with DNS rebinding")
}

func TestPurgesAreLogged(t *testing.T) {
	log := events.New(10)
	h := Handler(Options{Started: time.Now(), Events: log, Purge: func(string, bool, bool) (int, error) { return 3, nil }})
	do(h, http.MethodPost, "/purge", "127.0.0.1", url.Values{"url": {"/blog/"}, "prefix": {"1"}, "soft": {"1"}}, "Origin", "http://127.0.0.1")
	recent := log.Recent(1)
	require.Len(t, recent, 1)
	assert.Equal(t, events.Purge, recent[0].Kind)
	assert.Equal(t, "soft purged /blog/ and everything under it · 3 responses · from the admin page", recent[0].Text)

	body := do(h, http.MethodGet, "/stats", "127.0.0.1", nil).Body.String()
	assert.Contains(t, body, `class="e-purge"`)
	assert.Contains(t, body, "soft purged /blog/ and everything under it")
}

func TestEventsAreShown(t *testing.T) {
	log := events.New(10)
	for i := 0; i < 3; i++ {
		log.Add(events.Error, "backend 502", time.Minute, events.Text("the backend responded 502, most recently for /shop/<b>"))
	}
	h := Handler(Options{Started: time.Now(), Events: log})
	body := do(h, http.MethodGet, "/", "127.0.0.1", nil).Body.String()
	assert.Contains(t, body, `class="e-error"`)
	assert.Contains(t, body, "most recently for /shop/&lt;b&gt;", "event text is escaped")
	assert.Contains(t, body, "×3")
}

func TestTrend(t *testing.T) {
	start := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	history := make([]metrics.Minute, 60)
	for i := range history {
		history[i].Start = start.Add(time.Duration(i) * time.Minute)
	}
	assert.False(t, hitTrend(history).Enough)

	history[10] = metrics.Minute{Requests: 100, FromCache: 90}
	history[11] = metrics.Minute{Requests: 100, FromCache: 100}
	history[30] = metrics.Minute{Requests: 10, FromCache: 9}
	history[59] = metrics.Minute{Requests: 10, FromCache: 10}
	tr := hitTrend(history)
	require.True(t, tr.Enough)
	assert.True(t, tr.Hour)
	assert.InDelta(t, 0.9, tr.Min, 0.0001)
	require.Len(t, tr.Lines, 3, "minutes without requests break the line")
	// The scale goes from a little below the lowest share (88%) at the
	// bottom (y=44) to 100% at the top (y=4)
	assert.Equal(t, "40.7,37.3 44.7,4.0", tr.Lines[0])
	assert.Equal(t, "240.0,4.0", tr.Lines[2])
}

func TestHistogram(t *testing.T) {
	buckets := make([]uint64, len(metrics.BackendBuckets())+1)
	buckets[4] = 200 // 100-250ms
	buckets[5] = 100
	buckets[len(buckets)-1] = 1 // over 30s
	bars := histogram(buckets)
	require.Len(t, bars, 12)
	assert.Equal(t, "10ms", bars[0].Label)
	assert.Equal(t, "2.5s", bars[7].Label)
	assert.Equal(t, "30s+", bars[11].Label)
	assert.Equal(t, "over 30s", bars[11].Range)
	assert.Equal(t, 1.0, bars[4].Height)
	assert.Equal(t, 0.5, bars[5].Height)
	assert.Equal(t, 0.02, bars[11].Height, "a few slow requests still show")
	assert.Zero(t, bars[0].Height)
	assert.True(t, bars[11].Slow)
	assert.False(t, bars[4].Slow)
	assert.Equal(t, 41, bars[4].X)
}

func TestBackendStats(t *testing.T) {
	m := metrics.New(nil, 1<<20)
	backend := m.Transport(roundTripper(func(r *http.Request) (*http.Response, error) {
		status := http.StatusOK
		if r.URL.Path == "/shop/" {
			status = http.StatusBadGateway
		}
		return &http.Response{StatusCode: status, Body: http.NoBody}, nil
	}))
	for _, path := range []string{"/", "/", "/shop/"} {
		_, err := backend.RoundTrip(httptest.NewRequest(http.MethodGet, "http://backend"+path, nil))
		require.NoError(t, err)
	}
	h := Handler(Options{Started: time.Now(), Metrics: m})
	body := do(h, http.MethodGet, "/stats", "127.0.0.1", nil).Body.String()
	assert.Contains(t, body, `class="tile alert"`, "a recent backend error is highlighted")
	assert.Contains(t, body, "latest: 502 for /shop/")
	assert.Contains(t, body, "<span class=\"unit\"> p50</span>")
	assert.Contains(t, body, "3 backend requests")
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTrendSinceStart(t *testing.T) {
	start := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	history := make([]metrics.Minute, 60)
	for i := range history {
		history[i] = metrics.Minute{Start: start.Add(time.Duration(i) * time.Minute), Requests: 10, FromCache: 9}
	}
	recent := sinceStart(history, start.Add(57*time.Minute+30*time.Second))
	require.Len(t, recent, 3, "the minute Honey started in, and those after")
	tr := hitTrend(recent)
	assert.False(t, tr.Hour)
	assert.Equal(t, []string{"0.0,37.3 120.0,37.3 240.0,37.3"}, tr.Lines, "they fill the width")
	assert.Len(t, sinceStart(history, start.Add(-time.Hour)), 60)
}
