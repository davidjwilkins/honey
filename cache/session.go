package cache

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/davidjwilkins/honey/utilities"
)

// WordPressBypassCookies are the cookies (name prefixes) which WordPress
// and WooCommerce give visitors whose pages may be personal: logged in
// users, commenters (whose details are filled in on comment forms),
// visitors who have entered a post's password, and shoppers with a cart.
var WordPressBypassCookies = []string{
	"wordpress_logged_in_",
	"wp-postpass_",
	"comment_author_",
	"woocommerce_items_in_cart",
	"wp_woocommerce_session_",
}

// bypassCookiesFor returns the bypass cookies which apply to r: its
// route's, or else the cacher's.
func (c *defaultCacher) bypassCookiesFor(r *http.Request) []string {
	if r.URL != nil {
		if route := c.Route(r); route != nil && route.BypassCookies != nil {
			return *route.BypassCookies
		}
	}
	return c.bypassCookies
}

// bypassCookie returns the name of a cookie which r sends, and which
// marks it as being from a visitor whose pages may be personal, or "".
func (c *defaultCacher) bypassCookie(r *http.Request) string {
	prefixes := c.bypassCookiesFor(r)
	if len(prefixes) == 0 {
		return ""
	}
	// Parsed by hand rather than with r.Cookies(), which skips cookies
	// with values it considers invalid
	for _, line := range r.Header.Values("Cookie") {
		for _, pair := range strings.Split(line, ";") {
			name, _, _ := strings.Cut(pair, "=")
			if name = strings.TrimSpace(name); hasAnyPrefix(name, prefixes) {
				return name
			}
		}
	}
	return ""
}

// StartsSession returns whether resp sets one of the bypass cookies for
// its request.  Such a response is the first of a personal session (e.g.
// a WooCommerce add-to-cart link), so it mustn't be cached, nor have the
// cookie removed.
func (c *defaultCacher) StartsSession(resp *http.Response) bool {
	req := resp.Request
	if req == nil {
		req = &http.Request{}
	}
	prefixes := c.bypassCookiesFor(req)
	if len(prefixes) == 0 {
		return false
	}
	for _, line := range resp.Header.Values("Set-Cookie") {
		name, _, _ := strings.Cut(line, "=")
		if hasAnyPrefix(strings.TrimSpace(name), prefixes) {
			return true
		}
	}
	return false
}

func hasAnyPrefix(name string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// storable returns whether a shared cache may store r: it mustn't be
// marked private or no-store, nor have been fetched for a request with a
// bypass cookie (in case it is personal, without saying so).
func (c *defaultCacher) storable(r Response) bool {
	cc := r.Header().Get("Cache-Control")
	if _, noStore := utilities.Directive(cc, "no-store"); noStore {
		return false
	}
	// https://www.rfc-editor.org/rfc/rfc9111#section-5.2.2.7 - private
	// with a list of fields only makes those private.  Set-Cookie is the
	// only one allowed for, as it isn't cached anyway.
	if fields, private := utilities.Directive(cc, "private"); private && !strings.EqualFold(fields, "set-cookie") {
		return false
	}
	return c.bypassCookie(fetchedFor(r)) == ""
}

// fetchedFor returns (as nearly as it can) the request r was fetched for
func fetchedFor(r Response) *http.Request {
	impl, ok := r.(*responseImpl)
	if !ok {
		return &http.Request{Header: r.RequestHeaders()}
	}
	if impl.response != nil && impl.response.Request != nil {
		return impl.response.Request
	}
	req := &http.Request{Header: impl.requestHeaders}
	if req.Header == nil {
		req.Header = http.Header{}
	}
	// The base key is the method and URL (see baseKey)
	if method, rawURL, found := strings.Cut(impl.baseKey, " :: "); found {
		if u, err := url.Parse(rawURL); err == nil {
			req.Method, req.URL = method, u
		}
	}
	return req
}
