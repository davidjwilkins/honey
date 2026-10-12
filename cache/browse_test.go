package cache

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEntries(t *testing.T) {
	c := NewCacher(Options{DefaultTTL: time.Minute, StaleWhileRevalidate: time.Hour})
	c.AddAllowedCookie("lang")
	cachePage(t, c, "https://www.example.com/blog/?p=1", "post", http.Header{"Content-Type": {"text/html"}})
	r := cachePage(t, c, "https://www.example.com/about/", "about us", http.Header{"Vary": {"Accept-Language"}}, "Accept-Language", "fr")

	entries := c.Entries()
	require.Len(t, entries, 2, "Vary records aren't entries")
	about := entries[0]
	assert.Equal(t, "/about/", about.Path, "most recently used first")
	assert.Equal(t, "www.example.com", about.Host)
	assert.Equal(t, "GET", about.Method)
	assert.Equal(t, "https://www.example.com/about/", about.URL)
	assert.Equal(t, "::fr", about.Variant)
	assert.Equal(t, http.StatusOK, about.StatusCode)
	assert.Equal(t, len("about us"), about.Bytes)
	assert.Equal(t, time.Minute, about.Lifetime)
	assert.Equal(t, time.Hour, about.Stale)
	assert.True(t, about.Fresh(time.Now()))
	assert.False(t, about.Fresh(time.Now().Add(2*time.Minute)))
	assert.True(t, about.Usable(time.Now().Add(2*time.Minute)))
	assert.Zero(t, about.Hits)
	assert.Equal(t, "/blog/?p=1", entries[1].Path)
	assert.Equal(t, "text/html", entries[1].ContentType)

	e, resp, found := c.Entry(about.Key)
	require.True(t, found)
	assert.Equal(t, about.Key, e.Key)
	assert.Equal(t, "about us", string(resp.Body()))
	assert.Equal(t, http.Header{"Accept-Language": {"fr"}}, RequestHeader(resp))

	loaded, _ := c.Load(c.Hash(r), r)
	loaded.(*responseImpl).CountHit()
	e, _, _ = c.Entry(about.Key)
	assert.Equal(t, int64(1), e.Hits)

	c.Expire(mustParse(t, "/about/"), false)
	e, _, _ = c.Entry(about.Key)
	assert.True(t, e.Expired)
	assert.Zero(t, e.Lifetime, "a soft purged response is stale")

	_, _, found = c.Entry("nope")
	assert.False(t, found)
}

func mustParse(t *testing.T, rawURL string) *url.URL {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	return u
}
