package cache

import (
	"net/http"
	"time"
)

// Explanation says how the cache would handle a request, for people
// working out why something is (or isn't) cached.
type Explanation struct {
	// Uncacheable is why the request can't be cached, or "" if it can
	Uncacheable string
	// Route is the route whose settings apply, if any, and RouteNumber its
	// place in the list (from 1)
	Route       *Route
	RouteNumber int
	// NoCacheRoute is the number of a route which stops the request being
	// cached, or 0 if none does
	NoCacheRoute int
	// The settings which apply to the request's responses
	DefaultTTL           time.Duration
	StaleWhileRevalidate time.Duration
	StaleIfError         time.Duration
	BrowserCacheControl  string
}

// Explain says how the cache would handle r.  It neither changes r nor
// the cache.
func (c *defaultCacher) Explain(r *http.Request) Explanation {
	e := Explanation{
		Uncacheable:          c.uncacheable(r),
		DefaultTTL:           c.defaultTTL,
		StaleWhileRevalidate: c.staleWhileReval,
		StaleIfError:         c.staleIfError,
		BrowserCacheControl:  c.BrowserCacheControl(r),
	}
	routed := c.routing(r)
	for i, route := range c.routes {
		if route == routed.route {
			e.Route, e.RouteNumber = route, i+1
		}
		if route.NoCache && e.NoCacheRoute == 0 && route.matches(r) {
			e.NoCacheRoute = i + 1
		}
	}
	if route := routed.route; route != nil {
		if route.DefaultTTL > 0 {
			e.DefaultTTL = route.DefaultTTL
		}
		if route.StaleWhileRevalidate != nil {
			e.StaleWhileRevalidate = *route.StaleWhileRevalidate
		}
		if route.StaleIfError != nil {
			e.StaleIfError = *route.StaleIfError
		}
	}
	return e
}

// Describe returns what the route matches, e.g. "/wp-content/uploads/"
func (route *Route) Describe() string {
	if route.Regex != nil {
		return route.Regex.String()
	}
	return route.Prefix
}

// Peek returns the cached response stored under hash, if there is one,
// like Load, but without counting it as served.
func (c *defaultCacher) Peek(hash string) (Response, bool) {
	r, ok := c.store.get(hash, time.Now())
	if !ok {
		return nil, false
	}
	resp, ok := r.(Response)
	return resp, ok
}

// Routes returns the routes, in order.
func (c *defaultCacher) Routes() []*Route {
	return c.routes
}
