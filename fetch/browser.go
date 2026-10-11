package fetch

import (
	"net/http"

	"github.com/davidjwilkins/honey/cache"
)

// browserCacher is implemented by Cachers which can send clients a
// different Cache-Control header to the one they cache by
type browserCacher interface {
	BrowserCacheControl(r *http.Request) string
}

// setBrowserCacheControl replaces the Cache-Control header in h - the
// headers being sent for resp, a response to r which is served from (or
// stored in) the cache - with the one c is configured to send clients, if
// any.  Only successful responses get it, so that browsers don't keep
// e.g. a 404 for as long as a versioned asset.
func setBrowserCacheControl(c cache.Cacher, r *http.Request, h http.Header, resp cache.Response) {
	bc, ok := c.(browserCacher)
	if !ok {
		return
	}
	cc := bc.BrowserCacheControl(r)
	if cc == "" {
		return
	}
	if code := resp.StatusCode(); code != http.StatusOK && code != http.StatusNonAuthoritativeInfo {
		return
	}
	h.Set("Cache-Control", cc)
	// It says how long from now browsers may keep the response, so they
	// shouldn't take how long Honey has had it off, nor use an Expires
	// worked out from the cached Cache-Control
	h.Del("Age")
	h.Del("Expires")
}
