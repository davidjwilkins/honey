package cache

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRewritePage(t *testing.T) {
	c := NewCacher(Options{RewritePage: func(r *http.Response, body []byte) []byte {
		return bytes.ReplaceAll(body, []byte("slow"), []byte("fast!"))
	}})
	resp := c.Standardize(&http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/html"}, "Content-Length": {"14"}},
		Body:       io.NopCloser(bytes.NewBufferString("<p>slow</p>   ")),
		Request:    validRequest(),
	})
	assert.Equal(t, "<p>fast!</p>   ", string(resp.Body()))
	assert.Equal(t, "15", resp.Header().Get("Content-Length"))

	unchanged := NewCacher(Options{})
	resp2 := unchanged.Standardize(&http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/html"}},
		Body:       io.NopCloser(bytes.NewBufferString("<p>fast!</p>   ")),
		Request:    validRequest(),
	})
	assert.Equal(t, resp2.Header().Get("Etag"), resp.Header().Get("Etag"), "the Etag is the rewritten page's")
}
