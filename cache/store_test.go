package cache

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestStoreEvictsLeastRecentlyUsed(t *testing.T) {
	s := newStore(30)
	now := time.Now()
	s.set("a", "A", 10, time.Time{})
	s.set("b", "B", 10, time.Time{})
	s.set("c", "C", 10, time.Time{})
	_, found := s.get("a", now) // a is now the most recently used
	assert.True(t, found)
	s.set("d", "D", 10, time.Time{})

	_, found = s.get("b", now)
	assert.False(t, found, "the least recently used entry should be evicted")
	for _, key := range []string{"a", "c", "d"} {
		_, found = s.get(key, now)
		assert.True(t, found, key)
	}
	entries, size := s.stats()
	assert.Equal(t, 3, entries)
	assert.Equal(t, int64(30), size)
}

func TestStoreReplacesExistingKey(t *testing.T) {
	s := newStore(100)
	s.set("a", "A", 10, time.Time{})
	s.set("a", "AA", 20, time.Time{})
	value, _ := s.get("a", time.Now())
	assert.Equal(t, "AA", value)
	entries, size := s.stats()
	assert.Equal(t, 1, entries)
	assert.Equal(t, int64(20), size)
}

func TestStoreDropsEntriesPastDeadline(t *testing.T) {
	s := newStore(100)
	now := time.Now()
	s.set("a", "A", 10, now.Add(time.Minute))
	_, found := s.get("a", now)
	assert.True(t, found)
	_, found = s.get("a", now.Add(2*time.Minute))
	assert.False(t, found)
	entries, size := s.stats()
	assert.Equal(t, 0, entries)
	assert.Equal(t, int64(0), size)
}

func TestStoreIgnoresValuesLargerThanStore(t *testing.T) {
	s := newStore(10)
	s.set("a", "A", 11, time.Time{})
	_, found := s.get("a", time.Now())
	assert.False(t, found)
}

func standardized(c *defaultCacher, cacheControl string, body string) Response {
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Request:    validRequest(),
	}
	if cacheControl != "" {
		response.Header.Set("Cache-Control", cacheControl)
	}
	return c.Standardize(response)
}

func TestDefaultCacheIsBoundedBySize(t *testing.T) {
	c := NewCacher(Options{MaxBytes: 10 << 10})
	body := strings.Repeat("x", 1<<10)
	for i := 0; i < 100; i++ {
		request := newValidRequest("https://www.insomniac.com/page/" + string(rune('a'+i%26)) + string(rune('a'+i/26)))
		response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewBufferString(body)), Request: request}
		c.Cache(c.Hash(request), c.Standardize(response))
	}
	entries, size := c.Stats()
	assert.LessOrEqual(t, size, int64(10<<10))
	assert.Less(t, entries, 10)
	assert.Greater(t, entries, 0)
}

func TestDefaultCacheDoesNotStoreUnservableResponses(t *testing.T) {
	c := NewCacher(Options{})
	request := validRequest()
	c.Cache(c.Hash(request), standardized(c, "no-cache", "test"))
	_, found := c.Load(c.Hash(request), request)
	assert.False(t, found, "a no-cache response without stale-* directives can never be served")
}

func TestDefaultCacheKeepsResponsesForStaleIfError(t *testing.T) {
	c := NewCacher(Options{})
	request := validRequest()
	c.Cache(c.Hash(request), standardized(c, "no-cache, stale-if-error=60", "test"))
	_, found := c.Load(c.Hash(request), request)
	assert.True(t, found, "responses should be kept while stale-if-error allows them to be served")
}

func TestStandardizeFillsInDefaults(t *testing.T) {
	c := NewCacher(Options{DefaultTTL: time.Minute})
	r := standardized(c, "", "test")
	assert.Equal(t, "public, max-age=60", r.Header().Get("Cache-Control"))
	for _, header := range []string{"Date", "Last-Modified", "Expires", "Etag"} {
		assert.NotEmpty(t, r.Header().Get(header), header)
	}
	assert.True(t, strings.HasPrefix(r.Header().Get("Etag"), `"`), "Etags should be quoted")

	r = standardized(c, "private, max-age=10", "test")
	assert.Equal(t, "private, max-age=10", r.Header().Get("Cache-Control"))

	r = standardized(c, `public, no-cache="set-cookie", max-age=10`, "test")
	assert.Equal(t, "public, max-age=10", r.Header().Get("Cache-Control"))
}

func TestPurge(t *testing.T) {
	c := NewCacher(Options{})
	cacheURL := func(method, u string, vary string, cookie string) *http.Request {
		request := newValidRequest(u)
		request.Method = method
		if cookie != "" {
			request.Header.Set("Accept-Language", cookie)
		}
		response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewBufferString("body")), Request: request}
		if vary != "" {
			response.Header.Set("Vary", vary)
		}
		c.Cache(c.Hash(request), c.Standardize(response))
		return request
	}
	cached := func(request *http.Request) bool {
		_, found := c.Load(c.Hash(request), request)
		return found
	}
	page := cacheURL(http.MethodGet, "https://www.example.com/page", "", "")
	pageHead := cacheURL(http.MethodHead, "https://www.example.com/page", "", "")
	page2 := cacheURL(http.MethodGet, "https://www.example.com/page2", "", "")
	query := cacheURL(http.MethodGet, "https://www.example.com/page?p=1", "", "")
	english := cacheURL(http.MethodGet, "https://www.example.com/lang", "Accept-Language", "en")
	french := cacheURL(http.MethodGet, "https://www.example.com/lang", "Accept-Language", "fr")
	blogA := cacheURL(http.MethodGet, "https://www.example.com/blog/a", "", "")
	blogB := cacheURL(http.MethodGet, "https://www.example.com/blog/b?x=1", "", "")

	u, _ := url.Parse("https://www.example.com/page")
	assert.Equal(t, 2, c.Purge(u, false), "GET and HEAD")
	assert.False(t, cached(page))
	assert.False(t, cached(pageHead))
	assert.True(t, cached(page2), "purging /page mustn't purge /page2")
	assert.True(t, cached(query), "purging /page mustn't purge /page?p=1")

	u, _ = url.Parse("https://www.example.com/lang")
	assert.Equal(t, 2, c.Purge(u, false), "every variant")
	assert.False(t, cached(english))
	assert.False(t, cached(french))

	u, _ = url.Parse("https://www.example.com/blog/")
	assert.Equal(t, 2, c.Purge(u, true))
	assert.False(t, cached(blogA))
	assert.False(t, cached(blogB))
	assert.True(t, cached(page2))

	u, _ = url.Parse("https://www.example.com/")
	assert.Equal(t, 2, c.Purge(u, true))
	entries, size := c.Stats()
	assert.Equal(t, 0, entries, "purging everything should leave nothing, Vary records included")
	assert.Equal(t, int64(0), size)
}
