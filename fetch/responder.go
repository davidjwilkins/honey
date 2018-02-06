package fetch

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/davidjwilkins/honey/cache"
	"github.com/davidjwilkins/honey/singleflight"
	"github.com/davidjwilkins/honey/utilities"
)

var singleflights sync.Map

var staleWhileRevaldateFinder = regexp.MustCompile(`stale-while-revalidate=(?:\")?(\d+)(?:\")?(?:,|$)`)

// ResponseFromCache will see if there a response for request r which exists in cache c.
// If returns the hash of the request, and whether or not the request was responded to.
// It will return false if either Cache-Control or Pragma contains the no-cache directive,
// or if the response is not in the cache.∫b
// If it is found in the cache, it will check to see if the request'sIf-None-Match header
// has the same value as the response's Etag, and if so, will return a  301: Not Modified.
// Otherwise, we will return the cached response, with an "X-Honey-Cache: HIT" header
func RespondFromCache(c cache.Cacher, w http.ResponseWriter, r *http.Request) (hash string, responded bool, revalidate bool) {
	hash = c.Hash(r)
	cc := r.Header.Get("Cache-Control")
	if strings.Contains(cc, "no-cache") ||
		r.Header.Get("Pragma") == "no-cache" {
		return hash, false, false
	}
	resp, found := c.Load(hash, r)
	var statusCode int
	if found && (strings.Contains(cc, "must-revalidate") ||
		strings.Contains(cc, "proxy-revalidate") ||
		strings.Contains(cc, "max-age")) {
		responded, statusCode = resp.Validate(r)
		// https://tools.ietf.org/html/rfc5861#page-2
		// If the response is not valid, but it has a "stale-while-revalidate"
		// and we are within the timeframe specified, serve the stale content,
		// and revalidate in background
		if (!responded) && strings.Contains(cc, "stale-while-revalidate") {
			age, err := strconv.Atoi(resp.Age())
			maxAge, found := utilities.GetMaxAge(cc)
			if !found {
				maxAge = 0
			}
			if err == nil {
				var staleOffset int
				tmp := staleWhileRevaldateFinder.FindStringSubmatch(cc)
				if len(tmp) == 2 {
					staleOffset, err = strconv.Atoi(tmp[1])
					if err == nil && maxAge+staleOffset > age {
						revalidate = true
						responded = true
					}
				}
			}
		}
	} else {
		responded = found
		statusCode = http.StatusNotModified
	}
	if responded {
		for key, values := range resp.Header() {
			for _, value := range values {
				w.Header().Set(key, value)
			}
		}
		w.Header().Set("X-Honey-Cache", "HIT")
		if isNotModified(r, resp) {
			w.WriteHeader(statusCode)
			return
		}
		w.WriteHeader(resp.StatusCode())
		w.Write(resp.Body())
	}
	return
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
		hash := c.Hash(r.Request)
		m, found := singleflights.Load(hash)
		if !found {
			// TODO: handle this as it would be a serious error
			return nil
		}
		multi := m.(singleflight.Singleflight)
		response := c.Standardize(r)
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
		if response.StatusCode() >= 500 && strings.Contains(cc, "stale-if-error") {
			var staleAge string
			prevResponse, found := c.Load(c.Hash(r.Request), r.Request)
			if found {
				tmp := staleIfErrorFinder.FindStringSubmatch(cc)
				if len(tmp) == 2 {
					staleAge = tmp[1]
				}
				// This isn't in the spec, but we're going to support a * as meaning to
				// indefinitely serve from the cache if the backend response is invalid
				serveStale = staleAge == "*"
				if !serveStale && staleAge != "" {
					maxage, exists := utilities.GetMaxAge(cc)
					if exists {
						age, err := strconv.Atoi(prevResponse.Age())
						if err == nil {
							staleMax, err := strconv.Atoi(staleAge)
							if err == nil {
								serveStale = (age - maxage) < staleMax
							}
						}
					}
				}
				if serveStale {
					errorCode := response.StatusCode()
					response = prevResponse
					// http://www.iana.org/assignments/http-warn-codes/http-warn-codes.xhtml
					// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Warning
					r.Header.Set("Warning", fmt.Sprintf(`110 Honey "Response is Stale" "%s"`, time.Now().Format(time.RFC1123)))
					r.Header.Set("X-Honey-Cache", "STALE")
					r.Header.Set("X-Honey-Stale", fmt.Sprintf("Backend gave HTTP Status %d", errorCode))
				}
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
		if isNotModified(r.Request, response) {
			r.StatusCode = http.StatusNotModified
			r.Body = ioutil.NopCloser(bytes.NewReader([]byte{}))
		} else if canRespondWithoutBody(r.Request) {
			if cached, code := response.Validate(r.Request); cached {
				r.StatusCode = code
				r.Body = ioutil.NopCloser(bytes.NewReader([]byte{}))
			}
		}
		return nil
	}
}

// ResponseFromSingleflight will see if there is already a singleflight for the supplied hash.
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

func canRespondWithoutBody(req *http.Request) bool {
	return strings.Contains(req.Header.Get("Cache-Control"), "must-revalidate") ||
		strings.Contains(req.Header.Get("Cache-Control"), "proxy-revalidate") ||
		req.Header.Get("If-Modified-Since") != "" || req.Header.Get("If-UnModified-Since") != ""
}

var staleIfErrorFinder = regexp.MustCompile(`stale-if-error=(?:\")?(\d+|\*+)(?:\")?(?:,|$)`)
