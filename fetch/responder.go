package fetch

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/davidjwilkins/honey/cache"
	"github.com/davidjwilkins/honey/singleflight"
	"github.com/davidjwilkins/honey/utilities"
)

// RespondFromCache will see if there a response for request r which exists in cache c.
// It returns the hash of the request, whether or not the request was responded to, and
// whether the response was stale and so needs to be revalidated.
// It will return false if either Cache-Control or Pragma contains the no-cache directive,
// if the response is not in the cache, or if the cached response is no longer fresh
// (and can't be served stale per stale-while-revalidate).
// If it is found in the cache, it will check to see if the request's If-None-Match header
// has the same value as the response's Etag, and if so, will return a 304: Not Modified.
// Otherwise, we will return the cached response, with an "X-Honey-Cache: HIT" header
// (or "X-Honey-Cache: STALE" if it is being revalidated).
func RespondFromCache(c cache.Cacher, w http.ResponseWriter, r *http.Request) (hash string, responded bool, revalidate bool) {
	hash = c.Hash(r)
	cc := r.Header.Get("Cache-Control")
	if strings.Contains(cc, "no-cache") ||
		r.Header.Get("Pragma") == "no-cache" {
		return hash, false, false
	}
	resp, found := c.Load(hash, r)
	if !found {
		return hash, false, false
	}
	age, _ := strconv.Atoi(resp.Age())
	lifetime := freshnessLifetime(resp)
	statusCode := http.StatusNotModified
	if age >= lifetime {
		// https://tools.ietf.org/html/rfc5861#section-3
		// If the response is stale, but it has a "stale-while-revalidate"
		// and we are within the timeframe specified, serve the stale content,
		// and revalidate in background
		if age >= lifetime+utilities.StaleWhileRevalidate(resp.Header()) {
			return hash, false, false
		}
		responded, revalidate = true, true
	} else if strings.Contains(cc, "must-revalidate") ||
		strings.Contains(cc, "proxy-revalidate") ||
		strings.Contains(cc, "max-age") {
		responded, statusCode = resp.Validate(r)
	} else {
		responded = true
	}
	if responded {
		if h, ok := resp.(hitCounter); ok {
			h.CountHit()
		}
		header, body := cache.Negotiate(resp, r)
		noteCached(w, resp)
		copyHeader(w.Header(), header)
		w.Header().Set("Age", strconv.Itoa(age))
		if revalidate {
			w.Header().Set("X-Honey-Cache", "STALE")
		} else {
			w.Header().Set("X-Honey-Cache", "HIT")
			setBrowserCacheControl(c, r, w.Header(), resp)
		}
		if isNotModified(r, header.Get("Etag")) {
			w.WriteHeader(statusCode)
			return
		}
		if r.Header.Get("Range") != "" && resp.StatusCode() == http.StatusOK {
			// Handles Range and If-Range, using the Etag set above.  The
			// body is unencoded, as Negotiate doesn't encode range requests.
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
			return
		}
		w.WriteHeader(resp.StatusCode())
		w.Write(body)
	}
	return
}

// copyHeader copies a cached response's header to dst, without copying
// its values: each list is capped, so that adding to it (e.g. Header.Add)
// makes a new one rather than writing into the cache's.  (Nothing changes
// a header's values in place.)
func copyHeader(dst, cached http.Header) {
	for key, values := range cached {
		dst[key] = values[:len(values):len(values)]
	}
}

// canServeStaleOnError returns whether the cached response prev may be served
// because the backend errored, per its stale-if-error directive (or that of
// the backend's error response, errorCacheControl).
// https://tools.ietf.org/html/rfc5861#section-4
func canServeStaleOnError(prev cache.Response, errorCacheControl string) bool {
	cc := prev.Header().Get("Cache-Control")
	if _, found := utilities.Directive(cc, "stale-if-error"); !found {
		cc = errorCacheControl
	}
	staleIfError, forever := utilities.StaleIfError(cc)
	// This isn't in the spec, but we're going to support a * as meaning to
	// indefinitely serve from the cache if the backend response is invalid
	if forever {
		return true
	}
	if staleIfError == 0 {
		return false
	}
	age, err := strconv.Atoi(prev.Age())
	if err != nil {
		return false
	}
	return age-freshnessLifetime(prev) < staleIfError
}

// hitCounter is implemented by Responses which count how often they're
// served from the cache
type hitCounter interface {
	CountHit()
}

// expirer is implemented by Responses which can be soft purged
type expirer interface {
	Expired() bool
}

// freshnessLifetime returns how many seconds resp is fresh for, from when
// it was fetched: 0 if it has been soft purged.
func freshnessLifetime(resp cache.Response) int {
	if e, ok := resp.(expirer); ok && e.Expired() {
		return 0
	}
	return utilities.FreshnessLifetime(resp.Header())
}

// staleWarning is the Warning header value set on stale responses
// http://www.iana.org/assignments/http-warn-codes/http-warn-codes.xhtml
func staleWarning() string {
	return fmt.Sprintf(`110 Honey "Response is Stale" "%s"`, time.Now().UTC().Format(http.TimeFormat))
}

// FlushSingleflight is a forward.ResponseModifier - it returns a function
// which takes a pointer a Response, and modified it.  In our case, we don't
// actually modify the response, we instead save a standardized version of it
// in Cacher c (unless it contains the Cache-Control: no-store directive).
// It then writes the response to the singleflight, and deletes the key from the
// singleflight list (because responses can now be handled from the cache).
func FlushSingleflight(c cache.Cacher, done chan bool) func(*http.Response) error {
	// Any modifications made to the response headers should be made to r.Header
	// and not to response.Header as the headers from r will be copied to response,
	// but if they don't get set on r then they won't appear on the initial request
	return func(r *http.Response) error {
		// Only the cache marks responses as using ESI (see cache.Standardize),
		// and Surrogate-Control is for it, not clients
		standardized := false
		defer func() {
			if !standardized {
				r.Header.Del(cache.ESIHeader)
				r.Header.Del("Surrogate-Control")
			}
		}()
		if r.Request == nil {
			return nil
		}
		if isBypass(r.Request) {
			r.Header.Set("X-Honey-Cache", "NO-CACHE")
			return nil
		}
		leading, ok := r.Request.Context().Value(flightKey{}).(flight)
		if !ok || leading.flight == nil {
			return nil
		}
		hash := leading.hash
		finish := func(result singleflight.Result) {
			flights.Finish(hash, leading.flight, result)
			if done != nil {
				go func() { done <- true }()
			}
		}
		if s, ok := c.(sessionStarter); ok && s.StartsSession(r) {
			// It's personal: pass it (and its cookie) on to this requester
			// only, and have everyone waiting for it fetch their own
			finish(singleflight.Result{Bypass: true})
			r.Header.Set("X-Honey-Cache", "NO-CACHE")
			return nil
		}
		if !storableStatus(r.StatusCode, r.Header) && r.StatusCode < 500 {
			// e.g. a 400 for a request with an oversized header, or a 403
			// from a firewall: it's about this request, not the resource,
			// so it is neither cached nor given to anyone else (and a stale
			// response being refreshed is kept).  Server errors are still
			// shared, so that a struggling backend isn't sent everyone's
			// requests at once.
			finish(singleflight.Result{Bypass: true})
			r.Header.Set("X-Honey-Cache", "NO-CACHE")
			return nil
		}
		maxBytes := int64(1 << 62)
		if limiter, ok := c.(objectSizeLimiter); ok {
			maxBytes = limiter.MaxObjectSize()
		}
		fits, readErr := readBody(r, maxBytes)
		if readErr != nil {
			// The backend failed partway through (e.g. it stalled), so there
			// is no response to cache or share; serve a stale one if allowed
			r.Body.Close()
			if prev, found := c.Load(hash, r.Request); found && canServeStaleOnError(prev, "") {
				replaceWithStale(r, prev, fmt.Sprintf("Backend response failed: %v", readErr))
				finish(singleflight.Result{Response: prev, Stale: true})
				return nil
			}
			statusCode := http.StatusBadGateway
			var netErr net.Error
			if errors.As(readErr, &netErr) && netErr.Timeout() {
				statusCode = http.StatusGatewayTimeout
			}
			replaceWithError(r, statusCode)
			finish(singleflight.Result{StatusCode: statusCode})
			return nil
		}
		if !fits {
			// Stream it to this requester, and have everyone waiting for
			// it fetch it themselves
			finish(singleflight.Result{Bypass: true})
			r.Header.Set("X-Honey-Cache", "NO-CACHE")
			return nil
		}
		response := c.Standardize(r)
		standardized = true
		cc := response.Header().Get("Cache-Control")
		// no-store: https://www.w3.org/Protocols/rfc2616/rfc2616-sec14.html#sec14.9.2
		// and don't cache server errors
		if !strings.Contains(cc, "no-store") && response.StatusCode() < 500 {
			c.Cache(hash, response)
		}
		// if there was a server error, let's try and fetch a good response from the
		// cache and set a warning header to indicate that we have served stale content,
		// if there is a stale-if-error cache control
		// https://tools.ietf.org/html/rfc5861#page-3
		var serveStale bool
		if response.StatusCode() >= 500 {
			prevResponse, found := c.Load(hash, r.Request)
			if found && canServeStaleOnError(prevResponse, cc) {
				serveStale = true
				errorCode := response.StatusCode()
				response = prevResponse
				// Replace the error with the stale response for this requester too
				replaceWithStale(r, prevResponse, fmt.Sprintf("Backend gave HTTP Status %d", errorCode))
			}
		}
		if !serveStale {
			r.Header.Set("X-Honey-Cache", "MISS")
			if shareable(response) {
				setBrowserCacheControl(c, r.Request, r.Header, response)
			}
		}
		finish(singleflight.Result{Response: response, Stale: serveStale})
		// The request was sent to the backend without its conditional
		// headers and Accept-Encoding; decide from the full response which
		// encoding this client gets, and whether it should get a 304 (or
		// 412) instead.
		client := clientRequest(r.Request)
		header, body := cache.Negotiate(response, client)
		if encoding := header.Get("Content-Encoding"); encoding != r.Header.Get("Content-Encoding") {
			for _, key := range []string{"Content-Encoding", "Content-Length", "Etag"} {
				r.Header.Set(key, header.Get(key))
			}
			r.Body = ioutil.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
		if isNotModified(client, header.Get("Etag")) {
			r.StatusCode = http.StatusNotModified
			r.Body = ioutil.NopCloser(bytes.NewReader([]byte{}))
		} else if canRespondWithoutBody(client) {
			if cached, code := response.Validate(client); cached {
				r.StatusCode = code
				r.Body = ioutil.NopCloser(bytes.NewReader([]byte{}))
			}
		}
		return nil
	}
}

// heuristicallyCacheable are the status codes which may be cached without
// the response saying how long for
// https://www.rfc-editor.org/rfc/rfc9111#section-4.2.2 (less 206, which
// is only part of the response, and 501, as server errors aren't cached)
var heuristicallyCacheable = map[int]bool{
	http.StatusOK:                   true,
	http.StatusNonAuthoritativeInfo: true,
	http.StatusNoContent:            true,
	http.StatusMultipleChoices:      true,
	http.StatusMovedPermanently:     true,
	http.StatusPermanentRedirect:    true,
	http.StatusNotFound:             true,
	http.StatusMethodNotAllowed:     true,
	http.StatusGone:                 true,
	http.StatusRequestURITooLong:    true,
}

// storableStatus returns whether a response with statusCode and the
// backend's header may be cached: if it is heuristically cacheable, or if
// it is a temporary redirect which says how long it is fresh for.  Other
// client errors (400, 401, 403, 429...) depend on the request, not just on
// the resource, so they never are - otherwise one bad request (e.g. with
// a header too large for the backend) could break a page for everyone.
func storableStatus(statusCode int, header http.Header) bool {
	if heuristicallyCacheable[statusCode] {
		return true
	}
	switch statusCode {
	case http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect:
		cc := header.Get("Cache-Control")
		_, maxAge := utilities.Directive(cc, "max-age")
		_, sMaxAge := utilities.Directive(cc, "s-maxage")
		return maxAge || sMaxAge || header.Get("Expires") != ""
	}
	return false
}

func isNotModified(r *http.Request, etag string) bool {
	return r.Header.Get("If-None-Match") != "" &&
		r.Header.Get("If-None-Match") == etag
}

// sessionStarter is implemented by Cachers which know which cookies mark
// a visitor whose pages may be personal
type sessionStarter interface {
	StartsSession(*http.Response) bool
}

// objectSizeLimiter is implemented by Cachers which limit the size of the
// responses they cache
type objectSizeLimiter interface {
	MaxObjectSize() int64
}

// readBody reads up to maxBytes+1 bytes of r's body, to find out whether it
// fits in the cache, and replaces r.Body so that the whole body can still
// be read.  It returns an error if the body couldn't be read (e.g. the
// backend stalled), in which case it can't be cached or used at all.
func readBody(r *http.Response, maxBytes int64) (fits bool, err error) {
	if r.ContentLength > maxBytes {
		return false, nil
	}
	body := r.Body
	buffered, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	rest := io.Reader(body)
	if err != nil {
		rest = errorReader{err}
	}
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(buffered), rest), body}
	return err == nil && int64(len(buffered)) <= maxBytes, err
}

// replaceWithStale replaces the response r with the cached response prev,
// marked as stale for the given reason.
func replaceWithStale(r *http.Response, prev cache.Response, reason string) {
	for key := range r.Header {
		r.Header.Del(key)
	}
	for key, values := range prev.Header() {
		r.Header[key] = append([]string(nil), values...)
	}
	r.StatusCode = prev.StatusCode()
	r.Status = prev.Status()
	r.Body = ioutil.NopCloser(bytes.NewReader(prev.Body()))
	r.ContentLength = int64(len(prev.Body()))
	r.Header.Set("Age", prev.Age())
	r.Header.Set("Warning", staleWarning())
	r.Header.Set("X-Honey-Cache", "STALE")
	r.Header.Set("X-Honey-Stale", reason)
}

// replaceWithError replaces the response r with an error response.
func replaceWithError(r *http.Response, statusCode int) {
	for key := range r.Header {
		r.Header.Del(key)
	}
	body := http.StatusText(statusCode) + "\n"
	r.Header.Set("Content-Type", "text/plain; charset=utf-8")
	r.Header.Set("Content-Length", fmt.Sprint(len(body)))
	r.StatusCode = statusCode
	r.Status = fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode))
	r.Body = ioutil.NopCloser(strings.NewReader(body))
	r.ContentLength = int64(len(body))
}

type errorReader struct {
	err error
}

func (e errorReader) Read([]byte) (int, error) {
	return 0, e.err
}

func canRespondWithoutBody(req *http.Request) bool {
	return strings.Contains(req.Header.Get("Cache-Control"), "must-revalidate") ||
		strings.Contains(req.Header.Get("Cache-Control"), "proxy-revalidate") ||
		req.Header.Get("If-Modified-Since") != "" || req.Header.Get("If-UnModified-Since") != ""
}
