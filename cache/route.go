package cache

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Route is a rule for requests whose path starts with Prefix or, if Regex
// is set, whose path and query string match Regex.  Its settings override
// the cacher's for those requests.
type Route struct {
	Prefix string
	Regex  *regexp.Regexp
	// NoCache stops matching requests from being cached.  It applies if
	// any matching route sets it, wherever it is in the list.
	NoCache bool
	// DefaultTTL overrides Options.DefaultTTL, if it isn't zero.
	DefaultTTL time.Duration
	// StaleIfError overrides Options.StaleIfError, if it isn't nil.
	StaleIfError *time.Duration
	// QueryParams overrides the query parameters kept on requests (see
	// fetch.Options.QueryParams), if it isn't nil.
	QueryParams *[]string
}

func (route *Route) matches(r *http.Request) bool {
	if route.Regex != nil {
		return route.Regex.MatchString(r.URL.RequestURI())
	}
	return strings.HasPrefix(r.URL.Path, route.Prefix)
}

// AddRoute adds a route.  For settings, the first route which matches a
// request applies.
func (c *defaultCacher) AddRoute(route Route) {
	c.routes = append(c.routes, &route)
}

type routeKey struct{}

// routed records the route chosen for a request (which may be none)
type routed struct {
	route   *Route
	noCache bool
}

func (c *defaultCacher) match(r *http.Request) routed {
	var result routed
	for _, route := range c.routes {
		if !route.matches(r) {
			continue
		}
		if route.NoCache {
			result.noCache = true
		} else if result.route == nil {
			result.route = route
		}
	}
	return result
}

// WithRoute records which route applies to r, so that it still applies
// once the request has been rewritten (e.g. its query parameters
// filtered).  It should be called with the request as the client sent it.
func (c *defaultCacher) WithRoute(r *http.Request) *http.Request {
	if _, done := r.Context().Value(routeKey{}).(routed); done {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), routeKey{}, c.match(r)))
}

func (c *defaultCacher) routing(r *http.Request) routed {
	if r == nil {
		return routed{}
	}
	if result, ok := r.Context().Value(routeKey{}).(routed); ok {
		return result
	}
	return c.match(r)
}

// Route returns the route whose settings apply to r, or nil if none do.
func (c *defaultCacher) Route(r *http.Request) *Route {
	return c.routing(r).route
}
