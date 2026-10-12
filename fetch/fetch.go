package fetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/davidjwilkins/honey/cache"
	"github.com/davidjwilkins/honey/events"
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
	// BackendHost, if set, is the Host header sent to the backend, instead
	// of the host the client asked for (which is still sent in
	// X-Forwarded-Host, and is still part of the cache key) - e.g. to
	// proxy a site hosted elsewhere, whose server only answers to its own
	// name.
	BackendHost string
	// OnPurge, if set, is called after each purge, e.g. to warm the cache
	// again (see the warm package).
	OnPurge PurgeHook
	// Hosts, if not nil, are the only hosts (see HostAllowed) which
	// requests are handled for: requests for any other get a 421
	// Misdirected Request, rather than being sent to the backend and
	// cached (so that e.g. requests with random Host headers can't fill
	// the cache).  PURGE requests are allowed for any host, as purges
	// apply to every host.
	Hosts []string
	// Events, if set, is where PURGE requests and requests for other
	// hosts are logged, e.g. for the admin page.
	Events *events.Log
}

// FetchWithOptions is like Fetch, configured by opts.
func FetchWithOptions(c cache.Cacher, handler http.Handler, backend *url.URL, opts Options) http.HandlerFunc {
	filterQuery := queryFilter(c, opts.QueryParams)
	serve := serveFromCache(c, handler, filterQuery)
	var self http.HandlerFunc
	purgeFragments := FragmentPurger(c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { self(w, r) }))
	self = func(w http.ResponseWriter, r *http.Request) {
		// Only the cache sends this to the backend (see forBackend)
		r.Header.Del(cache.FragmentTokenHeader)
		if r.Method == MethodPurge && r.Header.Get(FragmentPurgeHeader) != "" {
			handleFragmentPurge(w, r, opts, purgeFragments)
			return
		}
		SwitchBackend(r, backend)
		if opts.Hosts != nil && r.Method != MethodPurge && !HostAllowed(r.Host, opts.Hosts) {
			host := r.Host
			opts.Events.Add(events.Refused, "other hosts", 10*time.Minute, func(e *events.Event) {
				e.Text = fmt.Sprintf("turned away requests for hosts other than %s, most recently %q", strings.Join(opts.Hosts, ", "), host)
			})
			http.Error(w, http.StatusText(http.StatusMisdirectedRequest), http.StatusMisdirectedRequest)
			return
		}
		if opts.BackendHost != "" {
			r.Host = opts.BackendHost
		}
		r = withRoute(c, r)
		if r.Method == MethodPurge {
			// so that it matches the cache keys
			filterQuery(r)
		}
		if opts.Control.applyControl(c, w, r, opts.OnPurge, opts.Events) {
			return
		}
		serve(w, r)
	}
	return self
}

// router is implemented by Cachers with per-route settings
type router interface {
	WithRoute(r *http.Request) *http.Request
	Route(r *http.Request) *cache.Route
}

// withRoute records the route which applies to r, if c has routes, before
// r is rewritten.
func withRoute(c cache.Cacher, r *http.Request) *http.Request {
	if rt, ok := c.(router); ok {
		return rt.WithRoute(r)
	}
	return r
}

// queryFilter returns a function which removes the query parameters not
// kept from a request, and sorts the rest.  The parameters kept are those
// of the request's route, or else keep.  If neither lists any, it does
// nothing.
func queryFilter(c cache.Cacher, keep []string) func(*http.Request) {
	rt, _ := c.(router)
	return func(r *http.Request) {
		params := keep
		if rt != nil {
			if route := rt.Route(r); route != nil && route.QueryParams != nil {
				params = *route.QueryParams
			}
		}
		if params == nil || r.URL.RawQuery == "" {
			return
		}
		kept := make(map[string]bool, len(params))
		for _, name := range params {
			kept[name] = true
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
			removeUnkeyed(c, r)
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
				// the backend request keeps its values (e.g. its route)
				// but not its cancellation.
				go revalidateInBackground(c, hash, handler, r.Clone(context.WithoutCancel(r.Context())))
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
			// A range request for a static file which isn't in the cache is
			// most likely seeking in large media, so let the backend handle
			// it rather than fetching the whole thing.  Range requests for
			// anything else are handled like any other request, so that
			// they can't be used to get around the cache.
			if r.Header.Get("Range") != "" && utilities.IsStaticFile(r.URL.Path) {
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

// overrideHeaders make some backends (e.g. WordPress's REST API, or IIS)
// handle a request as if it had another method or URL.  They aren't part
// of the cache key, so a request with one could have its response cached
// for everyone else's.
var overrideHeaders = []string{
	"X-Http-Method-Override",
	"X-Http-Method",
	"X-Method-Override",
	"X-Original-Url",
	"X-Rewrite-Url",
}

// removeUnkeyed removes what the backend's response might depend on, but
// which isn't part of the cache key, from a cacheable request: the
// overrideHeaders, and any cookies other than the allowed ones (which are
// part of the key).  Otherwise one visitor could have a page cached for
// everyone with e.g. their theme or currency cookie.  (Requests with
// cookies which mark them as personal, such as being logged in, aren't
// cacheable.)
func removeUnkeyed(c cache.Cacher, r *http.Request) {
	for _, name := range overrideHeaders {
		r.Header.Del(name)
	}
	if r.Header.Get("Cookie") == "" {
		return
	}
	allowed := c.AllowedCookies()
	utilities.KeepCookies(r.Header, func(name string) bool {
		for _, a := range allowed {
			if name == a {
				return true
			}
		}
		return false
	})
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
		writeShared(w, r, c, result.Response, result.Stale)
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
func writeShared(w http.ResponseWriter, r *http.Request, c cache.Cacher, resp cache.Response, stale bool) {
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
		setBrowserCacheControl(c, r, w.Header(), resp)
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
// the cache, unless a request for it is already in flight, or one has just
// finished (e.g. r found the stale response just before another request's
// refresh replaced it).
func revalidateInBackground(c cache.Cacher, hash string, handler http.Handler, r *http.Request) {
	f, leader := flights.Join(hash)
	if !leader {
		return
	}
	if resp, found := c.Load(hash, r); found {
		if age, err := strconv.Atoi(resp.Age()); err == nil && age < freshnessLifetime(resp) {
			flights.Finish(hash, f, singleflight.Result{Retry: true})
			return
		}
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
	// The backend marks shared fragments with this, so that the cache can
	// tell them from markers in the page's content (see cache/fragments.go)
	r.Header.Set(cache.FragmentTokenHeader, cache.NewFragmentToken(time.Now()))
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
		// Send the backend the request's Host (see SwitchBackend)
		forward.PassHostHeader(true),
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

// SwitchBackend changes the host and scheme of a request's URL to the
// backend's, which it is forwarded to.  The request keeps its Host header
// (the host the client asked for), which is passed on to the backend, as
// sites such as WordPress redirect requests for any other host.  The host
// is also put in the X-Forwarded-Host header (replacing any the client
// sent), and the protocol the client used (http or https) in
// X-Forwarded-Proto, unless it is already there (e.g. from a proxy in
// front of Honey which handles TLS).  The cache keys responses by both
// (see cache.Cacher.Hash), as the backend's responses may depend on them.
func SwitchBackend(req *http.Request, backend *url.URL) {
	host := req.Host
	if host == "" {
		// e.g. requests made by a client, rather than received by a server
		host = req.URL.Host
	}
	host = strings.ToLower(host)
	req.Host = host
	req.Header.Set("X-Forwarded-Host", host)
	switch strings.ToLower(req.Header.Get("X-Forwarded-Proto")) {
	case "http", "https":
	default:
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
	req.URL.Host = backend.Host
	req.URL.Scheme = backend.Scheme
}
