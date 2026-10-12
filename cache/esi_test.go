package cache

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func esiResponse(c *defaultCacher, capability, contentType string, header http.Header) Response {
	request := validRequest()
	if capability != "" {
		request.Header.Set("Surrogate-Capability", capability)
	}
	if header == nil {
		header = http.Header{}
	}
	header.Set("Content-Type", contentType)
	return c.Standardize(&http.Response{
		StatusCode: http.StatusOK,
		Header:     header,
		Body:       io.NopCloser(bytes.NewBufferString(`<esi:include src="/a"/>`)),
		Request:    request,
	})
}

func TestStandardizeMarksPagesWhichUseESI(t *testing.T) {
	c := NewCacher(Options{})
	esi := http.Header{"Surrogate-Control": {`max-age=60, content="ESI/1.0"`}}

	resp := esiResponse(c, SurrogateCapability, "text/html; charset=UTF-8", esi.Clone())
	assert.True(t, UsesESI(resp))
	assert.Empty(t, resp.Header().Get("Surrogate-Control"), "it's for Honey, not clients")

	assert.False(t, UsesESI(esiResponse(c, "", "text/html", esi.Clone())), "only if Honey said it supports ESI")
	assert.False(t, UsesESI(esiResponse(c, SurrogateCapability, "application/json", esi.Clone())), "only HTML")
	assert.False(t, UsesESI(esiResponse(c, SurrogateCapability, "text/html", nil)), "only if the page says so")
	assert.False(t, UsesESI(esiResponse(c, SurrogateCapability, "text/html", http.Header{ESIHeader: {"1"}})), "the backend can't mark it itself")
}

func TestESIEligible(t *testing.T) {
	c := NewCacher(Options{BypassCookies: WordPressBypassCookies, ESICookies: []string{"comment_author_"}})
	request := func(method, uri, cookies string) *http.Request {
		r := newValidRequest(uri)
		r.Method = method
		if cookies != "" {
			r.Header.Set("Cookie", cookies)
		}
		return r
	}
	assert.True(t, c.ESIEligible(request(http.MethodGet, "https://www.example.com/post/", "comment_author_abc=Jane; comment_author_email_abc=j; _ga=1")))
	assert.True(t, c.ESIEligible(request(http.MethodHead, "https://www.example.com/post/", "comment_author_abc=Jane")))
	assert.False(t, c.ESIEligible(request(http.MethodGet, "https://www.example.com/post/", "_ga=1")), "it has no bypass cookies: it's cacheable anyway")
	assert.False(t, c.ESIEligible(request(http.MethodGet, "https://www.example.com/post/", "comment_author_abc=Jane; wordpress_logged_in_abc=admin")), "it has other bypass cookies")
	assert.False(t, c.ESIEligible(request(http.MethodPost, "https://www.example.com/post/", "comment_author_abc=Jane")))
	authorized := request(http.MethodGet, "https://www.example.com/post/", "comment_author_abc=Jane")
	authorized.Header.Set("Authorization", "Basic x")
	assert.False(t, c.ESIEligible(authorized))

	off := NewCacher(Options{BypassCookies: WordPressBypassCookies})
	assert.False(t, off.ESIEligible(request(http.MethodGet, "https://www.example.com/post/", "comment_author_abc=Jane")), "off unless esiCookies are set")
}
