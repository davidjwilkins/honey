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

// esiOrigin serves pages whose comments section is an ESI include, which
// is filled in with the commenter's name, like WordPress's plugin
type esiOrigin struct {
	*origin
	pages, includes atomic.Int32
	// esi is whether pages say they use ESI
	esi bool
}

func newESIOrigin(esi bool, include string) *esiOrigin {
	o := &esiOrigin{esi: esi}
	o.origin = newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		if r.URL.Query().Get(ESIParam) == "comments" {
			o.includes.Add(1)
			io.WriteString(w, "<section>Comment form for "+commenter(r)+"</section>")
			return
		}
		o.pages.Add(1)
		if o.esi && !strings.Contains(r.Header.Get("Surrogate-Capability"), "ESI/1.0") {
			// Without ESI, the comments section is rendered in place
			io.WriteString(w, `<main>`+r.URL.Path+`</main><p>No ESI</p><section>Comment form for `+commenter(r)+`</section>`)
			return
		}
		if o.esi {
			w.Header().Set("Surrogate-Control", `content="ESI/1.0"`)
		}
		io.WriteString(w, `<main>`+r.URL.Path+`</main><!--esi <p>ESI on</p> --><esi:remove><p>No ESI</p></esi:remove>`+
			`<esi:include src="`+include+`"/>`)
	})
	return o
}

func commenter(r *http.Request) string {
	if c, err := r.Cookie("comment_author_abc"); err == nil {
		return c.Value
	}
	return "visitor"
}

func esiProxy(t *testing.T, o *esiOrigin, opts cache.Options, fetchOpts Options) string {
	t.Helper()
	if opts.BypassCookies == nil {
		opts.BypassCookies = cache.WordPressBypassCookies
	}
	return routeProxy(t, o.origin, opts, fetchOpts).URL
}

func TestESIForVisitors(t *testing.T) {
	o := newESIOrigin(true, "/post/?honey-esi=comments")
	proxy := esiProxy(t, o, cache.Options{DefaultTTL: time.Hour}, Options{})

	resp, body := get(t, proxy+"/post/")
	assert.Equal(t, "MISS", resp.Header.Get("X-Honey-Cache"))
	assert.Equal(t, `<main>/post/</main> <p>ESI on</p> <section>Comment form for visitor</section>`, body)
	assert.Empty(t, resp.Header.Get("Etag"), "the page is put together for each visitor")
	assert.Empty(t, resp.Header.Get(cache.ESIHeader))
	assert.Empty(t, resp.Header.Get("Surrogate-Control"))
	assert.Equal(t, "private, no-cache", resp.Header.Get("Cache-Control"))

	resp, body = get(t, proxy+"/post/")
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"))
	assert.Contains(t, body, "Comment form for visitor")
	assert.Equal(t, int32(1), o.pages.Load())
	assert.Equal(t, int32(1), o.includes.Load(), "the include is the same for every visitor, so it's cached too")
}

func TestESIForCommenters(t *testing.T) {
	o := newESIOrigin(true, "/post/?honey-esi=comments")
	proxy := esiProxy(t, o, cache.Options{DefaultTTL: time.Hour, ESICookies: []string{"comment_author_"}}, Options{})
	get(t, proxy+"/post/")

	for i := 0; i < 2; i++ {
		resp, body := get(t, proxy+"/post/", "Cookie", "comment_author_abc=Jane; _ga=1")
		assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"), "the commenter gets the cached page")
		assert.Contains(t, body, "<main>/post/</main>")
		assert.Contains(t, body, "Comment form for Jane", "with their own comments section")
	}
	assert.Equal(t, int32(1), o.pages.Load(), "the page wasn't rendered for the commenter")
	assert.Equal(t, int32(3), o.includes.Load(), "but their comments section was, each time")

	_, body := get(t, proxy+"/post/")
	assert.Contains(t, body, "Comment form for visitor", "and it isn't shared")

	resp, body := get(t, proxy+"/post/", "Cookie", "comment_author_abc=Jane; wordpress_logged_in_abc=admin")
	assert.Equal(t, "NO-CACHE", resp.Header.Get("X-Honey-Cache"), "visitors with other bypass cookies go to the backend")
	assert.Contains(t, body, "Comment form for Jane")
	assert.Equal(t, int32(2), o.pages.Load())

	resp, _ = get(t, proxy+"/never-cached/", "Cookie", "comment_author_abc=Jane")
	assert.Equal(t, "NO-CACHE", resp.Header.Get("X-Honey-Cache"), "nor if the page isn't cached")
}

func TestESIOnlyForSomeCookies(t *testing.T) {
	o := newESIOrigin(true, "/post/?honey-esi=comments")
	proxy := esiProxy(t, o, cache.Options{DefaultTTL: time.Hour}, Options{})
	get(t, proxy+"/post/")
	resp, body := get(t, proxy+"/post/", "Cookie", "comment_author_abc=Jane")
	assert.Equal(t, "NO-CACHE", resp.Header.Get("X-Honey-Cache"), "without esiCookies, commenters go to the backend")
	assert.Contains(t, body, "Comment form for Jane", "where the page still gets their comments section")
}

func TestESIOnlyIfThePageSaysSo(t *testing.T) {
	o := newESIOrigin(false, "/post/?honey-esi=comments")
	proxy := esiProxy(t, o, cache.Options{}, Options{})
	_, body := get(t, proxy+"/post/")
	assert.Contains(t, body, `<esi:include src="/post/?honey-esi=comments"/>`)
	assert.Contains(t, body, "<p>No ESI</p>")
	assert.Zero(t, o.includes.Load())

	// Nor can a visitor ask for it
	o = newESIOrigin(true, "/post/?honey-esi=comments")
	proxy = esiProxy(t, o, cache.Options{}, Options{})
	_, body = get(t, proxy+"/never-cached/", "Cookie", "wordpress_logged_in_abc=admin", "Surrogate-Capability", `honey="ESI/1.0"`)
	assert.Contains(t, body, "<p>No ESI</p>", "the backend isn't told the cache supports ESI")
	assert.NotContains(t, body, "esi")
}

func TestESIIncludesStayOnTheSite(t *testing.T) {
	var requested atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested.Add(1)
		io.WriteString(w, "from elsewhere")
	}))
	t.Cleanup(other.Close)
	for _, src := range []string{other.URL + "/", "//evil.example/", "javascript:alert(1)", ""} {
		o := newESIOrigin(true, src)
		proxy := esiProxy(t, o, cache.Options{}, Options{})
		_, body := get(t, proxy+"/post/")
		assert.Equal(t, `<main>/post/</main> <p>ESI on</p> `, body, src)
	}
	assert.Zero(t, requested.Load())
}

func TestESIIncludesAreCompressedAndFiltered(t *testing.T) {
	o := newESIOrigin(true, "comments-page?utm_source=x&amp;honey-esi=comments")
	proxy := esiProxy(t, o, cache.Options{}, Options{QueryParams: []string{"p"}})
	resp, err := http.Get(proxy + "/post/" + strings.Repeat("x", 2000) + "/")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.True(t, resp.Uncompressed, "it's compressed for the client (and Go decompressed it)")
	assert.Contains(t, string(body), "Comment form for visitor", "the include's honey-esi parameter is kept, and it's resolved against the page")
}

func TestESILimits(t *testing.T) {
	var calls atomic.Int32
	var handler http.HandlerFunc
	handler = func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// Every include includes itself
		ew, finish := withESI(w, r, handler, DefaultSecretHeader)
		defer finish()
		ew.Header().Set(cache.ESIHeader, "1")
		io.WriteString(ew, `x<esi:include src="/again"/>`)
	}
	visitor := httptest.NewRequest(http.MethodGet, "http://www.example.com/", nil)
	visitor.Header.Set("X-Forwarded-Proto", "http")
	w := httptest.NewRecorder()
	handler(w, visitor)
	assert.Equal(t, strings.Repeat("x", maxESIDepth+1)+`<esi:include src="/again"/>`, w.Body.String(), "includes are nested at most this deep")

	body := processESI([]byte(strings.Repeat(`<esi:include src="/a" alt="/b" onerror="continue"/>`, maxIncludes+5)), visitor,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/a" {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, "b")
		}), DefaultSecretHeader)
	assert.Equal(t, strings.Repeat("b", maxIncludes)+strings.Repeat(`<esi:include src="/a" alt="/b" onerror="continue"/>`, 5), string(body),
		"alt is used if src fails, and only so many are fetched")
}

func TestESIIncludesDontGetTheSecret(t *testing.T) {
	var got http.Header
	visitor := httptest.NewRequest(http.MethodGet, "http://www.example.com/", nil)
	visitor.Header.Set(DefaultSecretHeader, secret)
	visitor.Header.Set("Cookie", "comment_author_abc=Jane")
	visitor.Header.Set("If-None-Match", `"x"`)
	processESI([]byte(`<esi:include src="/a"/>`), visitor, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
	}), DefaultSecretHeader)
	require.NotNil(t, got)
	assert.Empty(t, got.Get(DefaultSecretHeader))
	assert.Empty(t, got.Get("If-None-Match"))
	assert.Equal(t, "comment_author_abc=Jane", got.Get("Cookie"), "it's requested as the visitor")
}

func TestExplainESI(t *testing.T) {
	o := newESIOrigin(true, "/post/?honey-esi=comments")
	backend, err := url.Parse(o.URL)
	require.NoError(t, err)
	c := cache.NewCacher(cache.Options{BypassCookies: cache.WordPressBypassCookies, DefaultTTL: time.Hour, ESICookies: []string{"comment_author_"}})
	proxy := httptest.NewServer(FetchWithOptions(c, Forwarder(c), backend, Options{}))
	t.Cleanup(proxy.Close)
	t.Cleanup(o.Close)

	explain := func(cookies string) Explanation {
		return Explain(c, backend, Options{}, explainRequest(http.MethodGet, proxy.URL+"/post/", cookies))
	}
	e := explain("comment_author_abc=Jane")
	assert.False(t, e.Cacheable)
	assert.Equal(t, "ok: Could get the cached page, with ESI", titles(e.Steps)[len(e.Steps)-1], "it isn't cached yet")

	get(t, proxy.URL+"/post/")
	e = explain("")
	assert.True(t, e.Cacheable)
	require.NotNil(t, e.Cached)
	assert.True(t, e.Cached.ESI)
	assert.Contains(t, titles(e.Steps), "changed: Uses ESI")

	e = explain("comment_author_abc=Jane")
	assert.False(t, e.Cacheable)
	assert.Equal(t, "changed: Served the cached page, with ESI", titles(e.Steps)[len(e.Steps)-1])
	assert.Equal(t, "Cached page, with its includes fetched for this visitor", e.Verdict)

	e = explain("comment_author_abc=Jane; wordpress_logged_in_abc=admin")
	assert.Equal(t, "stopped: Not cacheable", titles(e.Steps)[len(e.Steps)-1], "other bypass cookies")
}

func TestESIMarkerComesOnlyFromTheCache(t *testing.T) {
	var includes atomic.Int32
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		if r.URL.Path == "/a" {
			includes.Add(1)
		}
		w.Header().Set("Content-Type", "text/html")
		// but not Surrogate-Control
		w.Header().Set(cache.ESIHeader, "1")
		io.WriteString(w, `<esi:include src="/a"/>`)
	})
	proxy := esiProxy(t, &esiOrigin{origin: o}, cache.Options{}, Options{})
	for _, cookies := range []string{"", "wordpress_logged_in_abc=admin"} {
		resp, body := get(t, proxy+"/post/", "Cookie", cookies)
		assert.Equal(t, `<esi:include src="/a"/>`, body, cookies)
		assert.Empty(t, resp.Header.Get(cache.ESIHeader))
	}
	assert.Zero(t, includes.Load())
}
