package fetch

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// origin is a test backend which counts its requests
type origin struct {
	*httptest.Server
	hits    int32
	handler func(w http.ResponseWriter, r *http.Request, hit int)
}

func newOrigin(handler func(w http.ResponseWriter, r *http.Request, hit int)) *origin {
	o := &origin{handler: handler}
	o.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.handler(w, r, int(atomic.AddInt32(&o.hits, 1)))
	}))
	return o
}

func (o *origin) Hits() int { return int(atomic.LoadInt32(&o.hits)) }

func newProxy(t *testing.T, o *origin) *httptest.Server {
	return newProxyWith(t, o, cache.Options{})
}

func newProxyWith(t *testing.T, o *origin, opts cache.Options) *httptest.Server {
	backend, err := url.Parse(o.URL)
	require.NoError(t, err)
	c := cache.NewCacher(opts)
	proxy := httptest.NewServer(Fetch(c, Forwarder(c), backend))
	t.Cleanup(proxy.Close)
	t.Cleanup(o.Close)
	return proxy
}

func get(t *testing.T, u string, headers ...string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, u, nil)
	require.NoError(t, err)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	return resp, string(body)
}

func TestIntegrationCachesUntilExpiry(t *testing.T) {
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		w.Header().Set("Cache-Control", "max-age=1")
		io.WriteString(w, "v"+strconv.Itoa(hit))
	})
	proxy := newProxy(t, o)

	resp, body := get(t, proxy.URL+"/page")
	assert.Equal(t, "v1", body)
	assert.Equal(t, "MISS", resp.Header.Get("X-Honey-Cache"))
	assert.Contains(t, resp.Header.Get("Cache-Control"), "public")

	resp, body = get(t, proxy.URL+"/page")
	assert.Equal(t, "v1", body)
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"))
	assert.NotEmpty(t, resp.Header.Get("Last-Modified"), "cached responses should keep Last-Modified")
	assert.NotEmpty(t, resp.Header.Get("Expires"), "cached responses should keep Expires")
	assert.Equal(t, 1, o.Hits())

	time.Sleep(1100 * time.Millisecond)
	resp, body = get(t, proxy.URL+"/page")
	assert.Equal(t, "v2", body, "expired responses should be fetched again")
	assert.Equal(t, "MISS", resp.Header.Get("X-Honey-Cache"))
	assert.Equal(t, 2, o.Hits())
}

func TestIntegrationMultiplexesConcurrentMisses(t *testing.T) {
	release := make(chan struct{})
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		<-release
		w.Header().Set("Cache-Control", "max-age=60")
		io.WriteString(w, "v"+strconv.Itoa(hit))
	})
	proxy := newProxy(t, o)

	var wg sync.WaitGroup
	bodies := make([]string, 10)
	for i := range bodies {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, bodies[i] = get(t, proxy.URL+"/page")
		}(i)
	}
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
	for _, body := range bodies {
		assert.Equal(t, "v1", body)
	}
	assert.Equal(t, 1, o.Hits(), "concurrent misses should make a single backend request")
}

func TestIntegrationStaleWhileRevalidate(t *testing.T) {
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		if hit > 1 {
			// a slow refresh shouldn't hold up the stale response
			time.Sleep(500 * time.Millisecond)
		}
		w.Header().Set("Cache-Control", "max-age=1, stale-while-revalidate=30")
		io.WriteString(w, "v"+strconv.Itoa(hit))
	})
	proxy := newProxy(t, o)

	get(t, proxy.URL+"/page")
	time.Sleep(1100 * time.Millisecond)

	start := time.Now()
	resp, body := get(t, proxy.URL+"/page")
	assert.Less(t, time.Since(start), 400*time.Millisecond, "stale content should be served without waiting for the backend")
	assert.Equal(t, "v1", body)
	assert.Equal(t, "STALE", resp.Header.Get("X-Honey-Cache"))

	time.Sleep(700 * time.Millisecond)
	resp, body = get(t, proxy.URL+"/page")
	assert.Equal(t, "v2", body, "the cache should have been refreshed in the background")
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"))
	assert.Equal(t, 2, o.Hits())
}

func TestIntegrationStaleIfErrorOnServerError(t *testing.T) {
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		if hit > 1 {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, "broken")
			return
		}
		w.Header().Set("Cache-Control", "max-age=1, stale-if-error=60")
		io.WriteString(w, "v1")
	})
	proxy := newProxy(t, o)

	get(t, proxy.URL+"/page")
	time.Sleep(1100 * time.Millisecond)
	resp, body := get(t, proxy.URL+"/page")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "v1", body, "the stale response should be served instead of the error")
	assert.Equal(t, "STALE", resp.Header.Get("X-Honey-Cache"))
	assert.NotEmpty(t, resp.Header.Get("Warning"))
}

func TestIntegrationStaleIfErrorWhenBackendDown(t *testing.T) {
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		w.Header().Set("Cache-Control", "max-age=1, stale-if-error=60")
		io.WriteString(w, "v1")
	})
	proxy := newProxy(t, o)

	get(t, proxy.URL+"/page")
	o.Close()
	time.Sleep(1100 * time.Millisecond)
	resp, body := get(t, proxy.URL+"/page")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "v1", body, "the stale response should be served when the backend is unreachable")
	assert.Equal(t, "STALE", resp.Header.Get("X-Honey-Cache"))
}

func TestIntegrationBackendDownDoesNotHang(t *testing.T) {
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {})
	proxy := newProxy(t, o)
	o.Close()

	for i := 0; i < 3; i++ {
		resp, _ := get(t, proxy.URL+"/page")
		assert.Equal(t, http.StatusBadGateway, resp.StatusCode, "request %d", i)
	}
}

func TestIntegrationVaryStaysCachedAfterRefresh(t *testing.T) {
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		w.Header().Set("Cache-Control", "max-age=1")
		w.Header().Set("Vary", "Accept-Language")
		io.WriteString(w, r.Header.Get("Accept-Language")+strconv.Itoa(hit))
	})
	proxy := newProxy(t, o)

	_, body := get(t, proxy.URL+"/page", "Accept-Language", "en")
	assert.Equal(t, "en1", body)
	// The first response with a Vary header teaches the cache to key on it,
	// so this is a miss; from then on it is cached per language.
	_, body = get(t, proxy.URL+"/page", "Accept-Language", "fr")
	assert.Equal(t, "fr2", body)
	_, body = get(t, proxy.URL+"/page", "Accept-Language", "fr")
	assert.Equal(t, "fr2", body)

	time.Sleep(1100 * time.Millisecond)
	_, body = get(t, proxy.URL+"/page", "Accept-Language", "fr")
	assert.Equal(t, "fr3", body)
	resp, body := get(t, proxy.URL+"/page", "Accept-Language", "fr")
	assert.Equal(t, "fr3", body, "refreshed responses should be cached under the same key")
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"))
}

// fileOrigin is a backend which honours conditional and range requests,
// like a real web server serving a file.
func fileOrigin() *origin {
	modified := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		w.Header().Set("Cache-Control", "max-age=60")
		http.ServeContent(w, r, "page.html", modified, strings.NewReader("full page body"))
	})
}

func TestIntegrationConditionalMissDoesNotCacheNotModified(t *testing.T) {
	o := fileOrigin()
	proxy := newProxy(t, o)

	// A browser with an old copy revalidates while the cache is empty
	resp, body := get(t, proxy.URL+"/page", "If-Modified-Since", "Thu, 01 Jan 2026 00:00:00 GMT")
	assert.Equal(t, http.StatusNotModified, resp.StatusCode, "the client's copy is still current")
	assert.Equal(t, "", body)

	// A browser with no copy must get the full page, not the empty 304
	resp, body = get(t, proxy.URL+"/page")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "full page body", body)
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"))
	assert.Equal(t, 1, o.Hits())

	// A conditional request for an older copy gets the full page
	resp, body = get(t, proxy.URL+"/page", "If-Modified-Since", "Wed, 31 Dec 2025 00:00:00 GMT")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "full page body", body)
}

func TestIntegrationRangeMissPassesThrough(t *testing.T) {
	o := fileOrigin()
	proxy := newProxy(t, o)

	// Range requests for something not yet cached go straight to the
	// backend, so that seeking in large media still works
	resp, body := get(t, proxy.URL+"/page", "Range", "bytes=0-3")
	assert.Equal(t, http.StatusPartialContent, resp.StatusCode)
	assert.Equal(t, "full", body)
	assert.Equal(t, "NO-CACHE", resp.Header.Get("X-Honey-Cache"))

	resp, body = get(t, proxy.URL+"/page")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "full page body", body, "a partial response must not be cached as the whole page")
	assert.Equal(t, "MISS", resp.Header.Get("X-Honey-Cache"))
	assert.Equal(t, 2, o.Hits())
}

func TestIntegrationRangeServedFromCache(t *testing.T) {
	o := fileOrigin()
	proxy := newProxy(t, o)

	get(t, proxy.URL+"/video.mp4")
	resp, body := get(t, proxy.URL+"/video.mp4", "Range", "bytes=5-8")
	assert.Equal(t, http.StatusPartialContent, resp.StatusCode)
	assert.Equal(t, "page", body)
	assert.Equal(t, "bytes 5-8/14", resp.Header.Get("Content-Range"))
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"))

	// If-Range with a different validator means the client's copy is out
	// of date, so it gets the whole thing
	resp, body = get(t, proxy.URL+"/video.mp4", "Range", "bytes=5-8", "If-Range", `"something-else"`)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "full page body", body)
	assert.Equal(t, 1, o.Hits())
}

func TestIntegrationCachesStaticFiles(t *testing.T) {
	o := fileOrigin()
	proxy := newProxy(t, o)

	for _, path := range []string{"/style.css", "/app.js", "/logo.png"} {
		get(t, proxy.URL+path)
		resp, body := get(t, proxy.URL+path)
		assert.Equal(t, "full page body", body, path)
		assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"), path)
	}
	assert.Equal(t, 3, o.Hits())
}

func TestIntegrationSkipStaticFiles(t *testing.T) {
	o := fileOrigin()
	proxy := newProxyWith(t, o, cache.Options{SkipStaticFiles: true})

	get(t, proxy.URL+"/style.css")
	resp, _ := get(t, proxy.URL+"/style.css")
	assert.Equal(t, "NO-CACHE", resp.Header.Get("X-Honey-Cache"))
	assert.Equal(t, 2, o.Hits())
}

func TestIntegrationLargeResponsesStreamUncached(t *testing.T) {
	large := strings.Repeat("x", 4<<10)
	for name, chunked := range map[string]bool{"with Content-Length": false, "chunked": true} {
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
				<-release
				w.Header().Set("Cache-Control", "max-age=60")
				if !chunked {
					w.Header().Set("Content-Length", strconv.Itoa(len(large)))
				}
				for i := 0; i < len(large); i += 1 << 10 {
					io.WriteString(w, large[i:i+1<<10])
					w.(http.Flusher).Flush()
				}
			})
			proxy := newProxyWith(t, o, cache.Options{MaxObjectBytes: 1 << 10})

			// Concurrent requests for a response too large to cache each
			// get the whole of it
			var wg sync.WaitGroup
			bodies := make([]string, 5)
			for i := range bodies {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					_, bodies[i] = get(t, proxy.URL+"/video.mp4")
				}(i)
			}
			time.Sleep(200 * time.Millisecond)
			close(release)
			wg.Wait()
			for _, body := range bodies {
				assert.Equal(t, len(large), len(body))
			}

			resp, body := get(t, proxy.URL+"/video.mp4")
			assert.Equal(t, len(large), len(body))
			assert.Equal(t, "NO-CACHE", resp.Header.Get("X-Honey-Cache"), "it shouldn't have been cached")
			assert.Equal(t, 6, o.Hits())
		})
	}
}

func TestIntegrationRevalidatingWithEtagDoesNotCacheNotModified(t *testing.T) {
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		w.Header().Set("Cache-Control", "max-age=1")
		io.WriteString(w, "full page body")
	})
	proxy := newProxy(t, o)

	resp, _ := get(t, proxy.URL+"/page")
	etag := resp.Header.Get("Etag")
	require.NotEmpty(t, etag)

	// Once it has expired, the browser revalidates with the Etag it was given
	time.Sleep(1100 * time.Millisecond)
	resp, body := get(t, proxy.URL+"/page", "If-None-Match", etag)
	assert.Equal(t, http.StatusNotModified, resp.StatusCode)
	assert.Equal(t, "", body)

	resp, body = get(t, proxy.URL+"/page")
	assert.Equal(t, http.StatusOK, resp.StatusCode, "the 304 for one client must not become the cached status")
	assert.Equal(t, "full page body", body)
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"))
}

func TestIntegrationDoesNotCacheUnexpectedPartialResponses(t *testing.T) {
	// A backend which sends partial or not modified responses even
	// though Honey doesn't ask for them
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		w.Header().Set("Cache-Control", "max-age=60")
		if hit == 1 {
			w.Header().Set("Content-Range", "bytes 0-3/14")
			w.WriteHeader(http.StatusPartialContent)
			io.WriteString(w, "full")
			return
		}
		io.WriteString(w, "full page body")
	})
	proxy := newProxy(t, o)

	get(t, proxy.URL+"/page")
	_, body := get(t, proxy.URL+"/page")
	assert.Equal(t, "full page body", body)
	assert.Equal(t, 2, o.Hits())
}

func TestIntegrationMultiplexedRequestsWithDifferentVaryValues(t *testing.T) {
	release := make(chan struct{})
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		<-release
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("Vary", "Accept-Language")
		io.WriteString(w, "page in "+r.Header.Get("Accept-Language"))
	})
	proxy := newProxy(t, o)

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(language string) {
			defer wg.Done()
			_, body := get(t, proxy.URL+"/page", "Accept-Language", language)
			assert.Equal(t, "page in "+language, body)
		}([]string{"en", "fr"}[i%2])
	}
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
}
