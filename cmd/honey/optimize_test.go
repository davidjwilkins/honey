package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/davidjwilkins/honey/config"
	"github.com/davidjwilkins/honey/optimize"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandlerOptimize(t *testing.T) {
	var renders atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		renders.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		io.WriteString(w, `<!DOCTYPE html><html><head><meta charset="UTF-8"><title>Post</title>
<script async src="https://www.googletagmanager.com/gtag/js?id=G-1"></script></head>
<body><img src="/wp-content/uploads/hero.jpg" loading="lazy" alt=""><p>Hello</p></body></html>`)
	}))
	defer origin.Close()
	cfg, err := config.Parse(`
hosts = ["www.example.com"]
[backend]
uri = "` + origin.URL + `"
[cache]
defaultTTL = "1h"
staleWhileRevalidate = "1h"
earlyHints = true
[optimize]
delayScripts = true
preloadMainImage = true
`)
	require.NoError(t, err)
	h := newHandler(cfg)
	get := func() (*httptest.ResponseRecorder, string) {
		r := httptest.NewRequest(http.MethodGet, "http://www.example.com/post/", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w, w.Body.String()
	}

	w, body := get()
	assert.Equal(t, "MISS", w.Header().Get("X-Honey-Cache"))
	assert.Contains(t, body, `<script type="text/honey-delayed" data-honey-src="https://www.googletagmanager.com/gtag/js?id=G-1" async></script>`, "even the first visitor gets the optimized page")
	assert.Contains(t, body, `<script id="honey-delay">`)
	assert.Contains(t, body, `<script id="honey-main-image">`)

	// A desktop browser reports the main image
	for _, viewport := range []string{"desktop", "mobile"} {
		r := httptest.NewRequest(http.MethodPost, "http://www.example.com"+optimize.BeaconPath,
			strings.NewReader(`{"p":"/post/","v":"`+viewport+`","u":"http://www.example.com/wp-content/uploads/hero.jpg"}`))
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		assert.Equal(t, http.StatusNoContent, w.Code)
	}

	// The page is refreshed in the background, while the old one is served
	w, _ = get()
	assert.Equal(t, "STALE", w.Header().Get("X-Honey-Cache"))
	require.Eventually(t, func() bool { return renders.Load() == 2 }, 5e9, 1e7)
	require.Eventually(t, func() bool {
		_, body = get()
		return strings.Contains(body, "data-honey-main-image")
	}, 5e9, 1e7)
	assert.Contains(t, body, `<meta charset="UTF-8"><link rel="preload" as="image" href="/wp-content/uploads/hero.jpg" fetchpriority="high" data-honey-main-image>`)
	assert.Contains(t, body, `<img fetchpriority="high" src="/wp-content/uploads/hero.jpg" alt="">`)
	assert.NotContains(t, body, `<script id="honey-main-image">`)
}

func TestHandlerOptimizeWithBackendHost(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<html><head></head><body><img src="https://www.example.com/hero.jpg"></body></html>`)
	}))
	defer origin.Close()
	cfg, err := config.Parse(`
[backend]
uri = "` + origin.URL + `"
host = "wordpress.internal"
[cache]
staleWhileRevalidate = "1h"
[optimize]
preloadMainImage = true
`)
	require.NoError(t, err)
	h := newHandler(cfg)
	get := func() string {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://www.example.com/post/", nil))
		return w.Body.String()
	}
	assert.Contains(t, get(), "honey-main-image")
	for _, viewport := range []string{"desktop", "mobile"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "http://www.example.com"+optimize.BeaconPath,
			strings.NewReader(`{"p":"/post/","v":"`+viewport+`","u":"https://www.example.com/hero.jpg"}`)))
	}
	require.Eventually(t, func() bool { return strings.Contains(get(), "data-honey-main-image") }, 5e9, 1e7,
		"it's learned for the page as it's cached, under the backend's Host")
}
