package fetch

import (
	"net/http"
	"strings"

	"github.com/davidjwilkins/honey/cache"
)

// earlyHinter is implemented by Cachers which can say which Early Hints to
// send for pages which aren't cached (see cache/hints.go)
type earlyHinter interface {
	Peek(hash string) (cache.Response, bool)
	CommonEarlyHints(host string) []string
}

// wantsEarlyHints returns whether r is from a browser loading a page, which
// Early Hints would help, rather than e.g. an ESI include or an API call
func wantsEarlyHints(r *http.Request) bool {
	return r.Method == http.MethodGet && esiDepth(r.Context()) == 0 && r.Header.Get("Range") == "" &&
		strings.Contains(r.Header.Get("Accept"), "text/html")
}

// sendEarlyHints sends links as a 103 Early Hints response, so that the
// browser can fetch them while the page is fetched from the backend
func sendEarlyHints(w http.ResponseWriter, links []string) {
	if len(links) == 0 {
		return
	}
	h := w.Header()
	existing := h.Values("Link")
	h.Del("Link")
	for _, link := range links {
		h.Add("Link", link)
	}
	w.WriteHeader(http.StatusEarlyHints)
	// They're for the 103, not the page
	h.Del("Link")
	for _, link := range existing {
		h.Add("Link", link)
	}
}

// hintsFor returns the Early Hints for r, which is about to be sent to the
// backend: those of the page cached for it, or for visitors without its
// cookies, if there is one (even if it's too old to serve), or else those
// all of its host's recently cached pages have.
func hintsFor(c cache.Cacher, r *http.Request, filterQuery func(*http.Request)) []string {
	h, ok := c.(earlyHinter)
	if !ok {
		return nil
	}
	anonymous := r.Clone(r.Context())
	anonymous.Header.Del("Cookie")
	filterQuery(anonymous)
	if c.CanCache(anonymous) {
		if resp, found := h.Peek(c.Hash(anonymous)); found {
			if hints := cache.EarlyHints(resp); hints != nil {
				return hints
			}
		}
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return h.CommonEarlyHints(host)
}
