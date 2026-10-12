package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/cdn"
	"github.com/davidjwilkins/honey/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandlerCDN(t *testing.T) {
	var mu sync.Mutex
	var purges []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		purges = append(purges, r.URL.Path+" "+string(body))
		mu.Unlock()
		io.WriteString(w, `{"success":true}`)
	}))
	defer api.Close()
	saved := cdn.CloudflareAPI
	cdn.CloudflareAPI = api.URL
	defer func() { cdn.CloudflareAPI = saved }()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<p>"+r.URL.Path+"</p>")
	}))
	defer origin.Close()
	cfg, err := config.Parse(`
hosts = ["www.example.com"]
[backend]
uri = "` + origin.URL + `"
[cache]
defaultTTL = "1h"
staleWhileRevalidate = "1h"
[control]
secret = "correct-horse-battery-staple"
[cdn]
provider = "cloudflare"
zone = "z1"
token = "tok"
`)
	require.NoError(t, err)
	h := build(cfg, time.Now())
	send := func(method, path string, headers ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://www.example.com"+path, nil)
		for i := 0; i+1 < len(headers); i += 2 {
			r.Header.Set(headers[i], headers[i+1])
		}
		w := httptest.NewRecorder()
		h.handler.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/hello/", "/wp-sitemap.xml", "/wp-sitemap-posts-post-1.xml"} {
		send(http.MethodGet, path)
	}

	send("PURGE", "/hello/", "X-Honey-Secret", "correct-horse-battery-staple", "X-Honey-Purge", "soft")
	send("PURGE", "/wp-sitemap*", "X-Honey-Secret", "correct-horse-battery-staple")
	h.cdn.Flush()
	mu.Lock()
	assert.Equal(t, []string{`/zones/z1/purge_cache {"files":["https://www.example.com/hello/","https://www.example.com/wp-sitemap","https://www.example.com/wp-sitemap-posts-post-1.xml","https://www.example.com/wp-sitemap.xml"]}`}, purges,
		"Honey's purges are passed on, with the sitemaps Honey had cached")
	mu.Unlock()

	// The soft purged page is served once more while it's refreshed, but
	// the CDN mustn't keep it
	w := send(http.MethodGet, "/hello/")
	assert.Equal(t, "STALE", w.Header().Get("X-Honey-Cache"))
	assert.Equal(t, "no-cache", w.Header().Get("Cache-Control"))
	require.Eventually(t, func() bool { return send(http.MethodGet, "/hello/").Header().Get("X-Honey-Cache") == "HIT" }, 5*time.Second, 10*time.Millisecond)
	assert.True(t, strings.HasPrefix(send(http.MethodGet, "/hello/").Header().Get("Cache-Control"), "public"))
}

func TestHandlerCDNPersonalPages(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<p>Hello, "+r.Header.Get("Cookie")+"</p>")
	}))
	defer origin.Close()
	cfg, err := config.Parse(`
hosts = ["www.example.com"]
[backend]
uri = "` + origin.URL + `"
[cdn]
provider = "bunny"
token = "key"
`)
	require.NoError(t, err)
	h := newHandler(cfg)
	r := httptest.NewRequest(http.MethodGet, "http://www.example.com/", nil)
	r.Header.Set("Cookie", "wordpress_logged_in_abc=admin")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	assert.Equal(t, "NO-CACHE", w.Header().Get("X-Honey-Cache"))
	assert.Equal(t, "private", w.Header().Get("Cache-Control"), "WordPress didn't say the logged in user's page is personal, so Honey does")
}
