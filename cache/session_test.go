package cache

import (
	"bytes"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBypassCookiesStopCaching(t *testing.T) {
	c := NewDefaultCacher()
	for _, cookie := range []string{
		"wordpress_logged_in_0123abcd=admin%7C1700000000%7Ctoken%7Chash",
		"wp-postpass_0123abcd=%24P%24Bhash",
		"comment_author_0123abcd=Jane",
		"woocommerce_items_in_cart=1",
		"wp_woocommerce_session_0123abcd=1%7C%7C1700000000",
		// among others, and with a value r.Cookies() would reject
		`_ga=GA1.1; wordpress_logged_in_0123abcd="a b"; theme=dark`,
	} {
		r := newValidRequest("https://www.example.com/hello-world/")
		r.Header.Set("Cookie", cookie)
		assert.False(t, c.CanCache(r), cookie)
	}
	for _, cookie := range []string{"", "_ga=GA1.1", "wordpress_test_cookie=WP%20Cookie%20check", "wp-settings-1=x"} {
		r := newValidRequest("https://www.example.com/hello-world/")
		r.Header.Set("Cookie", cookie)
		assert.True(t, c.CanCache(r), cookie)
	}
}

func TestNoBypassCookiesByDefault(t *testing.T) {
	r := newValidRequest("https://www.example.com/")
	r.Header.Set("Cookie", "wordpress_logged_in_0123abcd=admin")
	assert.True(t, NewCacher(Options{}).CanCache(r), "NewCacher has no site-specific rules")
	assert.False(t, NewCacher(Options{BypassCookies: []string{"wordpress_logged_in_"}}).CanCache(r))
}

func TestRouteOverridesBypassCookies(t *testing.T) {
	c := NewCacher(Options{BypassCookies: WordPressBypassCookies})
	none := []string{}
	c.AddRoute(Route{Prefix: "/wp-content/uploads/", BypassCookies: &none})
	page := newValidRequest("https://www.example.com/hello-world/")
	page.Header.Set("Cookie", "wordpress_logged_in_0123abcd=admin")
	image := newValidRequest("https://www.example.com/wp-content/uploads/cat.jpg")
	image.Header.Set("Cookie", "wordpress_logged_in_0123abcd=admin")
	assert.False(t, c.CanCache(page))
	assert.True(t, c.CanCache(image))
}

func response(request *http.Request, header http.Header) *http.Response {
	return &http.Response{
		Status:     "200 OK",
		StatusCode: http.StatusOK,
		Header:     header,
		Body:       io.NopCloser(bytes.NewBufferString("hello")),
		Request:    request,
	}
}

func TestStartsSession(t *testing.T) {
	c := NewDefaultCacher()
	r := newValidRequest("https://www.example.com/shop/?add-to-cart=12")
	assert.True(t, c.StartsSession(response(r, http.Header{"Set-Cookie": {
		"_ga=1; Path=/",
		"wp_woocommerce_session_0123abcd=1%7C%7C1700000000; Path=/; HttpOnly",
	}})))
	assert.False(t, c.StartsSession(response(r, http.Header{})))
}

func TestUnknownCookiesStartSessions(t *testing.T) {
	c := NewCacher(Options{BypassCookies: WordPressBypassCookies, StripCookies: []string{"pll_", "_ga"}})
	c.AddAllowedCookie("lang")
	r := newValidRequest("https://www.example.com/")
	for _, tc := range []struct {
		header http.Header
		starts bool
	}{
		{http.Header{"Set-Cookie": {"PHPSESSID=abc; Path=/"}}, true},
		{http.Header{"Set-Cookie": {"lang=fr; Path=/", "pll_language=fr; Path=/", "_ga=1"}}, false},
		{http.Header{"Set-Cookie": {"lang=fr; Path=/", "PHPSESSID=abc"}}, true},
		// The response says its Set-Cookie is only for this requester
		{http.Header{"Set-Cookie": {"PHPSESSID=abc"}, "Cache-Control": {`private="set-cookie", max-age=60`}}, false},
		{http.Header{"Set-Cookie": {"PHPSESSID=abc"}, "Cache-Control": {`no-cache="Set-Cookie"`}}, false},
		// but not for bypass cookies, which mark personal pages
		{http.Header{"Set-Cookie": {"wp_woocommerce_session_1=x"}, "Cache-Control": {`private="set-cookie"`}}, true},
	} {
		assert.Equal(t, tc.starts, c.StartsSession(response(r, tc.header)), "%v", tc.header)
	}
}

func TestPrivateSetCookieReachesTheRequester(t *testing.T) {
	c := NewCacher(Options{})
	r := newValidRequest("https://www.example.com/")
	resp := response(r, http.Header{
		"Cache-Control": {`private="set-cookie", max-age=600`},
		"Set-Cookie":    {"PHPSESSID=abc; Path=/"},
	})
	standardized := c.Standardize(resp)
	assert.Equal(t, []string{"PHPSESSID=abc; Path=/"}, resp.Header.Values("Set-Cookie"))
	assert.Empty(t, standardized.Header().Values("Set-Cookie"), "but it isn't cached")
}

func TestPrivateResponsesAreNotStored(t *testing.T) {
	for _, tc := range []struct {
		cacheControl string
		stored       bool
	}{
		{"private", false},
		{"private, max-age=600", false},
		{"max-age=600, PRIVATE", false},
		{`private="x-user"`, false},
		{"no-store", false},
		{"max-age=600", true},
		{`private="set-cookie", max-age=600`, true},
	} {
		c := NewCacher(Options{})
		r := cachePage(t, c, "https://www.example.com/", "hello", http.Header{"Cache-Control": {tc.cacheControl}})
		_, found := c.Load(c.Hash(r), r)
		assert.Equal(t, tc.stored, found, tc.cacheControl)
	}
}

func TestPrivateSetCookieIsNotStored(t *testing.T) {
	c := NewCacher(Options{})
	c.AddAllowedCookie("lang")
	r := cachePage(t, c, "https://www.example.com/", "hello", http.Header{
		"Cache-Control": {`private="set-cookie", max-age=600`},
		"Set-Cookie":    {"lang=en; Path=/"},
	})
	assert.Empty(t, load(t, c, r).Header().Values("Set-Cookie"))
}

func TestResponsesForBypassCookiesAreNotStored(t *testing.T) {
	c := NewCacher(Options{BypassCookies: WordPressBypassCookies})
	// e.g. by a library user calling Cache directly
	r := cachePage(t, c, "https://www.example.com/", "Howdy, admin", http.Header{"Cache-Control": {"max-age=600"}},
		"Cookie", "wordpress_logged_in_0123abcd=admin")
	r.Header.Del("Cookie")
	_, found := c.Load(c.Hash(r), r)
	assert.False(t, found)
}

func TestSnapshotSkipsResponsesWhichShouldntBeStored(t *testing.T) {
	// as an earlier version would have cached them
	old := NewCacher(Options{})
	cachePage(t, old, "https://www.example.com/logged-in/", "Howdy, admin", http.Header{"Cache-Control": {"max-age=600"}},
		"Cookie", "wordpress_logged_in_0123abcd=admin")
	private := newValidRequest("https://www.example.com/private/")
	old.store.set(old.Hash(private)+"private", old.Standardize(response(private, http.Header{"Cache-Control": {"private, max-age=600"}})), 100, time.Time{})
	public := cachePage(t, old, "https://www.example.com/public/", "Hello", http.Header{"Cache-Control": {"max-age=600"}})
	path := filepath.Join(t.TempDir(), "cache.snapshot")
	saved, err := old.SaveTo(path)
	require.NoError(t, err)
	require.Equal(t, 3, saved)

	c := NewCacher(Options{BypassCookies: WordPressBypassCookies})
	loaded, err := c.LoadFrom(path)
	require.NoError(t, err)
	assert.Equal(t, 1, loaded)
	assert.Equal(t, "Hello", string(load(t, c, public).Body()))
}
