package fetch

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shopPage is a shop's page, long enough to be compressed
var shopPage = `<main>` + strings.Repeat("<p>Honey jar, $9.99</p>", 100) + `</main>`

// shopOrigin serves pages like WordPress's plugin does with WooCommerce:
// the shopper's cart is an include, in an ESI comment, for shoppers only
type shopOrigin struct {
	*origin
	pages, includes atomic.Int32
}

func newShopOrigin() *shopOrigin {
	o := &shopOrigin{}
	o.origin = newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		if r.URL.Query().Get(ESIParam) == "cart" {
			o.includes.Add(1)
			if c, err := r.Cookie("wp_woocommerce_session_abc"); err == nil {
				io.WriteString(w, "<script>cart = "+c.Value+"</script>")
			}
			return
		}
		o.pages.Add(1)
		if strings.Contains(r.Header.Get("Surrogate-Capability"), "ESI/1.0") {
			w.Header().Set("Surrogate-Control", `content="ESI/1.0"`)
			w.Header().Add(cache.ESICookiesHeader, "woocommerce_items_in_cart, wp_woocommerce_session_")
			io.WriteString(w, shopPage+`<!--esi <esi:include src="/?honey-esi=cart"/> -->`)
			return
		}
		cart := ""
		if c, err := r.Cookie("wp_woocommerce_session_abc"); err == nil {
			cart = "<script>cart = " + c.Value + "</script>"
		}
		io.WriteString(w, shopPage+cart)
	})
	return o
}

const shopper = "woocommerce_items_in_cart=1; wp_woocommerce_session_abc=jar"

func TestESIForShoppers(t *testing.T) {
	o := newShopOrigin()
	proxy := esiProxy(t, o.origin, cache.Options{DefaultTTL: time.Hour, ESICookies: []string{"woocommerce_items_in_cart", "wp_woocommerce_session_"}}, Options{})
	get(t, proxy+"/shop/")

	resp, body := get(t, proxy+"/shop/")
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"))
	assert.Equal(t, shopPage+`<!--esi <esi:include src="/?honey-esi=cart"/> -->`, body,
		"visitors without cookies get the page as it is: its include is only for shoppers")
	assert.True(t, resp.Uncompressed, "compressed once, like any other page")
	assert.NotEmpty(t, resp.Header.Get("Etag"))
	assert.NotEqual(t, "private, no-cache", resp.Header.Get("Cache-Control"))
	assert.Empty(t, resp.Header.Get(cache.ESIHeader))
	assert.Empty(t, resp.Header.Get(cache.ESICookiesHeader))
	assert.Equal(t, int32(0), o.includes.Load())

	for i := 0; i < 2; i++ {
		resp, body = get(t, proxy+"/shop/", "Cookie", shopper)
		assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"), "the shopper gets the cached page")
		assert.Equal(t, shopPage+` <script>cart = jar</script> `, body, "with their cart")
		assert.Equal(t, "private, no-cache", resp.Header.Get("Cache-Control"))
		assert.Empty(t, resp.Header.Get("Etag"))
	}
	assert.Equal(t, int32(1), o.pages.Load(), "the page wasn't rendered for the shopper")
	assert.Equal(t, int32(2), o.includes.Load(), "but their cart was, each time")

	_, body = get(t, proxy+"/shop/")
	assert.NotContains(t, body, "jar</script>", "and it isn't shared")

	resp, body = get(t, proxy+"/shop/", "Cookie", shopper+"; comment_author_abc=Jane")
	assert.Equal(t, "NO-CACHE", resp.Header.Get("X-Honey-Cache"), "visitors with cookies the page isn't for go to the backend")
	assert.Contains(t, body, "<script>cart = jar</script>")
}

func TestESIForCookiesThePageIsFor(t *testing.T) {
	o := newShopOrigin()
	everyone := []string{"woocommerce_items_in_cart", "wp_woocommerce_session_", "comment_author_"}
	proxy := esiProxy(t, o.origin, cache.Options{DefaultTTL: time.Hour, ESICookies: everyone}, Options{})
	get(t, proxy+"/shop/")

	resp, _ := get(t, proxy+"/shop/", "Cookie", "comment_author_abc=Jane")
	assert.Equal(t, "NO-CACHE", resp.Header.Get("X-Honey-Cache"), "the page's includes aren't for commenters")
	resp, _ = get(t, proxy+"/shop/", "Cookie", shopper)
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"))

	// A page which doesn't say who it's for is for all esiCookies
	c := newESIOrigin(true, "/post/?honey-esi=comments")
	proxy = esiProxy(t, c.origin, cache.Options{DefaultTTL: time.Hour, ESICookies: everyone}, Options{})
	get(t, proxy+"/post/")
	resp, _ = get(t, proxy+"/post/", "Cookie", shopper+"; comment_author_abc=Jane")
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"))
}

func TestESIPagesForSomeAreForAllIfTheyNeedIt(t *testing.T) {
	// Like a post whose comments are an include: visitors without cookies
	// need its ESI processed too, whoever it says it's for
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Query().Get(ESIParam) != "" {
			io.WriteString(w, "comments")
			return
		}
		w.Header().Set("Surrogate-Control", `content="ESI/1.0"`)
		w.Header().Add(cache.ESICookiesHeader, "comment_author_")
		w.Header().Add(cache.ESICookiesHeader, "woocommerce_items_in_cart")
		io.WriteString(w, `<main>post</main><esi:include src="/?honey-esi=comments"/>`)
	})
	proxy := esiProxy(t, o, cache.Options{DefaultTTL: time.Hour, ESICookies: []string{"comment_author_", "woocommerce_items_in_cart"}}, Options{})
	get(t, proxy+"/post/")
	resp, body := get(t, proxy+"/post/")
	assert.Equal(t, `<main>post</main>comments`, body)
	assert.Equal(t, "private, no-cache", resp.Header.Get("Cache-Control"))
	resp, _ = get(t, proxy+"/post/", "Cookie", "comment_author_abc=Jane; woocommerce_items_in_cart=1")
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"), "from either header line")
}

func TestESIPagesArentForIgnoredQueryParameters(t *testing.T) {
	o := newShopOrigin()
	proxy := esiProxy(t, o.origin, cache.Options{DefaultTTL: time.Hour, ESICookies: []string{"woocommerce_items_in_cart", "wp_woocommerce_session_"}},
		Options{QueryParams: []string{"p"}})
	get(t, proxy+"/shop/")

	resp, _ := get(t, proxy+"/shop/?p=1", "Cookie", shopper)
	assert.Equal(t, "NO-CACHE", resp.Header.Get("X-Honey-Cache"), "?p=1 isn't cached yet")
	get(t, proxy+"/shop/?p=1")
	resp, _ = get(t, proxy+"/shop/?p=1", "Cookie", shopper)
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"))

	resp, _ = get(t, proxy+"/shop/?add-to-cart=12", "Cookie", shopper)
	assert.Equal(t, "NO-CACHE", resp.Header.Get("X-Honey-Cache"),
		"the cache ignores ?add-to-cart, but WooCommerce doesn't, so the shopper mustn't get /shop/ instead")
}

func TestExplainESIForShoppers(t *testing.T) {
	o := newShopOrigin()
	backend, err := url.Parse(o.URL)
	require.NoError(t, err)
	c := cache.NewCacher(cache.Options{BypassCookies: cache.WordPressBypassCookies, DefaultTTL: time.Hour,
		ESICookies: []string{"comment_author_", "woocommerce_items_in_cart", "wp_woocommerce_session_"}})
	proxy := httptest.NewServer(FetchWithOptions(c, Forwarder(c), backend, Options{}))
	t.Cleanup(proxy.Close)
	t.Cleanup(o.Close)
	get(t, proxy.URL+"/shop/")

	explain := func(cookies string) Explanation {
		return Explain(c, backend, Options{}, explainRequest(http.MethodGet, proxy.URL+"/shop/", cookies))
	}
	e := explain(shopper)
	assert.Equal(t, "changed: Served the cached page, with ESI", titles(e.Steps)[len(e.Steps)-1])
	e = explain("comment_author_abc=Jane")
	assert.Equal(t, "stopped: Not served the cached page", titles(e.Steps)[len(e.Steps)-1], "the page isn't for commenters")
	assert.Equal(t, "Not cached: every request goes to the backend", e.Verdict)
}
