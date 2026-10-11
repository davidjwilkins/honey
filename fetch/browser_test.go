package fetch

import (
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stringPtr(s string) *string { return &s }

const immutable = "public, max-age=31536000, immutable"

func TestBrowserCacheControl(t *testing.T) {
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		if r.URL.Path == "/assets/missing.css" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "max-age=600")
		io.WriteString(w, r.URL.Path)
	})
	proxy := routeProxy(t, o, cache.Options{BrowserCacheControl: "no-cache"}, Options{},
		cache.Route{Prefix: "/assets/", BrowserCacheControl: stringPtr(immutable)},
		cache.Route{Prefix: "/feed/", BrowserCacheControl: stringPtr("")})

	for _, want := range []string{"MISS", "HIT"} {
		resp, _ := get(t, proxy.URL+"/assets/app.css")
		assert.Equal(t, want, resp.Header.Get("X-Honey-Cache"))
		assert.Equal(t, immutable, resp.Header.Get("Cache-Control"), want)
		assert.Empty(t, resp.Header.Get("Age"), "browsers should keep it for max-age from now")
		assert.Empty(t, resp.Header.Get("Expires"))

		resp, _ = get(t, proxy.URL+"/page")
		assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"), "the site-wide setting applies elsewhere")

		resp, _ = get(t, proxy.URL+"/feed/")
		assert.Equal(t, "max-age=600, public", resp.Header.Get("Cache-Control"), "the route inherits the cached response's")
	}

	resp, _ := get(t, proxy.URL+"/assets/app.css", "If-None-Match", `"nope"`)
	etag := resp.Header.Get("Etag")
	resp, _ = get(t, proxy.URL+"/assets/app.css", "If-None-Match", etag)
	assert.Equal(t, http.StatusNotModified, resp.StatusCode)
	assert.Equal(t, immutable, resp.Header.Get("Cache-Control"), "a 304 updates what the browser has")

	resp, _ = get(t, proxy.URL+"/assets/missing.css")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.NotContains(t, resp.Header.Get("Cache-Control"), "immutable", "errors aren't kept")
}

func TestBrowserCacheControlNotForUncachedResponses(t *testing.T) {
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		w.Header().Set("Cache-Control", r.URL.Query().Get("cc"))
		io.WriteString(w, "hello")
	})
	proxy := routeProxy(t, o, cache.Options{BrowserCacheControl: immutable, BypassCookies: []string{"session"}}, Options{},
		cache.Route{Prefix: "/admin", NoCache: true})

	for _, tc := range []struct{ path, cookie string }{
		{"/page?cc=private", ""},
		{"/page?cc=no-store", ""},
		{"/admin?cc=max-age%3D600", ""},
		{"/page?cc=max-age%3D600", "session=1"},
	} {
		resp, _ := get(t, proxy.URL+tc.path, "Cookie", tc.cookie)
		assert.NotContains(t, resp.Header.Get("Cache-Control"), "immutable", tc.path+" "+tc.cookie)
	}
}

func TestBrowserCacheControlNotForStaleResponses(t *testing.T) {
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		w.Header().Set("Cache-Control", "max-age=1, stale-while-revalidate=60")
		io.WriteString(w, "hello")
	})
	proxy := routeProxy(t, o, cache.Options{BrowserCacheControl: immutable}, Options{})

	get(t, proxy.URL+"/")
	time.Sleep(1100 * time.Millisecond)
	resp, _ := get(t, proxy.URL+"/")
	require.Equal(t, "STALE", resp.Header.Get("X-Honey-Cache"))
	assert.Equal(t, "max-age=1, stale-while-revalidate=60, public", resp.Header.Get("Cache-Control"),
		"browsers shouldn't keep a stale response")
}

func TestBrowserCacheControlForMultiplexedResponses(t *testing.T) {
	release := make(chan struct{})
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		<-release
		io.WriteString(w, "hello")
	})
	proxy := routeProxy(t, o, cache.Options{BrowserCacheControl: "no-cache"}, Options{})

	var wg sync.WaitGroup
	results := make(chan [2]string, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _ := get(t, proxy.URL+"/")
			results <- [2]string{resp.Header.Get("X-Honey-Cache"), resp.Header.Get("Cache-Control")}
		}()
	}
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
	close(results)
	multiplexed := 0
	for result := range results {
		if result[0] == "MISS (MULTIPLEXED)" {
			multiplexed++
		}
		assert.Equal(t, "no-cache", result[1], result[0])
	}
	assert.Equal(t, 2, multiplexed)
}
