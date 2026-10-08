package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/davidjwilkins/honey/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandlerAppliesConfig(t *testing.T) {
	var hits int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.SetCookie(w, &http.Cookie{Name: "site_lang_id", Value: "1"})
		http.SetCookie(w, &http.Cookie{Name: "tracking", Value: "1"})
		io.WriteString(w, "hello")
	}))
	defer origin.Close()

	cfg, err := config.Parse(`
[backend]
uri = "` + origin.URL + `"

[cache]
allowedCookies = ["site_lang_id"]

[[route]]
match = "/wp-admin"
cache = false
`)
	require.NoError(t, err)
	proxy := httptest.NewServer(newHandler(cfg))
	defer proxy.Close()

	get := func(path string) *http.Response {
		resp, err := http.Get(proxy.URL + path)
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		assert.Equal(t, "hello", string(body))
		return resp
	}

	resp := get("/page")
	assert.Equal(t, "MISS", resp.Header.Get("X-Honey-Cache"))
	require.Len(t, resp.Cookies(), 1, "only allowed cookies should pass through the cache")
	assert.Equal(t, "site_lang_id", resp.Cookies()[0].Name)
	assert.Equal(t, "HIT", get("/page").Header.Get("X-Honey-Cache"))

	assert.Equal(t, "NO-CACHE", get("/wp-admin/edit.php").Header.Get("X-Honey-Cache"))
	assert.Equal(t, "NO-CACHE", get("/wp-admin/edit.php").Header.Get("X-Honey-Cache"))
	assert.Equal(t, int32(3), atomic.LoadInt32(&hits))

	get("/style.css")
	assert.Equal(t, "HIT", get("/style.css").Header.Get("X-Honey-Cache"), "static files should be cached by default")
}

func TestHandlerCanSkipStaticFiles(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello")
	}))
	defer origin.Close()
	cfg, err := config.Parse("[backend]\nuri = \"" + origin.URL + "\"\n[cache]\nstaticFiles = false\n")
	require.NoError(t, err)
	proxy := httptest.NewServer(newHandler(cfg))
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/style.css")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, "NO-CACHE", resp.Header.Get("X-Honey-Cache"))
}
