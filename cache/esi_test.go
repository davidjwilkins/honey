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

func TestESIMark(t *testing.T) {
	for _, test := range []struct {
		cookies []string
		body    string
		want    string
	}{
		{nil, `<esi:include src="/a"/>`, "all"},
		{nil, `<!--esi <esi:include src="/a"/> -->`, "all"},
		{[]string{"woocommerce_items_in_cart, wp_woocommerce_session_"}, `<p><!--esi <esi:include src="/a"/> --></p>`, "personal woocommerce_items_in_cart wp_woocommerce_session_"},
		{[]string{"comment_author_", "woocommerce_items_in_cart"}, `<p><!--esi <esi:include src="/a"/> --><esi:include src="/b"/></p>`, "all comment_author_ woocommerce_items_in_cart"},
		{[]string{"comment_author_"}, `<esi:remove>visitor</esi:remove><!--esi <esi:include src="/a"/> -->`, "all comment_author_"},
		{[]string{"woocommerce_items_in_cart"}, `<p>nothing personal</p>`, "personal woocommerce_items_in_cart"},
	} {
		assert.Equal(t, test.want, esiMark(http.Header{ESICookiesHeader: test.cookies}, []byte(test.body)), "%v %s", test.cookies, test.body)
	}
}

func TestESIServes(t *testing.T) {
	c := NewCacher(Options{BypassCookies: WordPressBypassCookies, ESICookies: []string{"comment_author_", "woocommerce_items_in_cart", "wp_woocommerce_session_"}})
	page := func(cookies ...string) Response {
		return esiResponse(c, SurrogateCapability, "text/html", http.Header{"Surrogate-Control": {`content="ESI/1.0"`}, ESICookiesHeader: cookies})
	}
	visitor := func(cookies string) *http.Request {
		r := validRequest()
		r.Header.Set("Cookie", cookies)
		return r
	}
	shop := page("woocommerce_items_in_cart, wp_woocommerce_session_")
	assert.Empty(t, shop.Header().Get(ESICookiesHeader), "it's for Honey, not clients")
	assert.True(t, c.ESIServes(shop, visitor("woocommerce_items_in_cart=1; wp_woocommerce_session_abc=x; _ga=1")))
	assert.False(t, c.ESIServes(shop, visitor("woocommerce_items_in_cart=1; comment_author_abc=Jane")))
	assert.True(t, c.ESIServes(page(), visitor("woocommerce_items_in_cart=1; comment_author_abc=Jane")), "a page which doesn't say is for all esiCookies")
	assert.False(t, c.ESIServes(esiResponse(c, SurrogateCapability, "text/html", nil), visitor("comment_author_abc=Jane")), "it doesn't use ESI")
}
