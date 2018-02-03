package cache

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var tz = time.FixedZone("America/Los_Angeles", -8)

// A Response interface is saved in the cache, and is used
// to populate a real (net/http) Response when a request
// may be served via the cache.
// It must know it's own status, status code, headers,
// and response body.
type Response interface {
	Status() string
	StatusCode() int
	Header() http.Header
	Body() []byte
	Validate(*http.Request) (bool, int)
	Age() string
	Cookie(name string) (*http.Cookie, error)
	RequestHeaders() http.Header
}

type responseImpl struct {
	response       *http.Response
	cookies        map[string]*http.Cookie
	body           []byte
	headers        http.Header
	requestHeaders http.Header
	once           sync.Once
	now            time.Time
}

func (r *responseImpl) RequestHeaders() http.Header {
	return r.requestHeaders
}

// Cookie returns the cookie with the given name from the response
func (r *responseImpl) Cookie(name string) (*http.Cookie, error) {
	if c, ok := r.cookies[name]; ok {
		return c, nil
	}
	return nil, http.ErrNoCookie
}

// Status returns the Status of the response
func (r *responseImpl) Status() string {
	return r.response.Status
}

// StatusCode returns the http Status Code of the response
func (r *responseImpl) StatusCode() int {
	return r.response.StatusCode
}

// Header returns a map[string][]string containing the
// headers to be set in the response
func (r *responseImpl) Header() http.Header {
	return r.headers
}

func (r *responseImpl) Age() string {
	return strconv.FormatUint(uint64(time.Since(r.now)/time.Second), 10)
}

var smaxAgeFinder = regexp.MustCompile(`s-maxage=(?:\")?(\d+)(?:\")?(?:,|$)`)
var maxAgeFinder = regexp.MustCompile(`max-age=(?:\")?(\d+)(?:\")?(?:,|$)`)

// Validate returns true and a status code if a response is still considered valid
// for request req. Otherwise it will return false and the status code should be
// ignored.
func (r *responseImpl) Validate(req *http.Request) (bool, int) {
	if strings.Contains(req.Header.Get("Cache-Control"), "must-revalidate") ||
		strings.Contains(req.Header.Get("Cache-Control"), "proxy-revalidate") {
		cc := r.Header().Get("Cache-Control")
		var age string
		if strings.Contains(cc, "s-maxage") {
			// https://tools.ietf.org/html/rfc7234#section-5.2.2.8
			tmp := smaxAgeFinder.FindStringSubmatch(cc)
			if len(tmp) == 2 {
				age = tmp[1]
			}
		} else if strings.Contains(cc, "max-age") {
			// https://tools.ietf.org/html/rfc7234#section-5.2.2.9
			tmp := maxAgeFinder.FindStringSubmatch(cc)
			if len(tmp) == 2 {
				age = tmp[1]
			}
		}
		if age != "" {
			delta, err := strconv.Atoi(age)
			if err == nil {
				return int(time.Since(r.now)/time.Second) < delta, http.StatusNotModified
			}
		}

		// https://tools.ietf.org/html/rfc7231#section-7.1.1.1
		if r.Header().Get("Expires") == "" || r.Header().Get("Expires") == "0" {
			return false, 0
		}
		//e.g. "Sun, 06 Nov 1994 08:49:37 GMT"
		expires, err := time.Parse(time.RFC1123, r.Header().Get("Expires"))
		if err != nil {
			//e.g. "Monday, 02-Jan-06 15:04:05 MST"
			expires, err = time.Parse(time.RFC850, r.Header().Get("Expires"))
		}
		//e.g. "Mon Jan _2 15:04:05 2006"
		if err != nil {
			expires, err = time.Parse(time.ANSIC, r.Header().Get("Expires"))
			_, offset := r.now.Zone()
			// Make the timezone match - not sure if this is a good idea
			// but it seems to make sense
			expires = expires.Add(time.Duration(offset * int(time.Second) * -1))
		}
		//e.g. "Mon, 02 Jan 2006 15:04:05 -0700"
		if err != nil {
			expires, err = time.Parse(time.RFC1123Z, r.Header().Get("Expires"))
		}
		if err != nil {
			return false, 0
		}
		return r.now.Before(expires), http.StatusNotModified
	}
	if req.Header.Get("If-Modified-Since") != "" || req.Header.Get("If-UnModified-Since") != "" {
		modified, err := time.Parse(time.RFC1123, r.Header().Get("Last-Modified"))
		if err != nil {
			return false, 0
		}
		// The If-Modified-Since request HTTP header makes the request conditional: the server will
		// send back the requested resource, with a 200 status, only if it has been last modified
		// after the given date.
		if req.Header.Get("If-Modified-Since") != "" {
			ifModifiedSince, err := time.Parse(time.RFC1123, req.Header.Get("If-Modified-Since"))
			if err != nil {
				return false, 0
			}
			return modified.Before(ifModifiedSince), http.StatusNotModified
		}
		// The If-Unmodified-Since request HTTP header makes the request conditional: the server will
		// send back the requested resource, or accept it in the case of a POST or another non-safe
		// method, only if it has not been last modified after the given date. If the request has been
		// modified after the given date, the response will be a 412 (Precondition Failed) error.
		ifUnmodifiedSince, err := time.Parse(time.RFC1123, req.Header.Get("If-Unmodified-Since"))
		if err != nil {
			return false, 0
		}
		valid := ifUnmodifiedSince.After(modified)

		// For If-Unmodified-Since, we need to treat it an invalid resource as if it were cached,
		// in that we shouldn't fetch the resource.  But valid resources we still do fetch.
		// So although this seems backwards, it is correct.
		if !valid {
			return true, http.StatusPreconditionFailed
		}
		return false, http.StatusOK
	}
	return true, http.StatusNotModified
}

// Body returns the content of the response
func (r *responseImpl) Body() []byte {
	return r.body
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}
