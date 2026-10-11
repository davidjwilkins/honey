package fetch

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fussyOrigin rejects requests with an X-Big header, as a server does a
// request with a header too large for it, and otherwise responds with how
// many times it has been requested
func fussyOrigin(status int) *origin {
	return newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		if r.Header.Get("X-Big") != "" {
			w.WriteHeader(status)
			io.WriteString(w, http.StatusText(status))
			return
		}
		io.WriteString(w, "v"+strconv.Itoa(hit))
	})
}

func TestRequestErrorsAreNotCached(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusRequestHeaderFieldsTooLarge,
		http.StatusTooManyRequests,
		http.StatusFound,
	} {
		o := fussyOrigin(status)
		proxy := newProxy(t, o)

		resp, _ := get(t, proxy.URL+"/", "X-Big", "1")
		assert.Equal(t, status, resp.StatusCode)
		assert.Equal(t, "NO-CACHE", resp.Header.Get("X-Honey-Cache"), status)
		resp, body := get(t, proxy.URL+"/")
		assert.Equal(t, "v2", body, "%d: one visitor's bad request mustn't break the page for everyone", status)
		assert.Equal(t, "MISS", resp.Header.Get("X-Honey-Cache"))
		resp, _ = get(t, proxy.URL+"/")
		assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"))
	}
}

func TestStorableStatus(t *testing.T) {
	for _, tc := range []struct {
		status   int
		header   http.Header
		storable bool
	}{
		{http.StatusOK, nil, true},
		{http.StatusNoContent, nil, true},
		{http.StatusMovedPermanently, nil, true},
		{http.StatusPermanentRedirect, nil, true},
		{http.StatusNotFound, nil, true},
		{http.StatusGone, nil, true},
		{http.StatusFound, nil, false},
		{http.StatusFound, http.Header{"Cache-Control": {"max-age=60"}}, true},
		{http.StatusTemporaryRedirect, http.Header{"Cache-Control": {"public, s-maxage=60"}}, true},
		{http.StatusSeeOther, http.Header{"Expires": {"Thu, 01 Jan 2099 00:00:00 GMT"}}, true},
		{http.StatusSeeOther, http.Header{"Cache-Control": {"public"}}, false},
		{http.StatusPartialContent, nil, false},
		{http.StatusNotModified, nil, false},
		{http.StatusBadRequest, nil, false},
		{http.StatusForbidden, http.Header{"Cache-Control": {"max-age=600"}}, false},
		{http.StatusTooManyRequests, nil, false},
	} {
		header := tc.header
		if header == nil {
			header = http.Header{}
		}
		assert.Equal(t, tc.storable, storableStatus(tc.status, header), "%d %v", tc.status, tc.header)
	}
}

func TestNotFoundIsCached(t *testing.T) {
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		http.NotFound(w, r)
	})
	proxy := newProxy(t, o)
	get(t, proxy.URL+"/nope")
	resp, _ := get(t, proxy.URL+"/nope")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"))
}

func TestRefreshErrorKeepsTheStalePage(t *testing.T) {
	o := fussyOrigin(http.StatusBadRequest)
	proxy := softPurgeProxy(t, o, cache.Options{DefaultTTL: time.Hour, StaleWhileRevalidate: time.Hour})

	get(t, proxy+"/")
	purgeRequest(t, proxy, "/", true)
	// The bad request is the one which refreshes the page, in the background
	resp, body := get(t, proxy+"/", "X-Big", "1")
	assert.Equal(t, "STALE", resp.Header.Get("X-Honey-Cache"))
	assert.Equal(t, "v1", body)
	require.Eventually(t, func() bool { return o.Hits() == 2 }, 3*time.Second, 10*time.Millisecond)

	resp, body = get(t, proxy+"/")
	assert.Equal(t, http.StatusOK, resp.StatusCode, "the error mustn't replace the page")
	assert.Equal(t, "v1", body)
	eventually(t, proxy, "/", "v3")
}

func TestWaitersDontGetTheLeadersError(t *testing.T) {
	release := make(chan struct{})
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		<-release
		if r.Header.Get("X-Big") != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		io.WriteString(w, "page")
	})
	proxy := newProxy(t, o)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, _ := get(t, proxy.URL+"/", "X-Big", "1")
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	}()
	// for the bad request to lead, and the others to wait for it
	time.Sleep(100 * time.Millisecond)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, body := get(t, proxy.URL+"/")
			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, "page", body)
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
}

// unkeyedOrigin responds with the request's theme cookie, language cookie and
// method override (like a theme switcher, a translation plugin which
// doesn't send Vary, and WordPress's REST API)
func unkeyedOrigin() *origin {
	return newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		theme, lang := "default", "en"
		if c, err := r.Cookie("theme"); err == nil {
			theme = c.Value
		}
		if c, err := r.Cookie("lang"); err == nil {
			lang = c.Value
		}
		method := r.Method
		if override := r.Header.Get("X-HTTP-Method-Override"); override != "" {
			method = override
		}
		io.WriteString(w, method+" "+theme+" "+lang)
	})
}

func TestUnknownCookiesDontReachTheBackend(t *testing.T) {
	o := unkeyedOrigin()
	proxy := newProxyWith(t, o, cache.Options{BypassCookies: cache.WordPressBypassCookies})

	_, body := get(t, proxy.URL+"/", "Cookie", "theme=evil; _ga=GA1.2.3")
	assert.Equal(t, "GET default en", body, "a visitor's cookies mustn't decide the page cached for everyone")
	resp, body := get(t, proxy.URL+"/")
	assert.Equal(t, "GET default en", body)
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"))
}

func TestCookiesReachTheBackendWhenNotCaching(t *testing.T) {
	o := unkeyedOrigin()
	proxy := routeProxy(t, o, cache.Options{}, Options{}, cache.Route{Prefix: "/account", NoCache: true})

	_, body := get(t, proxy.URL+"/account", "Cookie", "theme=dark", "X-HTTP-Method-Override", "PUT")
	assert.Equal(t, "PUT dark en", body)
}

func TestAllowedCookiesArePartOfTheKey(t *testing.T) {
	o := unkeyedOrigin()
	c := cache.NewCacher(cache.Options{})
	c.AddAllowedCookie("lang")
	backend, err := url.Parse(o.URL)
	require.NoError(t, err)
	proxy := httptest.NewServer(Fetch(c, Forwarder(c), backend))
	t.Cleanup(proxy.Close)
	t.Cleanup(o.Close)

	_, body := get(t, proxy.URL+"/", "Cookie", "lang=fr; theme=evil")
	assert.Equal(t, "GET default fr", body)
	_, body = get(t, proxy.URL+"/")
	assert.Equal(t, "GET default en", body, "even without Vary: Cookie, the French page is only for the French")
	for cookie, want := range map[string]string{"lang=fr": "GET default fr", "theme=x; lang=fr": "GET default fr", "": "GET default en"} {
		resp, body := get(t, proxy.URL+"/", "Cookie", cookie)
		assert.Equal(t, want, body, cookie)
		assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"), cookie)
	}
	assert.Equal(t, 2, o.Hits())
}

func TestMethodOverridesAreRemoved(t *testing.T) {
	o := unkeyedOrigin()
	proxy := newProxy(t, o)

	for _, header := range []string{"X-HTTP-Method-Override", "X-HTTP-Method", "X-Method-Override", "X-Original-URL", "X-Rewrite-URL"} {
		get(t, proxy.URL+"/wp-json/wp/v2/posts", header, "OPTIONS")
	}
	resp, body := get(t, proxy.URL+"/wp-json/wp/v2/posts")
	assert.Equal(t, "GET default en", body)
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"))
}

func TestRangeRequestsForPagesShareAFlight(t *testing.T) {
	release := make(chan struct{})
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		<-release
		assert.Empty(t, r.Header.Get("Range"))
		io.WriteString(w, "full page body")
	})
	proxy := newProxy(t, o)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, body := get(t, proxy.URL+"/page", "Range", "bytes=0-3")
			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, "full page body", body)
		}()
	}
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
	assert.Equal(t, 1, o.Hits(), "a Range header mustn't be a way around the cache")

	resp, body := get(t, proxy.URL+"/page", "Range", "bytes=0-3")
	assert.Equal(t, http.StatusPartialContent, resp.StatusCode)
	assert.Equal(t, "full", body)
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"))
}
