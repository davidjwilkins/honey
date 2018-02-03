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
	Validate(*http.Request) bool
	Age() string
}

type responseImpl struct {
	response *http.Response
	body     []byte
	headers  http.Header
	once     sync.Once
	now      time.Time
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

// Validate returns true if a response is still considered valid
// for request req.
func (r *responseImpl) Validate(req *http.Request) bool {
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
				return int(time.Since(r.now)/time.Second) < delta
			}
		}

		// https://tools.ietf.org/html/rfc7231#section-7.1.1.1
		if r.Header().Get("Expires") == "" || r.Header().Get("Expires") == "0" {
			return false
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
			// TODO: Grab correct server timezone?
			expires = expires.Add(8 * time.Hour)

		}
		//e.g. "Mon, 02 Jan 2006 15:04:05 -0700"
		if err != nil {
			expires, err = time.Parse(time.RFC1123Z, r.Header().Get("Expires"))
		}
		if err != nil {
			return false
		}
		return r.now.Before(expires)
	}

	if req.Header.Get("If-Modifed-Since") != "" || req.Header.Get("If-Unmodifed-Since") != "" {
		var check time.Time
		modified, err := time.Parse(time.RFC1123, r.Header().Get("Last-Modified"))
		if err != nil {
			return false
		}
		if req.Header.Get("If-Modifed-Since") != "" {
			check, err = time.Parse(time.RFC1123, r.Header().Get("If-Modifed-Since"))
			if err != nil {
				return true
			}
			return check.Before(modified)
		}
		check, err = time.Parse(time.RFC1123, r.Header().Get("If-Unmodifed-Since"))
		if err != nil {
			return true
		}
		return check.After(modified)
	}
	return true
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
