package fetch

import (
	"io"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hintsOrigin serves pages which use a stylesheet of their own, and the
// theme's
func hintsOrigin() *origin {
	return newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		io.WriteString(w, `<html><head><link rel="stylesheet" href="/theme.css"><link rel="stylesheet" href="`+r.URL.Path+`page.css"></head><body>`+r.URL.Path+`</body></html>`)
	})
}

// hintedGet requests rawURL as a browser would, and returns the response,
// and the Link headers of any Early Hints sent before it
func hintedGet(t *testing.T, rawURL string, headers ...string) (*http.Response, []string) {
	t.Helper()
	var hints []string
	trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
		if code == http.StatusEarlyHints {
			hints = append(hints, header.Values("Link")...)
		}
		return nil
	}}
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	require.NoError(t, err)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp, hints
}

func hintsProxy(t *testing.T, earlyHints bool) string {
	return routeProxy(t, hintsOrigin(), cache.Options{BypassCookies: cache.WordPressBypassCookies, DefaultTTL: time.Hour},
		Options{EarlyHints: earlyHints}).URL
}

var postHints = []string{"</theme.css>; rel=preload; as=style", "</post/page.css>; rel=preload; as=style"}

func TestEarlyHintsForUncachedRequests(t *testing.T) {
	proxy := hintsProxy(t, true)
	resp, hints := hintedGet(t, proxy+"/post/")
	assert.Equal(t, "MISS", resp.Header.Get("X-Honey-Cache"))
	assert.Empty(t, hints, "nothing is known about the page yet")

	resp, hints = hintedGet(t, proxy+"/post/")
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"))
	assert.Empty(t, hints, "a cached page doesn't keep anyone waiting")

	resp, hints = hintedGet(t, proxy+"/post/", "Cookie", "wordpress_logged_in_abc=admin")
	assert.Equal(t, "NO-CACHE", resp.Header.Get("X-Honey-Cache"))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, postHints, hints, "a logged in user waits for WordPress, so gets the cached page's hints")
	assert.Empty(t, resp.Header.Values("Link"), "they're for the 103, not the page")

	_, hints = hintedGet(t, proxy+"/post/?preview=1", "Cookie", "wordpress_logged_in_abc=admin")
	assert.Empty(t, hints, "another page, and too few pages have been cached to know which hints all of them have")

	_, hints = hintedGet(t, proxy+"/post/", "Cookie", "wordpress_logged_in_abc=admin", "Accept", "application/json")
	assert.Empty(t, hints, "only for browsers loading pages")
}

func TestEarlyHintsForOtherPages(t *testing.T) {
	proxy := hintsProxy(t, true)
	for _, path := range []string{"/", "/a/", "/b/"} {
		hintedGet(t, proxy+path)
	}
	resp, hints := hintedGet(t, proxy+"/checkout/", "Cookie", "woocommerce_items_in_cart=1")
	assert.Equal(t, "NO-CACHE", resp.Header.Get("X-Honey-Cache"))
	assert.Equal(t, []string{"</theme.css>; rel=preload; as=style"}, hints, "a page which isn't cached gets those every page has")

	_, hints = hintedGet(t, proxy+"/c/")
	assert.Equal(t, []string{"</theme.css>; rel=preload; as=style"}, hints, "as does a page which isn't cached yet")
}

func TestEarlyHintsAreOffByDefault(t *testing.T) {
	proxy := hintsProxy(t, false)
	hintedGet(t, proxy+"/post/")
	_, hints := hintedGet(t, proxy+"/post/", "Cookie", "wordpress_logged_in_abc=admin")
	assert.Empty(t, hints)
}

func TestEarlyHintsForESIPages(t *testing.T) {
	o := newESIOrigin(true, "/post/?honey-esi=comments")
	inner := o.origin.handler
	o.origin.handler = func(w http.ResponseWriter, r *http.Request, hit int) {
		if r.URL.Query().Get(ESIParam) == "" {
			w.Header().Set("Content-Type", "text/html")
			if strings.Contains(r.Header.Get("Surrogate-Capability"), "ESI/1.0") {
				w.Header().Set("Surrogate-Control", `content="ESI/1.0"`)
			}
			io.WriteString(w, `<head><link rel="stylesheet" href="/theme.css"></head><esi:include src="/post/?honey-esi=comments"/>`)
			return
		}
		inner(w, r, hit)
	}
	proxy := esiProxy(t, o.origin, cache.Options{DefaultTTL: time.Hour, ESICookies: []string{"comment_author_"}}, Options{EarlyHints: true})
	hintedGet(t, proxy+"/post/")
	resp, hints := hintedGet(t, proxy+"/post/", "Cookie", "comment_author_abc=Jane")
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"))
	assert.Equal(t, []string{"</theme.css>; rel=preload; as=style"}, hints, "the page waits for its includes")
}
