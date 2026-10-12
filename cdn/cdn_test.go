package cdn

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAPI records the requests a CDN's API is sent
type fakeAPI struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
	// fail, if set, is the response to the next request
	fail func(w http.ResponseWriter)
}

func newFakeAPI(t *testing.T) *fakeAPI {
	a := &fakeAPI{}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		a.mu.Lock()
		defer a.mu.Unlock()
		a.requests = append(a.requests, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Authorization")+r.Header.Get("AccessKey")+" "+string(body))
		if a.fail != nil {
			fail := a.fail
			a.fail = nil
			fail(w)
			return
		}
		io.WriteString(w, `{"success":true,"errors":[],"messages":[],"result":{"id":"zone"}}`)
	}))
	t.Cleanup(a.Close)
	return a
}

func (a *fakeAPI) sent() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.requests...)
}

func TestCloudflare(t *testing.T) {
	api := newFakeAPI(t)
	log := events.New(0)
	p, err := New(Options{Provider: Cloudflare, Zone: "zone1", Token: "tok", Hosts: []string{"www.example.com"}, Events: log, api: api.URL})
	require.NoError(t, err)

	// WordPress's purges when a post changes
	p.Purge("/hello-world/", false, nil)
	p.Purge("/hello-world/", false, nil)
	p.Purge("/", false, nil)
	p.Purge("/page/", true, []string{"/page/2/"})
	p.Purge("/category/news/", true, nil)
	p.Purge("/category/news/page/2/", false, nil)
	p.Purge("/feed/", false, nil)
	p.Purge("/wp-sitemap", true, []string{"/wp-sitemap-posts-post-1.xml", "/wp-sitemap.xml"})
	p.Purge("/?p=1", false, nil)
	p.Flush()

	assert.Equal(t, []string{
		`POST /zones/zone1/purge_cache Bearer tok {"prefixes":["www.example.com/category/news","www.example.com/page"]}`,
		`POST /zones/zone1/purge_cache Bearer tok {"files":["https://www.example.com/","https://www.example.com/?p=1","https://www.example.com/feed/","https://www.example.com/hello-world/","https://www.example.com/wp-sitemap","https://www.example.com/wp-sitemap-posts-post-1.xml","https://www.example.com/wp-sitemap.xml"]}`,
	}, api.sent(), "whole path segments by prefix, others as Honey's cached paths, and nothing twice")
	require.NotEmpty(t, log.Recent(1))
	assert.Equal(t, "purged 9 URLs and prefixes from Cloudflare", log.Recent(1)[0].Text)

	// Everything
	api.requests = nil
	p.Purge("/a/", false, nil)
	p.Purge("/", true, nil)
	p.Flush()
	assert.Equal(t, []string{`POST /zones/zone1/purge_cache Bearer tok {"hosts":["www.example.com"]}`}, api.sent())

	// In batches of 30
	api.requests = nil
	for i := 0; i < 31; i++ {
		p.Purge("/post-"+strings.Repeat("x", i)+"/", false, nil)
	}
	p.Flush()
	assert.Len(t, api.sent(), 2)
}

func TestCloudflareErrors(t *testing.T) {
	api := newFakeAPI(t)
	log := events.New(0)
	p, err := New(Options{Provider: Cloudflare, Zone: "zone1", Token: "bad", Hosts: []string{"www.example.com"}, Events: log, api: api.URL})
	require.NoError(t, err)
	api.fail = func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`)
	}
	p.Purge("/a/", false, nil)
	p.Flush()
	require.NotEmpty(t, log.Recent(1))
	assert.Equal(t, events.Error, log.Recent(1)[0].Kind)
	assert.Equal(t, "couldn't purge from Cloudflare: Authentication error (10000)", log.Recent(1)[0].Text)

	// Busy: it tries again
	api.requests = nil
	api.fail = func(w http.ResponseWriter) { w.WriteHeader(http.StatusTooManyRequests) }
	p.Purge("/b/", false, nil)
	p.Flush()
	assert.Len(t, api.sent(), 2)
	assert.Contains(t, log.Recent(1)[0].Text, "purged 1 URL or prefix from Cloudflare")
}

func TestBunny(t *testing.T) {
	api := newFakeAPI(t)
	p, err := New(Options{Provider: Bunny, Token: "key", Hosts: []string{"example.com", "www.example.com"}, api: api.URL})
	require.NoError(t, err)
	p.Purge("/hello-world/", false, nil)
	p.Purge("/wp-sitemap", true, []string{"/wp-sitemap.xml"})
	p.Flush()
	assert.Equal(t, []string{
		"POST /purge?async=true&url=https%3A%2F%2Fexample.com%2Fwp-sitemap%2A key ",
		"POST /purge?async=true&url=https%3A%2F%2Fwww.example.com%2Fwp-sitemap%2A key ",
		"POST /purge?async=true&url=https%3A%2F%2Fexample.com%2Fhello-world%2F key ",
		"POST /purge?async=true&url=https%3A%2F%2Fwww.example.com%2Fhello-world%2F key ",
	}, api.sent(), "Bunny purges any prefix")

	api.requests = nil
	for i := 0; i < 60; i++ {
		p.Purge("/post-"+strings.Repeat("x", i)+"/", false, nil)
	}
	p.Flush()
	assert.Equal(t, []string{
		"POST /purge?async=true&url=https%3A%2F%2Fexample.com%2F%2A key ",
		"POST /purge?async=true&url=https%3A%2F%2Fwww.example.com%2F%2A key ",
	}, api.sent(), "too many: the whole site")
}

func TestPurgesAreCollected(t *testing.T) {
	api := newFakeAPI(t)
	p, err := New(Options{Provider: Cloudflare, Zone: "z", Token: "t", Hosts: []string{"h"}, Delay: 50 * time.Millisecond, api: api.URL})
	require.NoError(t, err)
	p.Purge("/a/", false, nil)
	p.Purge("/b/", false, nil)
	assert.Empty(t, api.sent())
	require.Eventually(t, func() bool { return len(api.sent()) == 1 }, 5*time.Second, 10*time.Millisecond)
	var body map[string][]string
	require.NoError(t, json.Unmarshal([]byte(api.sent()[0][strings.Index(api.sent()[0], "{"):]), &body))
	assert.Equal(t, []string{"https://h/a/", "https://h/b/"}, body["files"])
}

func TestNew(t *testing.T) {
	for name, opts := range map[string]Options{
		"no token":    {Provider: Cloudflare, Zone: "z", Hosts: []string{"h"}},
		"no zone":     {Provider: Cloudflare, Token: "t", Hosts: []string{"h"}},
		"no hosts":    {Provider: Bunny, Token: "t"},
		"unknown":     {Provider: "akamai", Token: "t", Hosts: []string{"h"}},
		"no provider": {Token: "t", Hosts: []string{"h"}},
	} {
		_, err := New(opts)
		assert.Error(t, err, name)
	}
	var p *Purger
	p.Purge("/", true, nil)
	p.Flush()
}

func TestProtect(t *testing.T) {
	h := Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cc := r.URL.Query().Get("cc"); cc != "none" {
			w.Header().Set("Cache-Control", cc)
		}
		w.Header().Set("Expires", "Mon, 12 Oct 2026 12:00:00 GMT")
		w.Header().Set("X-Honey-Cache", r.URL.Query().Get("cache"))
		io.WriteString(w, "page")
	}))
	for _, test := range []struct{ cache, cc, want string }{
		{"HIT", "public, max-age=300", "public, max-age=300"},
		{"MISS", "public, max-age=300", "public, max-age=300"},
		{"STALE", "public, max-age=300", "no-cache"},
		{"NO-CACHE", "max-age=300", "private, max-age=300"},
		{"NO-CACHE", "none", "private"},
		{"NO-CACHE", "no-cache, must-revalidate, max-age=0, no-store, private", "no-cache, must-revalidate, max-age=0, no-store, private"},
		{"BLOCKED", "none", "private"},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/?"+url.Values{"cache": {test.cache}, "cc": {test.cc}}.Encode(), nil))
		assert.Equal(t, test.want, w.Header().Get("Cache-Control"), "%s %s", test.cache, test.cc)
		assert.Equal(t, "page", w.Body.String())
	}
}
