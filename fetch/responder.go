package fetch

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/davidjwilkins/honey/cache"
	"github.com/davidjwilkins/honey/singleflight"
	"github.com/davidjwilkins/honey/utilities"
)

var singleflights sync.Map

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
	lifetime := utilities.FreshnessLifetime(resp.Header())
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
		for key, values := range resp.Header() {
			for _, value := range values {
				w.Header().Set(key, value)
			}
		}
		w.Header().Set("Age", strconv.Itoa(age))
		if revalidate {
			w.Header().Set("X-Honey-Cache", "STALE")
		} else {
			w.Header().Set("X-Honey-Cache", "HIT")
		}
		if isNotModified(r, resp) {
			w.WriteHeader(statusCode)
			return
		}
		if r.Header.Get("Range") != "" && resp.StatusCode() == http.StatusOK {
			// Handles Range and If-Range, using the Etag set above
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(resp.Body()))
			return
		}
		w.WriteHeader(resp.StatusCode())
		w.Write(resp.Body())
	}
	return
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
	return age-utilities.FreshnessLifetime(prev.Header()) < staleIfError
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
		if r.Request == nil {
			return nil
		}
		if isBypass(r.Request) {
			r.Header.Set("X-Honey-Cache", "NO-CACHE")
			return nil
		}
		hash := requestHash(c, r.Request)
		m, found := singleflights.Load(hash)
		if !found {
			// TODO: handle this as it would be a serious error
			return nil
		}
		multi := m.(singleflight.Singleflight)
		if limiter, ok := c.(objectSizeLimiter); ok && !fitsInCache(r, limiter.MaxObjectSize()) {
			// Stream it to this requester, and have everyone waiting for
			// it fetch it themselves
			singleflights.Delete(hash)
			if f, ok := r.Request.Context().Value(flightKey{}).(flight); ok && f.backend != nil {
				multi.Bypass(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					f.backend.ServeHTTP(w, withBypass(req))
				}))
			} else {
				multi.Abort(http.StatusBadGateway)
			}
			r.Header.Set("X-Honey-Cache", "NO-CACHE")
			return nil
		}
		response := c.Standardize(r)
		cc := response.Header().Get("Cache-Control")
		// no-store: https://www.w3.org/Protocols/rfc2616/rfc2616-sec14.html#sec14.9.2
		// and don't cache server errors, or responses only meant for one
		// requester's conditional or range request
		if !strings.Contains(cc, "no-store") && response.StatusCode() < 500 && !isPartialStatus(response.StatusCode()) {
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
				for key := range r.Header {
					r.Header.Del(key)
				}
				for key, values := range prevResponse.Header() {
					r.Header[key] = append([]string(nil), values...)
				}
				r.StatusCode = prevResponse.StatusCode()
				r.Status = prevResponse.Status()
				r.Body = ioutil.NopCloser(bytes.NewReader(prevResponse.Body()))
				r.ContentLength = int64(len(prevResponse.Body()))
				r.Header.Set("Age", prevResponse.Age())
				r.Header.Set("Warning", staleWarning())
				r.Header.Set("X-Honey-Cache", "STALE")
				r.Header.Set("X-Honey-Stale", fmt.Sprintf("Backend gave HTTP Status %d", errorCode))
			}
		}
		if !serveStale {
			r.Header.Set("X-Honey-Cache", "MISS")
		}
		go func() {
			multi.Write(response)
			singleflights.Delete(hash)
			if done != nil {
				done <- true
			}
		}()
		// The request was sent to the backend without its conditional
		// headers; decide from the full response whether this client
		// should get a 304 (or 412) instead.
		client := clientRequest(r.Request)
		if isNotModified(client, response) {
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

// RespondFromSingleflight will see if there is already a singleflight for the supplied hash.
// If so, it will add ResponseWriter w to the singleflight, wait for the singleflight to response,
// and then return true.  Otherwise, it will create a new singleflight for the hash, and return
// false.
func RespondFromSingleflight(hash string, c cache.Cacher, w http.ResponseWriter, r *http.Request, handler func(w http.ResponseWriter, r *http.Request)) (responded bool) {
	multi := singleflight.NewSingleflight(c, r, handler)
	m, fetching := singleflights.LoadOrStore(hash, multi)
	if fetching {
		multi = m.(singleflight.Singleflight)
		multi.AddWriter(w, r)
		multi.Wait()
		singleflights.Delete(hash)
		return true
	}
	return false
}

func isNotModified(r *http.Request, resp cache.Response) bool {
	return r.Header.Get("If-None-Match") != "" &&
		r.Header.Get("If-None-Match") == resp.Header().Get("Etag")
}

// objectSizeLimiter is implemented by Cachers which limit the size of the
// responses they cache
type objectSizeLimiter interface {
	MaxObjectSize() int64
}

// fitsInCache returns whether r's body is no larger than maxBytes.  It
// reads at most maxBytes+1 bytes of the body to find out, and replaces
// r.Body so that the whole body can still be read.  A body which can't
// be read doesn't fit, so that a truncated body isn't cached.
func fitsInCache(r *http.Response, maxBytes int64) bool {
	if r.ContentLength > maxBytes {
		return false
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
	return err == nil && int64(len(buffered)) <= maxBytes
}

type errorReader struct {
	err error
}

func (e errorReader) Read([]byte) (int, error) {
	return 0, e.err
}

// isPartialStatus returns whether statusCode is a response to a conditional
// or range request, which doesn't contain the full response.
func isPartialStatus(statusCode int) bool {
	return statusCode == http.StatusPartialContent ||
		statusCode == http.StatusNotModified ||
		statusCode == http.StatusPreconditionFailed
}

func canRespondWithoutBody(req *http.Request) bool {
	return strings.Contains(req.Header.Get("Cache-Control"), "must-revalidate") ||
		strings.Contains(req.Header.Get("Cache-Control"), "proxy-revalidate") ||
		req.Header.Get("If-Modified-Since") != "" || req.Header.Get("If-UnModified-Since") != ""
}
