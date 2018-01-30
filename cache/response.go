package cache

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

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
	r.headers.Set("Age", strconv.FormatUint(uint64(time.Since(r.now)/time.Second), 10))
	return r.headers
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
