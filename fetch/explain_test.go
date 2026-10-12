package fetch

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// explainSetup is a cache like a WordPress site's, and a proxy using it
func explainSetup(t *testing.T) (cache.Cacher, *url.URL, Options, string) {
	o := noHeadersOrigin()
	t.Cleanup(o.Close)
	backend, err := url.Parse(o.URL)
	require.NoError(t, err)
	c := cache.NewCacher(cache.Options{BypassCookies: cache.WordPressBypassCookies, DefaultTTL: 5 * time.Minute, StaleWhileRevalidate: time.Hour})
	c.AddRoute(cache.Route{Regex: regexp.MustCompile(`/wp-admin`), NoCache: true})
	c.AddRoute(cache.Route{Prefix: "/wp-content/uploads/", DefaultTTL: 7 * 24 * time.Hour})
	c.AddAllowedCookie("lang")
	opts := Options{Hosts: []string{"www.example.com"}, QueryParams: []string{"p", "s"}}
	proxy := httptest.NewServer(FetchWithOptions(c, Forwarder(c), backend, opts))
	t.Cleanup(proxy.Close)
	return c, backend, opts, proxy.URL
}

func explainRequest(method, rawURL, cookies string) *http.Request {
	r := httptest.NewRequest(method, rawURL, nil)
	if cookies != "" {
		r.Header.Set("Cookie", cookies)
	}
	return r
}

func titles(steps []Step) []string {
	var t []string
	for _, s := range steps {
		t = append(t, s.Outcome+": "+s.Title)
	}
	return t
}

func TestExplainCacheable(t *testing.T) {
	c, backend, opts, _ := explainSetup(t)
	e := Explain(c, backend, opts, explainRequest(http.MethodGet, "http://www.example.com/blog/?utm_source=x&s=honey", "_ga=1; lang=fr"))
	assert.True(t, e.Cacheable)
	assert.Equal(t, "Cacheable, not in the cache: the next request fetches and caches it", e.Verdict)
	assert.Equal(t, []string{
		"ok: Host allowed",
		"ok: No route applies",
		"changed: Query parameters filtered",
		"changed: Cookies removed",
	}, titles(e.Steps))
	assert.Equal(t, "removed utm_source, which aren't in queryParams; kept s", e.Steps[2].Detail)
	assert.Equal(t, "removed _ga before the backend sees them, as they aren't in allowedCookies; kept lang", e.Steps[3].Detail)
	assert.Equal(t, "GET :: http://www.example.com/blog/?s=honey :: cookies :: lang=fr", e.Key)
	assert.Equal(t, 5*time.Minute, e.DefaultTTL)
	assert.Equal(t, time.Hour, e.StaleWhileRevalidate)
	assert.Nil(t, e.Cached)
}

func TestExplainRoutes(t *testing.T) {
	c, backend, opts, _ := explainSetup(t)
	e := Explain(c, backend, opts, explainRequest(http.MethodGet, "http://www.example.com/wp-content/uploads/a.jpg", ""))
	assert.True(t, e.Cacheable)
	assert.Equal(t, "changed: Route 2 applies", titles(e.Steps)[1])
	assert.Equal(t, 7*24*time.Hour, e.DefaultTTL, "the route's settings apply")

	e = Explain(c, backend, opts, explainRequest(http.MethodGet, "http://www.example.com/wp-admin/edit.php", ""))
	assert.False(t, e.Cacheable)
	assert.Equal(t, "Not cached: every request goes to the backend", e.Verdict)
	last := e.Steps[len(e.Steps)-1]
	assert.Equal(t, Step{Outcome: StepStopped, Title: "Not cacheable", Detail: "route 1 (/wp-admin) has cache = false"}, last)
}

func TestExplainNotCacheable(t *testing.T) {
	c, backend, opts, _ := explainSetup(t)
	for _, tc := range []struct{ method, url, cookies, reason string }{
		{http.MethodPost, "http://www.example.com/", "", "only GET and HEAD requests are cached"},
		{http.MethodGet, "http://www.example.com/", "wordpress_logged_in_abc=admin", "it has the cookie wordpress_logged_in_abc, which marks visitors whose pages may be personal (bypassCookies)"},
	} {
		e := Explain(c, backend, opts, explainRequest(tc.method, tc.url, tc.cookies))
		assert.False(t, e.Cacheable)
		assert.Equal(t, tc.reason, e.Steps[len(e.Steps)-1].Detail)
	}

	e := Explain(c, backend, opts, explainRequest(http.MethodGet, "http://evil.example/", ""))
	assert.Equal(t, "Turned away: not one of the hosts", e.Verdict)
	assert.Equal(t, []string{"stopped: Host turned away"}, titles(e.Steps))
}

func TestExplainCached(t *testing.T) {
	c, backend, opts, proxy := explainSetup(t)
	req, err := http.NewRequest(http.MethodGet, proxy+"/about/", nil)
	require.NoError(t, err)
	req.Host = "www.example.com"
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()

	e := Explain(c, backend, opts, explainRequest(http.MethodGet, "http://www.example.com/about/", ""))
	assert.Equal(t, "Cacheable, in the cache now", e.Verdict)
	require.NotNil(t, e.Cached)
	assert.Equal(t, http.StatusOK, e.Cached.StatusCode)
	assert.True(t, e.Cached.Usable)
	assert.Greater(t, e.Cached.FreshFor, 4*time.Minute)

	purge := Purger(c, backend, opts)
	_, err = purge("/about/", false, true)
	require.NoError(t, err)
	e = Explain(c, backend, opts, explainRequest(http.MethodGet, "http://www.example.com/about/", ""))
	assert.Equal(t, "Cacheable, stale: the next request gets it while it's refreshed", e.Verdict)
	assert.True(t, e.Cached.Expired)
}
