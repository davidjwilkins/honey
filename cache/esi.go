package cache

import (
	"net/http"
	"strings"

	"github.com/davidjwilkins/honey/utilities"
)

// Edge Side Includes (https://www.w3.org/TR/esi-lang/) let the backend
// leave holes in a page - <esi:include src="..."/> - which are filled in
// for each visitor when the page is served, by requesting their src (see
// fetch.ESI).  The page can then be cached and shared, while the parts
// which differ between visitors (e.g. a comment form filled in with a
// commenter's name) are fetched for each of them - from the cache, if
// they're the same for everyone, or from the backend, if they're personal.
//
// Honey says it supports them by sending the backend
// "Surrogate-Capability: honey="ESI/1.0"" with requests for pages it may
// cache, and only processes a response's ESI if it says it uses them, with
// "Surrogate-Control: content="ESI/1.0"" (which isn't sent on to clients).

// SurrogateCapability is the Surrogate-Capability header Honey sends the
// backend
const SurrogateCapability = `honey="ESI/1.0"`

// ESIHeader marks a cached response as using ESI.  It is only ever set by
// the cache, and removed before responses are sent to clients.
const ESIHeader = "X-Honey-Esi"

// usesESI returns whether r is an HTML response which uses ESI, to a
// request which Honey said it supports them for
func usesESI(r *http.Response) bool {
	if r.Request == nil || !strings.Contains(r.Request.Header.Get("Surrogate-Capability"), "ESI/1.0") || !isHTML(r.Header) {
		return false
	}
	for _, sc := range r.Header.Values("Surrogate-Control") {
		if content, found := utilities.Directive(sc, "content"); found && strings.Contains(content, "ESI/1.0") {
			return true
		}
	}
	return false
}

// UsesESI returns whether resp is a cached response which uses ESI
func UsesESI(resp Response) bool {
	return resp != nil && resp.Header().Get(ESIHeader) != ""
}

// ESIEligible returns whether r - which can't be cached, because of its
// bypass cookies - may be served a cached page which uses ESI for the
// parts which differ between visitors: if every bypass cookie it has is
// one of the ESICookies.  (Whether there is such a page is up to the
// caller.)
func (c *defaultCacher) ESIEligible(r *http.Request) bool {
	if len(c.esiCookies) == 0 || (r.Method != http.MethodGet && r.Method != http.MethodHead) ||
		r.Header.Get("Authorization") != "" || c.routing(r).noCache ||
		(c.skipStaticFiles && utilities.IsStaticFile(r.URL.Path)) {
		return false
	}
	prefixes := c.bypassCookiesFor(r)
	found := false
	for _, cookie := range utilities.Cookies(r.Header) {
		if !hasAnyPrefix(cookie[0], prefixes) {
			continue
		}
		if !hasAnyPrefix(cookie[0], c.esiCookies) {
			return false
		}
		found = true
	}
	return found
}

// Derived returns what build makes of resp's body, made once for each
// cached response (e.g. the parsed form of a page which uses ESI), rather
// than for every request.  It returns nil if resp isn't a cached response.
func Derived(resp Response, build func(body []byte) any) any {
	r, ok := resp.(*responseImpl)
	if !ok {
		return nil
	}
	r.derivedOnce.Do(func() { r.derived = build(r.body) })
	return r.derived
}
