package cache

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func compressiblePage() string {
	var b strings.Builder
	for i := 0; b.Len() < 50<<10; i++ {
		fmt.Fprintf(&b, "<li><a href=\"/category/%d/\">Category %d</a> - %d posts, last updated %d days ago</li>\n", i*31%97, i, i*i%113, i%29)
	}
	return b.String()
}

func htmlResponse(c *defaultCacher, body string, headers ...string) *responseImpl {
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/html; charset=utf-8"}},
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Request:    validRequest(),
	}
	for i := 0; i+1 < len(headers); i += 2 {
		response.Header.Set(headers[i], headers[i+1])
	}
	return c.Standardize(response).(*responseImpl)
}

func decode(t *testing.T, compressed []byte) string {
	t.Helper()
	decoded, err := io.ReadAll(brotli.NewReader(bytes.NewReader(compressed)))
	require.NoError(t, err)
	return string(decoded)
}

func TestBrotliRecompressedInBackground(t *testing.T) {
	c := NewCacher(Options{})
	body := compressiblePage()
	r := htmlResponse(c, body)
	fast := r.brotliBody()
	require.NotNil(t, fast, "compressible responses should be compressed when cached")
	assert.Equal(t, body, decode(t, fast))
	assert.Equal(t, "Accept-Encoding", r.Header().Get("Vary"))

	request := validRequest()
	c.Cache(c.Hash(request), r)
	c.recompressor.wait()

	best := r.brotliBody()
	assert.Less(t, len(best), len(fast), "the background pass should compress better")
	assert.Equal(t, body, decode(t, best))
	assert.Equal(t, body, string(r.Body()), "the unencoded body is kept for clients which don't accept brotli")
}

func TestBrotliCountsTowardsCacheSize(t *testing.T) {
	c := NewCacher(Options{})
	r := htmlResponse(c, compressiblePage())
	withBrotli := responseSize("key", r)
	r.brotli.Store(nil)
	assert.Greater(t, withBrotli, responseSize("key", r))
}

func TestBrotliCanBeDisabled(t *testing.T) {
	c := NewCacher(Options{DisableBrotli: true})
	r := htmlResponse(c, compressiblePage())
	assert.Nil(t, r.brotliBody())
	assert.Equal(t, "", r.Header().Get("Vary"))
}

func TestBrotliMinBytes(t *testing.T) {
	c := NewCacher(Options{BrotliMinBytes: 100 << 10})
	assert.Nil(t, htmlResponse(c, compressiblePage()).brotliBody(), "responses under BrotliMinBytes shouldn't be compressed")
}

func TestBrotliVaryIsNotPartOfCacheKey(t *testing.T) {
	c := NewCacher(Options{})
	r := htmlResponse(c, compressiblePage(), "Vary", "Accept-Encoding")
	c.Cache(c.Hash(validRequest()), r)

	gzip := validRequest()
	gzip.Header.Set("Accept-Encoding", "gzip")
	br := validRequest()
	br.Header.Set("Accept-Encoding", "gzip, br")
	assert.Equal(t, c.Hash(gzip), c.Hash(br))
	_, found := c.Load(c.Hash(br), br)
	assert.True(t, found)
	assert.Equal(t, []string{"Accept-Encoding"}, r.Header().Values("Vary"), "Vary: Accept-Encoding shouldn't be added twice")
}

func TestNegotiate(t *testing.T) {
	c := NewCacher(Options{})
	body := compressiblePage()
	r := htmlResponse(c, body)

	request := validRequest()
	header, served := Negotiate(r, request)
	assert.Equal(t, body, string(served))
	assert.Equal(t, "", header.Get("Content-Encoding"))

	request.Header.Set("Accept-Encoding", "gzip, br")
	header, served = Negotiate(r, request)
	assert.Equal(t, "br", header.Get("Content-Encoding"))
	assert.Equal(t, fmt.Sprint(len(served)), header.Get("Content-Length"))
	assert.Equal(t, strings.TrimSuffix(r.Header().Get("Etag"), `"`)+`-br"`, header.Get("Etag"))
	assert.Equal(t, body, decode(t, served))
	assert.Equal(t, "", r.Header().Get("Content-Encoding"), "Negotiate mustn't modify the cached headers")

	request.Header.Set("Range", "bytes=0-10")
	header, served = Negotiate(r, request)
	assert.Equal(t, "", header.Get("Content-Encoding"), "ranges apply to the unencoded body")
	assert.Equal(t, body, string(served))
}
