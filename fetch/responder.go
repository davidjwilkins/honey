package fetch

import (
	"bytes"
	"io/ioutil"
	"net/http"
	"strings"
	"sync"

	"github.com/davidjwilkins/honey/cache"
	"github.com/davidjwilkins/honey/multiplexer"
)

var multiplexers sync.Map

// ResponseFromCache will see if there a response for request r which exists in cache c.
// If returns the hash of the request, and whether or not the request was responded to.
// It will return false if either Cache-Control or Pragma contains the no-cache directive,
// or if the response is not in the cache.
// If it is found in the cache, it will check to see if the request's If-None-Match header
// has the same value as the response's Etag, and if so, will return a 301: Not Modified.
// Otherwise, we will return the cached response, with an "X-Honey-Cache: HIT" header
func RespondFromCache(c cache.Cacher, w http.ResponseWriter, r *http.Request) (hash string, responded bool) {
	hash = c.Hash(r)
	if strings.Contains(r.Header.Get("Cache-Control"), "no-cache") ||
		r.Header.Get("Pragma") == "no-cache" {
		return hash, false
	}
	resp, found := c.Load(hash)
	if strings.Contains(r.Header.Get("Cache-Control"), "must-revalidate") ||
		strings.Contains(r.Header.Get("Cache-Control"), "proxy-revalidate") {
		responded = resp.Validate(r)
	} else {
		responded = found
	}
	if responded {
		if isNotModified(r, resp) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(resp.StatusCode())
		for key, values := range resp.Header() {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.Header().Set("X-Honey-Cache", "HIT")
		w.Write(resp.Body())
	}
	return
}

// FlushMultiplexer is a forward.ResponseModifier - it returns a function
// which takes a pointer a Response, and modified it.  In our case, we don't
// actually modify the response, we instead save a standardized version of it
// in Cacher c (unless it contains the Cache-Control: no-store directive).
// It then writes the response to the multiplexer, and deletes the key from the
// multiplexer list (because responses can now be handled from the cache).
func FlushMultiplexer(c cache.Cacher, done chan bool) func(*http.Response) error {
	return func(r *http.Response) error {
		if r.Request == nil {
			return nil
		}
		hash := c.Hash(r.Request)
		m, found := multiplexers.Load(hash)
		if !found {
			// TODO: handle this as it would be a serious error
			return nil
		}
		multi := m.(multiplexer.Multiplexer)
		response := c.Standardize(r)
		if !strings.Contains(response.Header().Get("Cache-Control"), "no-store") {
			c.Cache(hash, response)
		}
		r.Header.Set("X-Honey-Cache", "MISS")
		go func() {
			multi.Write(response)
			multiplexers.Delete(hash)
			if done != nil {
				done <- true
			}
		}()
		if isNotModified(r.Request, response) {
			r.StatusCode = http.StatusNotModified
			r.Body = ioutil.NopCloser(bytes.NewReader([]byte{}))
		}
		return nil
	}
}

// ResponseFromMultiplexer will see if there is already a multiplexer for the supplied hash.
// If so, it will add ResponseWriter w to the multiplexer, wait for the multiplexer to response,
// and then return true.  Otherwise, it will create a new multiplexer for the hash, and return
// false.
func RespondFromMultiplexer(hash string, c cache.Cacher, w http.ResponseWriter, r *http.Request) (responded bool) {
	multi := multiplexer.NewMultiplexer(c, r)
	m, fetching := multiplexers.LoadOrStore(hash, multi)
	if fetching {
		multi = m.(multiplexer.Multiplexer)
		multi.AddWriter(w, r)
		if onwait != nil {
			(*onwait)()
		}
		multi.Wait()
		multiplexers.Delete(hash)
		return true
	}
	return false
}

func isNotModified(r *http.Request, resp cache.Response) bool {
	return r.Header.Get("If-None-Match") != "" &&
		r.Header.Get("If-None-Match") == resp.Header().Get("Etag")
}

// for testing - lets us flush multiplexer once it is waiting
var onwait *func()
