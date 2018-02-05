package fetch

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"github.com/davidjwilkins/honey/cache"
	"github.com/vulcand/oxy/forward"
)

// Fetch will fetch and save responses from the cache, if possible.
// If not, it will attempt to do a single request from the backend,
// and multiplex the response to all requesters.
func Fetch(c cache.Cacher, handler http.Handler, backend *url.URL) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		SwitchBackend(r, backend)
		// CanCache tells us if this *Cache* is able to cache the request.
		// I.e. There are no *custom* rules preventing it.  Even if it returns
		// true, the request itself may still not be cacheable.
		cacheable := c.CanCache(r)
		if cacheable {
			// ResponeFromCache will always return the hash, and responded will
			// tell us if we were able to respond via the cache.  It will return
			// false if the cache entry does not yet exist, or if the request
			// is not eligible for cacheing (due to Cache-Control: No-Cache, for
			// example).
			hash, responded, revalidate := RespondFromCache(c, w, r)
			if responded {
				return
			} else if revalidate {
				w = httptest.NewRecorder()
			} else {
				// https://www.w3.org/Protocols/rfc2616/rfc2616-sec14.html#sec14.9.4
				// If we couldn't respond from the cache, and they only want it if
				// it is cached, then exit with a 504 per the spec.
				if strings.Contains(r.Header.Get("Cache-Control"), "only-if-cached") {
					w.WriteHeader(http.StatusGatewayTimeout)
					return
				}
			}
			// RespondFromMultiplexer will return true if there was an in-flight
			// request with the same hash, and we were able to respond with it's
			// response.  It will block until the in-flight request has completed.
			responded = RespondFromMultiplexer(hash, c, w, r, Fetch(c, handler, backend))
			if responded {
				return
			}
		} else {
			w.Header().Set("X-Honey-Cache", "NO-CACHE")
		}
		handler.ServeHTTP(w, r)
	}
}

// Forwarder returns a new forward.Forwarder which saves responses
// into cache.Cacher c.  It panics if it cannot create the forwarder.
func Forwarder(c cache.Cacher) http.Handler {
	forwarder, err := forward.New(
		forward.ResponseModifier(FlushMultiplexer(c, nil)),
	)
	if err != nil {
		panic(err)
	}
	return forwarder
}

// SwitchBackend changes the host and scheme of a request
// to match the backend that it should be forwarder to.
// We have to do this before Rewrite is called by forward
// because otherwise we can't match the URL in the cache or
// multiplexer.  It sets the X-Forwarded-Proto header if not
// already set to indicate the protocol (HTTP or HTTPS) that a
// client used to connect, and sets the Host header to indicate
// the actual hostname requested.
func SwitchBackend(req *http.Request, backend *url.URL) {
	req.Host = req.URL.Host
	req.URL.Host = backend.Host
	if req.Header.Get("X-Forwarded-Proto") == "" {
		req.Header.Add("X-Forwarded-Proto", req.URL.Scheme)
	}
	req.URL.Scheme = backend.Scheme
}
