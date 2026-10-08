package fetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"github.com/davidjwilkins/honey/cache"
	"github.com/davidjwilkins/honey/singleflight"
	"github.com/vulcand/oxy/forward"
	"github.com/vulcand/oxy/utils"
)

// Fetch will fetch and save responses from the cache, if possible.
// If not, it will attempt to do a single request from the backend,
// and multiplex the response to all requesters.
func Fetch(c cache.Cacher, handler http.Handler, backend *url.URL) http.HandlerFunc {
	serve := serveFromCache(c, handler)
	return func(w http.ResponseWriter, r *http.Request) {
		SwitchBackend(r, backend)
		serve(w, r)
	}
}

// serveFromCache returns a handler for requests which have already been
// switched to the backend.
func serveFromCache(c cache.Cacher, handler http.Handler) http.HandlerFunc {
	var serve http.HandlerFunc
	serve = func(w http.ResponseWriter, r *http.Request) {
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
			if revalidate {
				// The stale response has been sent; refresh it without
				// making this requester wait.  The original request's
				// context is cancelled once it has been responded to, so
				// it can't be used for the backend request.
				go revalidateInBackground(hash, c, handler, serve, r.Clone(context.Background()))
				return
			}
			if responded {
				return
			}
			// https://www.w3.org/Protocols/rfc2616/rfc2616-sec14.html#sec14.9.4
			// If we couldn't respond from the cache, and they only want it if
			// it is cached, then exit with a 504 per the spec.
			if strings.Contains(r.Header.Get("Cache-Control"), "only-if-cached") {
				w.WriteHeader(http.StatusGatewayTimeout)
				return
			}
			// RespondFromSingleflight will return true if there was an in-flight
			// request with the same hash, and we were able to respond with it's
			// response.  It will block until the in-flight request has completed.
			responded = RespondFromSingleflight(hash, c, w, r, serve)
			if responded {
				return
			}
			r = withHash(r, hash)
		} else {
			w.Header().Set("X-Honey-Cache", "NO-CACHE")
		}
		handler.ServeHTTP(w, r)
	}
	return serve
}

// revalidateInBackground fetches a fresh copy of the response to r into
// the cache, unless a request for it is already in flight.
func revalidateInBackground(hash string, c cache.Cacher, handler http.Handler, serve http.HandlerFunc, r *http.Request) {
	if _, inFlight := singleflights.Load(hash); inFlight {
		return
	}
	// We want the full response to cache, not a 304 for this client
	r.Header.Del("If-None-Match")
	r.Header.Del("If-Modified-Since")
	w := httptest.NewRecorder()
	if RespondFromSingleflight(hash, c, w, r, serve) {
		return
	}
	handler.ServeHTTP(w, withHash(r, hash))
}

type hashKey struct{}

// withHash records the hash under which r's singleflight is stored, so
// that it can be found again once the backend responds, even if the
// cacher would now hash the request differently (e.g. because the Vary
// header it last saw for the URL has since been evicted).
func withHash(r *http.Request, hash string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), hashKey{}, hash))
}

// requestHash returns the hash recorded by withHash, or else hashes r.
func requestHash(c cache.Cacher, r *http.Request) string {
	if hash, ok := r.Context().Value(hashKey{}).(string); ok {
		return hash
	}
	return c.Hash(r)
}

// Forwarder returns a new forward.Forwarder which saves responses
// into cache.Cacher c.  It panics if it cannot create the forwarder.
func Forwarder(c cache.Cacher) http.Handler {
	forwarder, err := forward.New(
		forward.ResponseModifier(FlushSingleflight(c, nil)),
		forward.ErrorHandler(backendErrorHandler{c}),
	)
	if err != nil {
		panic(err)
	}
	return forwarder
}

// backendErrorHandler handles requests for which the backend could not
// be reached.  If the cache has a response which stale-if-error allows to
// be served, it serves that; otherwise it responds with an error.  Either
// way, any requests waiting on the same singleflight get the same response.
type backendErrorHandler struct {
	c cache.Cacher
}

func (h backendErrorHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, err error) {
	if !h.c.CanCache(r) {
		utils.DefaultHandler.ServeHTTP(w, r, err)
		return
	}
	hash := requestHash(h.c, r)
	var multi singleflight.Singleflight
	if m, found := singleflights.Load(hash); found {
		multi = m.(singleflight.Singleflight)
		singleflights.Delete(hash)
	}
	if prev, found := h.c.Load(hash, r); found && canServeStaleOnError(prev, "") {
		if multi != nil {
			go multi.Write(prev)
		}
		for key, values := range prev.Header() {
			w.Header()[key] = append([]string(nil), values...)
		}
		w.Header().Set("Age", prev.Age())
		w.Header().Set("Warning", staleWarning())
		w.Header().Set("X-Honey-Cache", "STALE")
		w.Header().Set("X-Honey-Stale", fmt.Sprintf("Backend error: %v", err))
		w.WriteHeader(prev.StatusCode())
		w.Write(prev.Body())
		return
	}
	if multi != nil {
		statusCode := http.StatusBadGateway
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			statusCode = http.StatusGatewayTimeout
		}
		multi.Abort(statusCode)
	}
	utils.DefaultHandler.ServeHTTP(w, r, err)
}

// SwitchBackend changes the host and scheme of a request
// to match the backend that it should be forwarder to.
// We have to do this before Rewrite is called by forward
// because otherwise we can't match the URL in the cache or
// singleflight.  It sets the X-Forwarded-Proto header if not
// already set to indicate the protocol (HTTP or HTTPS) that a
// client used to connect, and sets the Host header to indicate
// the actual hostname requested.
func SwitchBackend(req *http.Request, backend *url.URL) {
	req.Host = req.URL.Host
	req.URL.Host = backend.Host
	if req.Header.Get("X-Forwarded-Proto") == "" {
		proto := req.URL.Scheme
		if proto == "" {
			// Server requests don't have a scheme in their URL
			proto = "http"
			if req.TLS != nil {
				proto = "https"
			}
		}
		req.Header.Set("X-Forwarded-Proto", proto)
	}
	req.URL.Scheme = backend.Scheme
}
