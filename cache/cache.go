package cache

import (
	"net/http"
)

// A Cacher interface is used by the reverse proxy to
// determine whether a given request may be cached, and
// if so, to save the response to the cache under the
// appropriate hash. It must also be able to fetch, save
// and load the response to a given request
type Cacher interface {
	// CanCache returns whether an http.Request is
	// eligible for cacheing.
	CanCache(*http.Request) bool
	// Hash returns the key under which an http.Request
	// will be saved in the cache.  Any request which
	// hashes to the same value will be returned the
	// same response.
	Hash(*http.Request) string
	// Standardize takes an http.Response, and modifies
	// it to a response that can be returned to any client.
	// E.g. It removes unncessary headers, etc.
	Standardize(*http.Response) Response
	// Cache saves a response to the cache under the
	// supplied hash
	Cache(hash string, response Response)
	// Load retrieves a response from the cache for the
	// supplied hash
	Load(hash string, r *http.Request) (Response, bool)
	// AllowedCookies retrieves a list of cookies the cache will
	// allow in the response
	AllowedCookies() []string
}
