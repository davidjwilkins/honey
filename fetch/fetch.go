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
	"github.com/davidjwilkins/honey/utilities"
	"github.com/vulcand/oxy/forward"
	"github.com/vulcand/oxy/utils"
)

// Fetch will fetch and save responses from the cache, if possible.
// If not, it will attempt to do a single request from the backend,
// and multiplex the response to all requesters.  No requests are trusted
// to control the cache: see FetchWithControl.
func Fetch(c cache.Cacher, handler http.Handler, backend *url.URL) http.HandlerFunc {
	return FetchWithControl(c, handler, backend, Control{})
}

// FetchWithControl is like Fetch, but requests which control trusts may
// purge the cache (with the PURGE method), and make it fetch a fresh
// response (with Cache-Control: no-cache or Pragma: no-cache).
func FetchWithControl(c cache.Cacher, handler http.Handler, backend *url.URL, control Control) http.HandlerFunc {
	return FetchWithOptions(c, handler, backend, Options{Control: control})
}

// Options configures FetchWithOptions.
type Options struct {
	// Control says which requests may purge the cache, or make it fetch
	// fresh responses.
	Control Control
	// QueryParams, if not nil, are the only query parameters which matter.
	// Any others are removed from cacheable requests before they are
	// looked up in the cache or sent to the backend, so that they can't
	// be used to get around the cache (e.g. ?nocache=<random>), and so
	// that tracking parameters (e.g. utm_source) don't split the cache.
	// Parameters are also sorted, so their order doesn't matter.  A nil
	// QueryParams keeps every parameter.
	QueryParams []string
}

// FetchWithOptions is like Fetch, configured by opts.
func FetchWithOptions(c cache.Cacher, handler http.Handler, backend *url.URL, opts Options) http.HandlerFunc {
	filterQuery := queryFilter(opts.QueryParams)
	serve := serveFromCache(c, handler, filterQuery)
	return func(w http.ResponseWriter, r *http.Request) {
		SwitchBackend(r, backend)
		if r.Method == MethodPurge {
			// so that it matches the cache keys
			filterQuery(r)
		}
		if opts.Control.applyControl(c, w, r) {
			return
		}
		serve(w, r)
	}
}

// queryFilter returns a function which removes the query parameters not in
// keep from a request, and sorts the rest.  If keep is nil, it does nothing.
func queryFilter(keep []string) func(*http.Request) {
	if keep == nil {
		return func(*http.Request) {}
	}
	kept := map[string]bool{}
	for _, name := range keep {
		kept[name] = true
	}
	return func(r *http.Request) {
		if r.URL.RawQuery == "" {
			return
		}
		query := r.URL.Query()
		for name := range query {
			if !kept[name] {
				delete(query, name)
			}
		}
		r.URL.RawQuery = query.Encode()
		r.URL.ForceQuery = false
		// The forwarder uses RequestURI, when it's set, rather than URL
		if r.RequestURI != "" {
			r.RequestURI = r.URL.RequestURI()
		}
	}
}

// serveFromCache returns a handler for requests which have already been
// switched to the backend.
func serveFromCache(c cache.Cacher, handler http.Handler, filterQuery func(*http.Request)) http.HandlerFunc {
	var serve http.HandlerFunc
	serve = func(w http.ResponseWriter, r *http.Request) {
		// CanCache tells us if this *Cache* is able to cache the request.
		// I.e. There are no *custom* rules preventing it.  Even if it returns
		// true, the request itself may still not be cacheable.
		cacheable := c.CanCache(r)
		if cacheable {
			// Only cacheable requests are filtered, so that e.g. rules
			// against caching ?preview=true still see the whole query
			filterQuery(r)
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
				go revalidateInBackground(hash, handler, r.Clone(context.Background()))
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
			// A range request for something not in the cache is most likely
			// seeking in large media, so let the backend handle it rather
			// than fetching the whole thing.
			if r.Header.Get("Range") != "" {
				handler.ServeHTTP(w, withBypass(r))
				return
			}
			// If the same resource is already being fetched, wait for it
			f, leader := flights.Join(hash)
			if !leader {
				awaitFlight(w, r, f, c, handler, serve)
				return
			}
			// The Flight is normally finished as soon as the backend
			// responds (or errors); this makes sure nobody is left waiting
			// whatever happens.
			defer flights.Finish(hash, f, singleflight.Result{Retry: true})
			handler.ServeHTTP(w, forBackend(r, hash, f))
			return
		}
		w.Header().Set("X-Honey-Cache", "NO-CACHE")
		handler.ServeHTTP(w, r)
	}
	return serve
}

// flights holds the backend requests in progress, which other requests
// for the same resource wait for rather than making their own.
var flights singleflight.Group

// awaitFlight waits for Flight f, which is fetching the response to r,
// and then responds to r according to its Result.
func awaitFlight(w http.ResponseWriter, r *http.Request, f *singleflight.Flight, c cache.Cacher, handler http.Handler, serve http.HandlerFunc) {
	select {
	case <-f.Done():
	case <-r.Context().Done():
		return
	}
	result := f.Result()
	switch {
	case result.Bypass || (result.Response != nil && !shareable(result.Response)):
		handler.ServeHTTP(w, withBypass(r))
	case result.Retry || (result.Response != nil && !matchesVary(c, result.Response, r)):
		// Try again: this will most likely be served from the cache, or
		// start (or join) a Flight for this request's variant.  If that
		// doesn't work either, give up on sharing.
		if isRetry(r) {
			handler.ServeHTTP(w, withBypass(r))
			return
		}
		serve(w, withRetry(r))
	case result.Response != nil:
		writeShared(w, r, result.Response, result.Stale)
	default:
		statusCode := result.StatusCode
		if statusCode == 0 {
			statusCode = http.StatusBadGateway
		}
		http.Error(w, http.StatusText(statusCode), statusCode)
	}
}

// shareable returns whether resp may be given to requests other than the
// one it was fetched for.
func shareable(resp cache.Response) bool {
	cc := resp.Header().Get("Cache-Control")
	if _, private := utilities.Directive(cc, "private"); private {
		return false
	}
	if _, noStore := utilities.Directive(cc, "no-store"); noStore {
		return false
	}
	return !headerListContains(resp.Header().Values("Vary"), "*")
}

// matchesVary returns whether r has the same values as the request resp
// was fetched for, for the headers resp Varies on (other than
// Accept-Encoding, which each requester gets their own of).
func matchesVary(c cache.Cacher, resp cache.Response, r *http.Request) bool {
	vary := utilities.CacheVary(strings.Join(resp.Header().Values("Vary"), ","))
	if vary == "" {
		return true
	}
	fetchedFor := &http.Request{Header: resp.RequestHeaders()}
	return utilities.GetVaryHeadersHash(fetchedFor.Header, fetchedFor, c.AllowedCookies(), vary) ==
		utilities.GetVaryHeadersHash(r.Header, r, c.AllowedCookies(), vary)
}

func headerListContains(values []string, name string) bool {
	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(item), name) {
				return true
			}
		}
	}
	return false
}

// writeShared responds to r with resp, which was fetched for another request.
func writeShared(w http.ResponseWriter, r *http.Request, resp cache.Response, stale bool) {
	header, body := cache.Negotiate(resp, r)
	for key, values := range header {
		w.Header()[key] = append([]string(nil), values...)
	}
	w.Header().Set("Age", resp.Age())
	if stale {
		w.Header().Set("Warning", staleWarning())
		w.Header().Set("X-Honey-Cache", "STALE")
	} else {
		w.Header().Set("X-Honey-Cache", "MISS (MULTIPLEXED)")
	}
	if isNotModified(r, header.Get("Etag")) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(resp.StatusCode())
	w.Write(body)
}

type retryKey struct{}

// withRetry marks r as having already waited for one Flight, so that it
// doesn't wait for another.
func withRetry(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), retryKey{}, true))
}

func isRetry(r *http.Request) bool {
	retry, _ := r.Context().Value(retryKey{}).(bool)
	return retry
}

// revalidateInBackground fetches a fresh copy of the response to r into
// the cache, unless a request for it is already in flight.
func revalidateInBackground(hash string, handler http.Handler, r *http.Request) {
	f, leader := flights.Join(hash)
	if !leader {
		return
	}
	defer flights.Finish(hash, f, singleflight.Result{Retry: true})
	handler.ServeHTTP(httptest.NewRecorder(), forBackend(r, hash, f))
}

// conditionalHeaders make the backend respond with something other than
// the full response - such as a 304 Not Modified, a 206 Partial Content
// or a 412 Precondition Failed - which is specific to one requester, and
// so mustn't be cached and served to everyone.
//
// Accept-Encoding is also left out, so that the backend's response is
// unencoded: the cache encodes it for each client itself.  (Go's
// http.Transport still asks the backend for gzip, and decodes it.)
var conditionalHeaders = []string{
	"Accept-Encoding",
	"If-Match",
	"If-None-Match",
	"If-Modified-Since",
	"If-Unmodified-Since",
	"If-Range",
	"Range",
}

type flightKey struct{}

// flight is what is remembered about a request while it is sent to the backend
type flight struct {
	// hash is the hash under which the request's singleflight is stored,
	// so that it can be found again once the backend responds, even if
	// the cacher would now hash the request differently (e.g. because the
	// Vary header it last saw for the URL has since been evicted).
	hash string
	// conditional holds the request's conditionalHeaders, which are not
	// sent to the backend
	conditional http.Header
	// flight is the Flight which the request is the leader of
	flight *singleflight.Flight
}

type bypassKey struct{}

// withBypass marks r as being sent to the backend without the cache, so
// that its response isn't cached or shared via a singleflight.
func withBypass(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), bypassKey{}, true))
}

func isBypass(r *http.Request) bool {
	bypass, _ := r.Context().Value(bypassKey{}).(bool)
	return bypass
}

// forBackend returns a copy of r to fetch a response for the cache with:
// it has none of the conditionalHeaders, so that the backend sends the
// full response.  Whether this requester gets a 304 instead is decided
// from the cached response (see clientRequest).
func forBackend(r *http.Request, hash string, leading *singleflight.Flight) *http.Request {
	f := flight{hash: hash, conditional: http.Header{}, flight: leading}
	r = r.Clone(context.WithValue(r.Context(), flightKey{}, f))
	for _, name := range conditionalHeaders {
		if values, found := r.Header[name]; found {
			f.conditional[name] = values
			r.Header.Del(name)
		}
	}
	return r
}

// requestHash returns the hash recorded by forBackend, or else hashes r.
func requestHash(c cache.Cacher, r *http.Request) string {
	if f, ok := r.Context().Value(flightKey{}).(flight); ok {
		return f.hash
	}
	return c.Hash(r)
}

// clientRequest returns r as the client sent it, with any of the
// conditionalHeaders that forBackend removed.
func clientRequest(r *http.Request) *http.Request {
	f, ok := r.Context().Value(flightKey{}).(flight)
	if !ok || len(f.conditional) == 0 {
		return r
	}
	r = r.Clone(r.Context())
	for name, values := range f.conditional {
		r.Header[name] = values
	}
	return r
}

// Forwarder returns a new forward.Forwarder which saves responses
// into cache.Cacher c.  It panics if it cannot create the forwarder.
func Forwarder(c cache.Cacher) http.Handler {
	return NewForwarder(c, nil)
}

// NewForwarder is like Forwarder, but sends requests to the backend with
// transport (or http.DefaultTransport, if transport is nil) - e.g. to set
// timeouts.
func NewForwarder(c cache.Cacher, transport http.RoundTripper) http.Handler {
	if transport == nil {
		transport = http.DefaultTransport
	}
	forwarder, err := forward.New(
		forward.ResponseModifier(FlushSingleflight(c, nil)),
		forward.ErrorHandler(backendErrorHandler{c}),
		forward.RoundTripper(transport),
	)
	if err != nil {
		panic(err)
	}
	return forwarder
}

// backendErrorHandler handles requests for which the backend could not
// be reached.  If the cache has a response which stale-if-error allows to
// be served, it serves that; otherwise it responds with an error.  Either
// way, any requests waiting on the same Flight get the same response -
// unless the error was this request's client going away, in which case
// they try again.
type backendErrorHandler struct {
	c cache.Cacher
}

func (h backendErrorHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, err error) {
	if !h.c.CanCache(r) || isBypass(r) {
		utils.DefaultHandler.ServeHTTP(w, r, err)
		return
	}
	hash := requestHash(h.c, r)
	leading, _ := r.Context().Value(flightKey{}).(flight)
	finish := func(result singleflight.Result) {
		if leading.flight != nil {
			flights.Finish(hash, leading.flight, result)
		}
	}
	if prev, found := h.c.Load(hash, r); found && canServeStaleOnError(prev, "") {
		finish(singleflight.Result{Response: prev, Stale: true})
		header, body := cache.Negotiate(prev, clientRequest(r))
		for key, values := range header {
			w.Header()[key] = append([]string(nil), values...)
		}
		w.Header().Set("Age", prev.Age())
		w.Header().Set("Warning", staleWarning())
		w.Header().Set("X-Honey-Cache", "STALE")
		w.Header().Set("X-Honey-Stale", fmt.Sprintf("Backend error: %v", err))
		w.WriteHeader(prev.StatusCode())
		w.Write(body)
		return
	}
	if r.Context().Err() != nil {
		finish(singleflight.Result{Retry: true})
	} else {
		statusCode := http.StatusBadGateway
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			statusCode = http.StatusGatewayTimeout
		}
		finish(singleflight.Result{StatusCode: statusCode})
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
