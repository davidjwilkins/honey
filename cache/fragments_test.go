package cache

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func marked(token, name, content string) string {
	return fmt.Sprintf(`<!--honey:fragment name="%s" token="%s"-->%s<!--/honey:fragment-->`, name, token, content)
}

func TestParseFragments(t *testing.T) {
	token := NewFragmentToken(time.Now())
	page := "<main>Post</main><aside>" + marked(token, "posts-widget-recent-posts-2", "<ul><li>A</li></ul>") + "</aside>" +
		marked(token, "posts-block-1a2b", "<ol>B</ol>") + "<footer>"
	segments := parseFragments([]byte(page), token)
	require.Len(t, segments, 5)
	assert.Equal(t, "<main>Post</main><aside>", string(segments[0].text))
	assert.Equal(t, segment{name: "posts-widget-recent-posts-2", text: []byte("<ul><li>A</li></ul>")}, segments[1])
	assert.Equal(t, "posts-block-1a2b", segments[3].name)
	assert.Equal(t, "<footer>", string(segments[4].text))

	assert.Equal(t, `<main>Post</main><aside><!--honey:fragment name="posts-widget-recent-posts-2"--><ul><li>A</li></ul><!--/honey:fragment--></aside>`+
		`<!--honey:fragment name="posts-block-1a2b"--><ol>B</ol><!--/honey:fragment--><footer>`,
		string(first(assemble(segments, func(string) []byte { return nil }))), "the tokens are removed")
	body, assembled := assemble(segments, func(name string) []byte {
		if name == "posts-block-1a2b" {
			return []byte("<ol>C</ol>")
		}
		return nil
	})
	assert.Contains(t, string(body), "<ol>C</ol>")
	require.Len(t, assembled, 5)
	assert.Equal(t, "<ol>C</ol>", string(assembled[3].text), "the segments are the new page's")
	assert.Equal(t, "<footer>", string(assembled[4].text))
	assembled[4].text = append(assembled[4].text, '!')
	assert.Equal(t, "<footer>", string(body[len(body)-len("<footer>"):]), "and can't write past their part of it")

	for what, body := range map[string]string{
		"no fragments":         "<p>hello</p>",
		"another token":        marked(NewFragmentToken(time.Now()), "posts", "x"),
		"no token":             `<!--honey:fragment name="posts"-->x<!--/honey:fragment-->`,
		"a bad name":           marked(token, "Posts!", "x"),
		"no end":               `<!--honey:fragment name="posts" token="` + token + `"-->x`,
		"one inside another":   marked(token, "a", marked(token, "b", "x")),
		"an end without start": "x<!--/honey:fragment-->" + marked(token, "a", "x"),
		"too large":            marked(token, "a", strings.Repeat("x", maxFragmentBytes+1)),
		"too many":             strings.Repeat(marked(token, "a", "x"), maxFragmentsPerPage+1),
	} {
		assert.Nil(t, parseFragments([]byte(body), token), what)
	}
	assert.Nil(t, parseFragments([]byte(page), ""), "without a token, nothing is a fragment")
}

func TestTokenTime(t *testing.T) {
	start := time.Unix(1700000000, 123456789)
	sent, ok := tokenTime(NewFragmentToken(start))
	require.True(t, ok)
	assert.True(t, sent.Equal(start))
	_, ok = tokenTime("nonsense")
	assert.False(t, ok)
}

// fragmentPage caches a page at u with a fragment, as rendered for a
// backend request sent at sent, and returns the request
func fragmentPage(t *testing.T, c *defaultCacher, u, main, name, fragment string, sent time.Time) *http.Request {
	t.Helper()
	request := newValidRequest(u)
	token := NewFragmentToken(sent)
	request.Header.Set(FragmentTokenHeader, token)
	body := "<main>" + main + "</main>" + marked(token, name, fragment)
	response := &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/html; charset=UTF-8"}, "Cache-Control": {"max-age=600"}, "Content-Length": {fmt.Sprint(len(body))}},
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Request:    request,
	}
	c.Cache(c.Hash(request), c.Standardize(response))
	return request
}

func page(t *testing.T, c *defaultCacher, r *http.Request) (string, http.Header) {
	t.Helper()
	resp, found := c.Load(c.Hash(r), r)
	require.True(t, found, r.URL.String())
	return string(resp.Body()), resp.Header()
}

func TestFragmentsAreShared(t *testing.T) {
	c := NewCacher(Options{})
	a := fragmentPage(t, c, "https://www.example.com/a/", "A", "posts", "<li>Old post</li>", time.Now())
	body, header := page(t, c, a)
	assert.Equal(t, `<main>A</main><!--honey:fragment name="posts"--><li>Old post</li><!--/honey:fragment-->`, body)
	assert.Equal(t, fmt.Sprint(len(body)), header.Get("Content-Length"))
	etag := header.Get("Etag")

	// Another page is rendered after a new post is published
	b := fragmentPage(t, c, "https://www.example.com/b/", "B", "posts", "<li>New post</li>", time.Now())
	body, _ = page(t, c, b)
	assert.Contains(t, body, "<li>New post</li>")

	body, header = page(t, c, a)
	assert.Contains(t, body, "<main>A</main>", "the page is the same")
	assert.Contains(t, body, "<li>New post</li>", "but with the fragment's new content")
	assert.NotEqual(t, etag, header.Get("Etag"), "so it has a new Etag")
	assert.Equal(t, fmt.Sprint(len(body)), header.Get("Content-Length"))

	first, _ := c.Load(c.Hash(a), a)
	second, _ := c.Load(c.Hash(a), a)
	assert.Same(t, first, second, "it's only rebuilt once")
	assert.Len(t, c.Entries(), 2)
	assert.Len(t, c.Fragments(), 1, "one fragment, for one host")

	// Large enough to compress
	big := fragmentPage(t, c, "https://www.example.com/big/", strings.Repeat("Big page. ", 200), "posts", "<li>New post</li>", time.Now())
	fragmentPage(t, c, "https://www.example.com/b/", "B", "posts", "<li>Newer post</li>", time.Now())
	rebuilt, _ := c.Load(c.Hash(big), big)
	header, encoded := Negotiate(rebuilt, withHeader(big, "Accept-Encoding", "gzip"))
	assert.Equal(t, "gzip", header.Get("Content-Encoding"), "the rebuilt page is compressed again")
	assert.Equal(t, fmt.Sprint(len(encoded)), header.Get("Content-Length"))
	assert.Contains(t, string(rebuilt.Body()), "Newer post")
}

func withHeader(r *http.Request, name, value string) *http.Request {
	r = r.Clone(r.Context())
	r.Header.Set(name, value)
	return r
}

func TestFragmentsArePerHost(t *testing.T) {
	c := NewCacher(Options{})
	a := fragmentPage(t, c, "https://www.example.com/", "A", "posts", "www", time.Now())
	b := fragmentPage(t, c, "https://shop.example.com/", "B", "posts", "shop", time.Now())
	body, _ := page(t, c, a)
	assert.Contains(t, body, ">www<")
	body, _ = page(t, c, b)
	assert.Contains(t, body, ">shop<")
	assert.Len(t, c.Fragments(), 2)
}

func TestFragmentPurges(t *testing.T) {
	c := NewCacher(Options{})
	sent := time.Now()
	a := fragmentPage(t, c, "https://www.example.com/a/", "A", "posts-block-1", "old", sent)
	fragmentPage(t, c, "https://www.example.com/b/", "B", "posts-block-2", "other", sent)
	fragmentPage(t, c, "https://www.example.com/c/", "C", "menu", "menu", sent)

	count, sources := c.PurgeFragments("posts-*")
	assert.Equal(t, 2, count)
	assert.Equal(t, []string{"https://www.example.com/a/", "https://www.example.com/b/"}, sources, "the pages to refresh")
	count, _ = c.PurgeFragments("nothing")
	assert.Zero(t, count)

	// A page whose backend request was sent before the purge has old
	// content, even if it is cached after it
	fragmentPage(t, c, "https://www.example.com/late/", "Late", "posts-block-1", "stale", sent)
	body, _ := page(t, c, a)
	assert.Contains(t, body, ">old<")
	late, _ := page(t, c, newValidRequest("https://www.example.com/late/"))
	assert.Contains(t, late, ">old<", "a page with out of date content gets the current content")

	// The refresh
	fragmentPage(t, c, "https://www.example.com/a/", "A", "posts-block-1", "new", time.Now())
	late, _ = page(t, c, newValidRequest("https://www.example.com/late/"))
	assert.Contains(t, late, ">new<")
}

func TestFragmentsNeedHTMLAndATokenFromHoney(t *testing.T) {
	c := NewCacher(Options{})
	request := newValidRequest("https://www.example.com/feed/")
	token := NewFragmentToken(time.Now())
	request.Header.Set(FragmentTokenHeader, token)
	body := marked(token, "posts", "x")
	c.Cache(c.Hash(request), c.Standardize(&http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/rss+xml"}},
		Body: io.NopCloser(bytes.NewBufferString(body)), Request: request,
	}))
	got, _ := page(t, c, request)
	assert.Equal(t, body, got, "only HTML has fragments")
	assert.Empty(t, c.Fragments())
}

func TestTooManyFragments(t *testing.T) {
	f := newFragments()
	for i := 0; i < maxFragments; i++ {
		_, kept := f.seen("h", fmt.Sprint("f", i), []byte("x"), time.Now(), "")
		require.True(t, kept)
	}
	_, kept := f.seen("h", "one-too-many", []byte("x"), time.Now(), "")
	assert.False(t, kept)
	_, kept = f.seen("h", "f1", []byte("y"), time.Now(), "")
	assert.True(t, kept, "known fragments still change")
}

func first(body []byte, _ []segment) []byte { return body }
