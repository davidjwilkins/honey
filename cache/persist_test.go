package cache

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cachePage caches a response for u, sent to a request with the given
// headers, and returns that request
func cachePage(t *testing.T, c *defaultCacher, u string, body string, header http.Header, requestHeaders ...string) *http.Request {
	t.Helper()
	request := newValidRequest(u)
	for i := 0; i+1 < len(requestHeaders); i += 2 {
		request.Header.Set(requestHeaders[i], requestHeaders[i+1])
	}
	response := &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Header:     header.Clone(),
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Request:    request,
	}
	c.Cache(c.Hash(request), c.Standardize(response))
	return request
}

func load(t *testing.T, c *defaultCacher, request *http.Request) *responseImpl {
	t.Helper()
	r, found := c.Load(c.Hash(request), request)
	require.True(t, found, request.URL.String())
	return r.(*responseImpl)
}

func TestSnapshotRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.snapshot")
	c := NewCacher(Options{})
	c.AddAllowedCookie("site_lang_id")
	html := http.Header{"Content-Type": {"text/html"}, "Cache-Control": {"max-age=3600"}}
	page := cachePage(t, c, "https://www.example.com/page", compressiblePage(), html)
	withCookie := cachePage(t, c, "https://www.example.com/lang", "<p>hi</p>", http.Header{
		"Cache-Control": {"max-age=3600"},
		"Set-Cookie":    {"site_lang_id=fr; Path=/"},
	})
	vary := http.Header{"Cache-Control": {"max-age=3600"}, "Vary": {"Accept-Language"}}
	english := cachePage(t, c, "https://www.example.com/hello", "hello", vary, "Accept-Language", "en")
	french := cachePage(t, c, "https://www.example.com/hello", "bonjour", vary, "Accept-Language", "fr")
	original := load(t, c, page)

	saved, err := c.SaveTo(path)
	require.NoError(t, err)
	assert.Equal(t, 4, saved)

	restored := NewCacher(Options{})
	restored.AddAllowedCookie("site_lang_id")
	loaded, err := restored.LoadFrom(path)
	require.NoError(t, err)
	assert.Equal(t, 4, loaded)

	r := load(t, restored, page)
	assert.Equal(t, original.Body(), r.Body())
	assert.Equal(t, original.Header(), r.Header())
	assert.Equal(t, original.StatusCode(), r.StatusCode())
	assert.Equal(t, original.Status(), r.Status())
	assert.Equal(t, original.brotliBody(), r.brotliBody(), "the compressed copies should be saved too")
	assert.Equal(t, original.gzip, r.gzip)
	assert.Equal(t, original.now.UnixNano(), r.now.UnixNano(), "the age should carry on from when it was first cached")

	cookie, err := load(t, restored, withCookie).Cookie("site_lang_id")
	require.NoError(t, err)
	assert.Equal(t, "fr", cookie.Value)

	assert.Equal(t, "hello", string(load(t, restored, english).Body()), "each Vary variant should be restored")
	assert.Equal(t, "bonjour", string(load(t, restored, french).Body()))
}

func TestSnapshotSkipsExpiredResponses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.snapshot")
	c := NewCacher(Options{})
	fresh := cachePage(t, c, "https://www.example.com/fresh", "fresh", http.Header{"Cache-Control": {"max-age=3600"}})
	expiring := cachePage(t, c, "https://www.example.com/expiring", "expiring", http.Header{"Cache-Control": {"max-age=1"}})
	_, err := c.SaveTo(path)
	require.NoError(t, err)

	time.Sleep(1100 * time.Millisecond)
	restored := NewCacher(Options{})
	loaded, err := restored.LoadFrom(path)
	require.NoError(t, err)
	assert.Equal(t, 1, loaded, "a response which can no longer be served shouldn't be loaded")
	_, found := restored.Load(restored.Hash(fresh), fresh)
	assert.True(t, found)
	_, found = restored.Load(restored.Hash(expiring), expiring)
	assert.False(t, found)
}

func TestSnapshotKeepsMostRecentlyUsedWhenTooBig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.snapshot")
	c := NewCacher(Options{})
	var requests []*http.Request
	for i := 0; i < 10; i++ {
		requests = append(requests, cachePage(t, c, "https://www.example.com/"+string(rune('a'+i)), strings.Repeat("x", 1000), http.Header{"Cache-Control": {"max-age=3600"}}))
	}
	// use the first one, making it the most recently used
	load(t, c, requests[0])
	_, err := c.SaveTo(path)
	require.NoError(t, err)

	small := NewCacher(Options{MaxBytes: 4000})
	_, err = small.LoadFrom(path)
	require.NoError(t, err)
	entries, size := small.Stats()
	assert.LessOrEqual(t, size, int64(4000))
	assert.Greater(t, entries, 0)
	_, found := small.Load(small.Hash(requests[0]), requests[0])
	assert.True(t, found, "the most recently used should be kept")
	_, found = small.Load(small.Hash(requests[1]), requests[1])
	assert.False(t, found, "the least recently used should be dropped")
}

func TestSnapshotMissingAndBadFiles(t *testing.T) {
	dir := t.TempDir()
	c := NewCacher(Options{})
	loaded, err := c.LoadFrom(filepath.Join(dir, "missing"))
	assert.NoError(t, err, "a missing snapshot just means there's nothing to load")
	assert.Zero(t, loaded)

	garbage := filepath.Join(dir, "garbage")
	require.NoError(t, os.WriteFile(garbage, []byte("not a snapshot"), 0600))
	_, err = c.LoadFrom(garbage)
	assert.Error(t, err)

	path := filepath.Join(dir, "cache.snapshot")
	cachePage(t, c, "https://www.example.com/page", "page", http.Header{"Cache-Control": {"max-age=3600"}})
	_, err = c.SaveTo(path)
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	truncated := filepath.Join(dir, "truncated")
	require.NoError(t, os.WriteFile(truncated, data[:len(data)-10], 0600))
	fromTruncated := NewCacher(Options{})
	loaded, err = fromTruncated.LoadFrom(truncated)
	assert.Error(t, err)
	assert.Zero(t, loaded, "the only response was cut off, so none can be loaded")
	entries, _ := fromTruncated.Stats()
	assert.Zero(t, entries)
}

func TestSnapshotFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.snapshot")
	c := NewCacher(Options{})
	cachePage(t, c, "https://www.example.com/page", "page", http.Header{"Cache-Control": {"max-age=3600"}})
	for i := 0; i < 2; i++ {
		_, err := c.SaveTo(path)
		require.NoError(t, err)
	}
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm(), "cached pages shouldn't be readable by other users")
	files, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, files, 1, "no temporary files should be left behind")

	_, err = c.SaveTo(filepath.Join(dir, "missing-dir", "cache.snapshot"))
	assert.Error(t, err)
}

func TestSnapshotWhileInUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.snapshot")
	c := NewCacher(Options{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				request := cachePage(t, c, "https://www.example.com/"+string(rune('a'+i))+"/"+string(rune('a'+j%26)), compressiblePage()[:2000], http.Header{"Cache-Control": {"max-age=3600"}, "Content-Type": {"text/html"}})
				c.Load(c.Hash(request), request)
			}
		}(i)
	}
	for i := 0; i < 5; i++ {
		_, err := c.SaveTo(path)
		require.NoError(t, err)
	}
	wg.Wait()
	_, err := NewCacher(Options{}).LoadFrom(path)
	assert.NoError(t, err)
}
