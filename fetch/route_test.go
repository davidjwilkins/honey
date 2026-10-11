package fetch

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func durationPtr(d time.Duration) *time.Duration { return &d }
func paramsPtr(p ...string) *[]string            { return &p }

// routeProxy proxies to o with a cacher with the given options and routes
func routeProxy(t *testing.T, o *origin, opts cache.Options, fetchOpts Options, routes ...cache.Route) *httptest.Server {
	backend, err := url.Parse(o.URL)
	require.NoError(t, err)
	c := cache.NewCacher(opts)
	for _, route := range routes {
		c.AddRoute(route)
	}
	proxy := httptest.NewServer(FetchWithOptions(c, Forwarder(c), backend, fetchOpts))
	t.Cleanup(proxy.Close)
	t.Cleanup(o.Close)
	return proxy
}

// noHeadersOrigin responds without any caching headers, like WordPress
func noHeadersOrigin() *origin {
	return newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		io.WriteString(w, r.URL.RequestURI()+" v"+strconv.Itoa(hit))
	})
}

func TestRouteDefaultTTL(t *testing.T) {
	o := noHeadersOrigin()
	proxy := routeProxy(t, o, cache.Options{DefaultTTL: time.Second}, Options{},
		cache.Route{Prefix: "/static/", DefaultTTL: time.Hour})

	resp, _ := get(t, proxy.URL+"/page")
	assert.Equal(t, "public, max-age=1", resp.Header.Get("Cache-Control"))
	resp, _ = get(t, proxy.URL+"/static/app.css")
	assert.Equal(t, "public, max-age=3600", resp.Header.Get("Cache-Control"))

	time.Sleep(1100 * time.Millisecond)
	resp, _ = get(t, proxy.URL+"/page")
	assert.Equal(t, "MISS", resp.Header.Get("X-Honey-Cache"), "the site-wide TTL has passed")
	resp, _ = get(t, proxy.URL+"/static/app.css")
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"), "the route's TTL hasn't")
}

func TestRouteStaleIfError(t *testing.T) {
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		if hit > 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Cache-Control", "max-age=1")
		io.WriteString(w, r.URL.Path)
	})
	proxy := routeProxy(t, o, cache.Options{StaleIfError: time.Minute}, Options{},
		cache.Route{Prefix: "/checkout", StaleIfError: durationPtr(0)})

	get(t, proxy.URL+"/page")
	get(t, proxy.URL+"/checkout")
	time.Sleep(1100 * time.Millisecond)
	resp, body := get(t, proxy.URL+"/page")
	assert.Equal(t, "STALE", resp.Header.Get("X-Honey-Cache"))
	assert.Equal(t, "/page", body)
	resp, _ = get(t, proxy.URL+"/checkout")
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode, "the route turns serving stale off")
}

func TestRouteQueryParams(t *testing.T) {
	o := newEchoOrigin()
	proxy := routeProxy(t, o.origin, cache.Options{}, Options{QueryParams: []string{"p"}},
		cache.Route{Prefix: "/search", QueryParams: paramsPtr("s", "page")})

	_, body := get(t, proxy.URL+"/search?s=honey&page=2&utm_source=x&p=1")
	assert.Equal(t, "/search?page=2&s=honey", body, "the route's parameters replace the site-wide ones")
	_, body = get(t, proxy.URL+"/post?s=honey&p=1")
	assert.Equal(t, "/post?p=1", body)
}

func TestRouteMatchedBeforeRewriting(t *testing.T) {
	o := noHeadersOrigin()
	// utm_source is removed from the request, but the route still applies
	proxy := routeProxy(t, o, cache.Options{}, Options{QueryParams: []string{}},
		cache.Route{Regex: regexp.MustCompile(`[?&]utm_source=`), DefaultTTL: time.Hour})

	resp, body := get(t, proxy.URL+"/landing?utm_source=newsletter")
	assert.Equal(t, "/landing v1", body)
	assert.Equal(t, "public, max-age=3600", resp.Header.Get("Cache-Control"))
}

func TestRouteFirstMatchWinsButNoCacheAlwaysApplies(t *testing.T) {
	o := noHeadersOrigin()
	proxy := routeProxy(t, o, cache.Options{}, Options{},
		cache.Route{Prefix: "/blog/", DefaultTTL: time.Hour},
		cache.Route{Prefix: "/", DefaultTTL: time.Minute},
		cache.Route{Prefix: "/wp-admin", NoCache: true})

	resp, _ := get(t, proxy.URL+"/blog/post")
	assert.Equal(t, "public, max-age=3600", resp.Header.Get("Cache-Control"))
	resp, _ = get(t, proxy.URL+"/about")
	assert.Equal(t, "public, max-age=60", resp.Header.Get("Cache-Control"))
	get(t, proxy.URL+"/wp-admin/")
	resp, _ = get(t, proxy.URL+"/wp-admin/")
	assert.Equal(t, "NO-CACHE", resp.Header.Get("X-Honey-Cache"), "cache = false applies even after a broader route")
}

func TestRouteAppliesToBackgroundRevalidation(t *testing.T) {
	// The backend allows stale-while-revalidate but gives no max-age, so the
	// route's TTL applies.  The route matches a parameter which is removed
	// from the request, so the background refresh must carry the route
	// rather than match it again.
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		w.Header().Set("Cache-Control", "stale-while-revalidate=30")
		io.WriteString(w, "v"+strconv.Itoa(hit))
	})
	proxy := routeProxy(t, o, cache.Options{DefaultTTL: time.Hour}, Options{QueryParams: []string{}},
		cache.Route{Regex: regexp.MustCompile(`[?&]utm_source=`), DefaultTTL: time.Second})

	get(t, proxy.URL+"/landing?utm_source=a")
	time.Sleep(1100 * time.Millisecond)
	resp, body := get(t, proxy.URL+"/landing?utm_source=b")
	assert.Equal(t, "STALE", resp.Header.Get("X-Honey-Cache"))
	assert.Equal(t, "v1", body)

	// wait for the background refresh
	require.Eventually(t, func() bool { return o.Hits() == 2 }, 2*time.Second, 10*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	resp, body = get(t, proxy.URL+"/landing?utm_source=c")
	assert.Equal(t, "v2", body)
	assert.Contains(t, resp.Header.Get("Cache-Control"), "max-age=1", "the refreshed response should still get the route's TTL")
}
