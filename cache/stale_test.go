package cache

import (
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStaleWhileRevalidateIsAdded(t *testing.T) {
	off := time.Duration(0)
	week := 7 * 24 * time.Hour
	c := NewCacher(Options{StaleWhileRevalidate: 24 * time.Hour})
	c.AddRoute(Route{Prefix: "/checkout", StaleWhileRevalidate: &off})
	c.AddRoute(Route{Prefix: "/uploads/", StaleWhileRevalidate: &week})
	for _, tc := range []struct{ path, cacheControl, want string }{
		{"/page", "", "public, max-age=300, stale-while-revalidate=86400"},
		{"/page", "max-age=60, stale-while-revalidate=5", "max-age=60, stale-while-revalidate=5, public"},
		{"/page", "no-cache", "no-cache, public"},
		{"/checkout", "", "public, max-age=300"},
		{"/uploads/cat.jpg", "", "public, max-age=300, stale-while-revalidate=604800"},
	} {
		header := http.Header{}
		if tc.cacheControl != "" {
			header.Set("Cache-Control", tc.cacheControl)
		}
		request := newValidRequest("https://www.example.com" + tc.path)
		resp := c.Standardize(response(request, header))
		assert.Equal(t, tc.want, resp.Header().Get("Cache-Control"), "%s %s", tc.path, tc.cacheControl)
	}
}

func TestExpire(t *testing.T) {
	c := NewCacher(Options{})
	header := http.Header{"Cache-Control": {"max-age=600"}, "Vary": {"Accept-Language"}}
	en := cachePage(t, c, "https://www.example.com/blog/post", "hello", header, "Accept-Language", "en")
	fr := cachePage(t, c, "https://www.example.com/blog/post", "bonjour", header, "Accept-Language", "fr")
	other := cachePage(t, c, "https://www.example.com/about", "about", http.Header{"Cache-Control": {"max-age=600"}})

	u, _ := url.Parse("http://backend/blog/")
	assert.Equal(t, 2, c.Expire(u, true), "both variants")
	assert.Equal(t, 0, c.Expire(u, true), "they're already expired")
	for _, r := range []*http.Request{en, fr} {
		assert.True(t, load(t, c, r).Expired(), "kept, but expired")
	}
	assert.False(t, load(t, c, other).Expired())

	// and that survives a restart
	path := filepath.Join(t.TempDir(), "cache.snapshot")
	_, err := c.SaveTo(path)
	require.NoError(t, err)
	restored := NewCacher(Options{})
	_, err = restored.LoadFrom(path)
	require.NoError(t, err)
	assert.True(t, load(t, restored, en).Expired())
	assert.False(t, load(t, restored, other).Expired())
}
