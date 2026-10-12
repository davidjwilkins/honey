package cache

import (
	"bytes"
	"net/http"
	"regexp"
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
// the cache, and removed before responses are sent to clients.  Its value
// is how (see esiMark): "all" or "personal", then the bypass cookies the
// page is for, if it says.
const ESIHeader = "X-Honey-Esi"

// ESICookiesHeader is the response header with which a page which uses ESI
// says whose personal parts its includes are: the bypass cookies (name
// prefixes) of the visitors who may be served it, e.g.
// "X-Honey-Esi-Cookies: comment_author_" for a page whose comments are an
// include.  It may be sent more than once.  Without it, a page is for all
// visitors with ESICookies.  It isn't sent on to clients.
const ESICookiesHeader = "X-Honey-Esi-Cookies"

// esiComment matches <!--esi ... --> comments, whose content is only seen
// by an ESI processor
var esiComment = regexp.MustCompile(`(?s)<!--esi.*?-->`)

// esiMark returns the value of ESIHeader for a page which uses ESI, from
// its header and body:
//
//   - "all" if every visitor gets it with its ESI processed
//   - "personal" if only visitors with bypass cookies do: if it says whose
//     personal parts it has (with ESICookiesHeader), and all its ESI is in
//     <!--esi ... --> comments, so that it's as it should be for everyone
//     else as it is (as a browser would show it without ESI).  It's then
//     served to them like any other page, compressed once.
//
// followed by the bypass cookies it's for, if it says.
func esiMark(header http.Header, body []byte) string {
	lines := header.Values(ESICookiesHeader)
	if len(lines) == 0 {
		return "all"
	}
	var cookies []string
	for _, line := range lines {
		for _, name := range strings.Split(line, ",") {
			if name = strings.TrimSpace(name); name != "" {
				cookies = append(cookies, name)
			}
		}
	}
	mode := "personal"
	if bytes.Contains(esiComment.ReplaceAll(body, nil), []byte("<esi:")) {
		mode = "all"
	}
	return strings.Join(append([]string{mode}, cookies...), " ")
}

// esiCookiesOf returns the bypass cookies a page which uses ESI is for, as
// its ESIHeader says, and whether it says
func esiCookiesOf(mark string) ([]string, bool) {
	fields := strings.Fields(mark)
	if len(fields) < 2 {
		return nil, false
	}
	return fields[1:], true
}

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

// ESIServes returns whether resp - a cached page - may be served to r, which
// is ESIEligible: if it uses ESI, for all the bypass cookies r has (see
// ESICookiesHeader).
func (c *defaultCacher) ESIServes(resp Response, r *http.Request) bool {
	if !UsesESI(resp) {
		return false
	}
	covered, says := esiCookiesOf(resp.Header().Get(ESIHeader))
	if !says {
		return true
	}
	prefixes := c.bypassCookiesFor(r)
	for _, cookie := range utilities.Cookies(r.Header) {
		if hasAnyPrefix(cookie[0], prefixes) && !hasAnyPrefix(cookie[0], covered) {
			return false
		}
	}
	return true
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
