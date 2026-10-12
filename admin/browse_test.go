package admin

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/cache"
	"github.com/davidjwilkins/honey/optimize"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cachedPage is a cached response, for tests
type cachedPage struct {
	cache.Response
	body   string
	header http.Header
}

func (p cachedPage) Body() []byte                { return []byte(p.body) }
func (p cachedPage) Header() http.Header         { return p.header }
func (p cachedPage) RequestHeaders() http.Header { return http.Header{} }

func browseAdmin(t *testing.T, entries []cache.Entry) (http.Handler, *[]purgeCall) {
	t.Helper()
	var calls []purgeCall
	h := Handler(Options{
		Started: time.Now(),
		Hosts:   []string{"www.example.com"},
		Entries: func() []cache.Entry { return entries },
		Entry: func(key string) (cache.Entry, cache.Response, bool) {
			for _, e := range entries {
				if e.Key == key {
					return e, cachedPage{body: "<p>Hello & welcome</p>", header: http.Header{"Content-Type": {e.ContentType}, "Cache-Control": {"public, max-age=300"}}}, true
				}
			}
			return cache.Entry{}, nil, false
		},
		Purge: func(rawURL string, prefix, soft bool) (int, error) {
			calls = append(calls, purgeCall{rawURL, prefix, soft})
			return 2, nil
		},
	})
	return h, &calls
}

func entry(path string, hits int64, size int64, age, lifetime time.Duration) cache.Entry {
	return cache.Entry{
		Key: "GET :: https://www.example.com" + path, Method: "GET", URL: "https://www.example.com" + path,
		Host: "www.example.com", Path: path, StatusCode: http.StatusOK, ContentType: "text/html",
		Bytes: int(size), Size: size, Fetched: time.Now().Add(-age), Lifetime: lifetime, Stale: time.Hour, Hits: hits,
	}
}

func testEntries() []cache.Entry {
	notFound := entry("/old-post/", 3, 900, 2*time.Minute, 5*time.Minute)
	notFound.StatusCode = http.StatusNotFound
	expired := entry("/blog/b/", 0, 2000, 30*time.Second, 0)
	expired.Expired = true
	return []cache.Entry{
		entry("/blog/", 120, 40000, time.Minute, 5*time.Minute),
		entry("/blog/a/", 7, 58000, 10*time.Minute, 5*time.Minute),
		expired,
		entry("/style.css", 900, 12000, time.Hour, 7*24*time.Hour),
		notFound,
	}
}

func browse(h http.Handler, query string) string {
	w := do(h, http.MethodGet, "/cache"+query, "127.0.0.1:9090", nil)
	return w.Body.String()
}

// paths returns the paths listed, in order
func paths(body string) []string {
	var found []string
	for _, part := range strings.Split(body, `class="mono path">`)[1:] {
		path, _, _ := strings.Cut(part, "<")
		found = append(found, path)
	}
	return found
}

func TestBrowse(t *testing.T) {
	h, _ := browseAdmin(t, testEntries())
	body := browse(h, "")
	assert.Equal(t, []string{"/blog/", "/blog/a/", "/blog/b/", "/style.css", "/old-post/"}, paths(body), "most recently used first")
	assert.Contains(t, body, "5 cached responses")
	assert.Contains(t, body, `aria-current="page">Cache`)
	assert.Contains(t, body, "hosts www.example.com", "the header still says which hosts are served")
	assert.Contains(t, body, "fresh for 4m 0s")
	assert.Contains(t, body, "stale: refreshed on its next request", "/blog/a/ was fetched 10m ago and is fresh for 5m")
	assert.Contains(t, body, "soft purged: refreshed on its next request")
	assert.Contains(t, body, `href="/cache/entry?key=GET&#43;%3A%3A&#43;https%3A%2F%2Fwww.example.com%2Fblog%2F"`)
	assert.NotContains(t, body, `class="muted host"`, "one host isn't repeated on every row")

	other := entry("/", 0, 100, time.Minute, time.Hour)
	other.Host = "shop.example.com"
	h2, _ := browseAdmin(t, append(testEntries(), other))
	assert.Contains(t, browse(h2, ""), `<span class="muted host">shop.example.com</span>`, "but several are shown")

	for query, want := range map[string][]string{
		"?path=%2Fblog%2F":           {"/blog/", "/blog/a/", "/blog/b/"},
		"?path=post":                 {"/old-post/"},
		"?kind=static":               {"/style.css"},
		"?kind=pages":                {"/blog/", "/blog/a/", "/blog/b/"},
		"?kind=errors":               {"/old-post/"},
		"?state=fresh":               {"/blog/", "/style.css", "/old-post/"},
		"?state=stale":               {"/blog/a/"},
		"?state=expired":             {"/blog/b/"},
		"?sort=hits":                 {"/style.css", "/blog/", "/blog/a/", "/old-post/", "/blog/b/"},
		"?sort=size":                 {"/blog/a/", "/blog/", "/style.css", "/blog/b/", "/old-post/"},
		"?sort=oldest":               {"/style.css", "/blog/a/", "/old-post/", "/blog/", "/blog/b/"},
		"?sort=path":                 {"/blog/", "/blog/a/", "/blog/b/", "/old-post/", "/style.css"},
		"?path=%2Fblog%2F&sort=hits": {"/blog/", "/blog/a/", "/blog/b/"},
		"?kind=nonsense":             {"/blog/", "/blog/a/", "/blog/b/", "/style.css", "/old-post/"},
	} {
		assert.Equal(t, want, paths(browse(h, query)), query)
	}

	body = browse(h, "?path=%2Fblog%2F")
	assert.Contains(t, body, "3 matches")
	assert.Contains(t, body, `name="url" value="/blog/"`, "everything under a path can be purged")
	assert.Contains(t, body, `name="return" value="/cache?path=%2Fblog%2F"`)
	assert.NotContains(t, browse(h, "?path=post"), `name="prefix"`, "but not everything containing some text")
	assert.Contains(t, browse(h, "?path=nothing"), "Nothing cached matches.")
}

func TestBrowsePages(t *testing.T) {
	var entries []cache.Entry
	for i := 0; i < 120; i++ {
		entries = append(entries, entry("/p/"+strings.Repeat("x", i%3)+string(rune('a'+i%26))+"/", int64(i), 100, time.Minute, time.Hour))
	}
	h, _ := browseAdmin(t, entries)
	body := browse(h, "?sort=hits")
	assert.Len(t, paths(body), pageSize)
	assert.Contains(t, body, "1–50 of 120")
	assert.Contains(t, body, `<a href="/cache?page=2&amp;sort=hits">Next</a>`)
	assert.NotContains(t, body, "Previous")

	body = browse(h, "?sort=hits&page=3")
	assert.Len(t, paths(body), 20)
	assert.Contains(t, body, "101–120 of 120")
	assert.Contains(t, body, `<a href="/cache?page=2&amp;sort=hits">Previous</a>`)
	assert.NotContains(t, body, ">Next<")
	assert.Contains(t, browse(h, "?page=99"), "1–50 of 120", "a page which doesn't exist shows the first")
}

func TestEntryPage(t *testing.T) {
	h, _ := browseAdmin(t, testEntries())
	key := "GET :: https://www.example.com/blog/"
	w := do(h, http.MethodGet, "/cache/entry?"+url.Values{"key": {key}}.Encode(), "127.0.0.1:9090", nil)
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `<h1 class="mono path-title">/blog/</h1>`)
	assert.Contains(t, body, "Cache-Control: public, max-age=300")
	assert.Contains(t, body, "120 times")
	assert.Contains(t, body, "&lt;p&gt;Hello &amp; welcome&lt;/p&gt;", "the body is shown, escaped")
	assert.Contains(t, body, `href="/test?method=GET&amp;url=https%3A%2F%2Fwww.example.com%2Fblog%2F"`)
	assert.Contains(t, body, `name="return" value="/cache/entry?key=GET&#43;%3A%3A&#43;https%3A%2F%2Fwww.example.com%2Fblog%2F"`)
	assert.Contains(t, body, `rel="noopener noreferrer"`)

	w = do(h, http.MethodGet, "/cache/entry?key=gone", "127.0.0.1:9090", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "isn't in the cache any more")
}

func TestPurgeReturnsToTheCacheBrowser(t *testing.T) {
	h, calls := browseAdmin(t, testEntries())
	resp := post(h, "/purge", url.Values{"url": {"/blog/"}, "prefix": {"1"}, "soft": {"1"}, "return": {"/cache?path=%2Fblog%2F"}})
	require.Equal(t, http.StatusSeeOther, resp.Code)
	location := resp.Header.Get("Location")
	assert.True(t, strings.HasPrefix(location, "/cache?path=%2Fblog%2F&result="), location)
	assert.Equal(t, []purgeCall{{"/blog/", true, true}}, *calls)
	assert.Contains(t, browse(h, strings.TrimPrefix(location, "/cache")), "Expired 2 cached responses for")

	for _, page := range []string{"https://evil.example/cache", "//evil.example/cache", "/settings", "javascript:alert(1)", ""} {
		resp = post(h, "/purge", url.Values{"url": {"/"}, "return": {page}})
		assert.True(t, strings.HasPrefix(resp.Header.Get("Location"), "/?result="), "%q: only the cache pages are returned to", page)
	}
}

func TestReturnTo(t *testing.T) {
	assert.Equal(t, "/cache/entry?key=x", returnTo("/cache/entry?key=x#top").String())
	assert.Equal(t, "/", returnTo("http://127.0.0.1:9090/cache").String())
}

func TestBrowseNeedsTheCache(t *testing.T) {
	h := Handler(Options{Started: time.Now()})
	assert.Equal(t, http.StatusNotFound, do(h, http.MethodGet, "/cache", "127.0.0.1:9090", nil).Code)
	assert.Equal(t, http.StatusNotFound, do(h, http.MethodGet, "/cache/entry?key=x", "127.0.0.1:9090", nil).Code)
}

func TestFragmentsInTheCacheBrowser(t *testing.T) {
	var purgedName string
	updated := time.Now().Add(-90 * time.Second)
	h := Handler(Options{
		Started: time.Now(),
		Entries: func() []cache.Entry {
			e := entry("/blog/", 1, 100, time.Minute, time.Hour)
			e.Fragments = []string{"posts-widget-recent-posts-2"}
			return []cache.Entry{e}
		},
		Entry: func(key string) (cache.Entry, cache.Response, bool) {
			e := entry("/blog/", 1, 100, time.Minute, time.Hour)
			e.Fragments = []string{"posts-widget-recent-posts-2"}
			return e, cachedPage{header: http.Header{}}, true
		},
		Fragments: func() []cache.FragmentInfo {
			return []cache.FragmentInfo{{Host: "www.example.com", Name: "posts-widget-recent-posts-2", Bytes: 2048, Updated: updated}}
		},
		PurgeFragments: func(name string) (int, int, error) {
			purgedName = name
			return 1, 1, nil
		},
	})
	body := browse(h, "")
	assert.Contains(t, body, "1 shared fragment</summary>")
	assert.Contains(t, body, `<th scope="row" class="mono">posts-widget-recent-posts-2</th>`)
	assert.Contains(t, body, "1m 30s ago")

	entryBody := do(h, http.MethodGet, "/cache/entry?key=x", "127.0.0.1:9090", nil).Body.String()
	assert.Contains(t, entryBody, "Shared fragments")

	resp := post(h, "/purge", url.Values{"fragment": {"posts-widget-recent-posts-2"}, "return": {"/cache"}})
	require.Equal(t, http.StatusSeeOther, resp.Code)
	assert.Equal(t, "posts-widget-recent-posts-2", purgedName)
	assert.Contains(t, browse(h, strings.TrimPrefix(resp.Header.Get("Location"), "/cache")),
		"Purged 1 shared fragment called <span class=\"mono\">posts-widget-recent-posts-2</span>: refreshing 1 page to get its new content, for every page showing it.")
}

func TestEntryPageShowsOptimizations(t *testing.T) {
	page := entry("/post/", 1, 4000, time.Minute, time.Hour)
	var asked []string
	h := Handler(Options{
		Started: time.Now(),
		Entries: func() []cache.Entry { return []cache.Entry{page} },
		Entry: func(key string) (cache.Entry, cache.Response, bool) {
			return page, cachedPage{body: "<html>the page</html>", header: http.Header{"Content-Type": {"text/html"}}}, key == page.Key
		},
		Optimizations: func(host, path string, body []byte) *optimize.Report {
			asked = append(asked, host+path+" "+string(body))
			return &optimize.Report{
				MainImages: []optimize.MainImageReport{
					{Screens: "phones", State: "learned", URL: "/wp-content/uploads/hero-768x432.jpg"},
					{Screens: "desktops", State: "asked"},
				},
				DelayedScripts: []string{"https://www.googletagmanager.com/gtag/js?id=G-1", "inline (#fb-pixel)"},
				UsedCSS:        []optimize.UsedCSSReport{{Href: "/wp-content/themes/t/style.css?ver=1", Used: 17000, Whole: 281000}},
				Images: []optimize.ImageReport{
					{URL: "/wp-content/uploads/hero.jpg", Original: 24587, State: "converted", Converted: []optimize.ConvertedReport{{Format: "avif", Bytes: 5736}}},
					{URL: "/wp-content/uploads/p3.jpg", Original: 9000, State: "not converted", Why: "its colour profile isn't sRGB (P3)"},
					{URL: "/wp-content/uploads/new.jpg", State: "not seen"},
				},
			}
		},
	})
	w := do(h, http.MethodGet, "/cache/entry?"+url.Values{"key": {page.Key}}.Encode(), "127.0.0.1:9090", nil)
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Equal(t, []string{"www.example.com/post/ <html>the page</html>"}, asked)
	for _, want := range []string{
		"<h2>Optimizations</h2>",
		`<dt>Main image on phones</dt><dd><span class="mono">/wp-content/uploads/hero-768x432.jpg</span>`,
		"<dt>Main image on desktops</dt><dd>not known yet: the page asks visitors' browsers",
		"https://www.googletagmanager.com/gtag/js?id=G-1\ninline (#fb-pixel)",
		"/wp-content/themes/t/style.css?ver=1", "94% smaller",
		"avif <span class=\"mono\">5.6 KB</span> <span class=\"muted\">77% smaller</span>",
		"not converted: its colour profile isn&#39;t sRGB (P3)",
		"not asked for by a browser which takes AVIF or WebP yet",
	} {
		assert.Contains(t, body, want)
	}

	// Not for responses other than pages
	page.ContentType = "image/png"
	asked = nil
	w = do(h, http.MethodGet, "/cache/entry?"+url.Values{"key": {page.Key}}.Encode(), "127.0.0.1:9090", nil)
	assert.NotContains(t, w.Body.String(), "Optimizations")
	assert.Empty(t, asked)
}
