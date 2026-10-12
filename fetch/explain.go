package fetch

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/davidjwilkins/honey/cache"
	"github.com/davidjwilkins/honey/utilities"
)

// Step is one step in how a request is handled (see Explanation)
type Step struct {
	// Outcome is StepOK if the step lets the request through unchanged,
	// StepChanged if it changes it, and StepStopped if it stops it being
	// cached (or served)
	Outcome string
	Title   string
	Detail  string
}

// Step outcomes
const (
	StepOK      = "ok"
	StepChanged = "changed"
	StepStopped = "stopped"
)

// Explanation says how a request would be handled, step by step, without
// sending it anywhere: for people working out why a page is (or isn't)
// cached.
type Explanation struct {
	// Cacheable is whether the request may be served from the cache
	Cacheable bool
	// Verdict sums it up, e.g. "Cacheable, in the cache now"
	Verdict string
	Steps   []Step
	// Key is the request's cache key, if it is cacheable
	Key string
	// Cached is the response cached for it, if there is one
	Cached *CachedResponse
	// The settings which apply to its responses
	DefaultTTL           time.Duration
	StaleWhileRevalidate time.Duration
	StaleIfError         time.Duration
	// BrowserCacheControl is the Cache-Control browsers get, or "" for
	// the cached response's own
	BrowserCacheControl string
}

// CachedResponse describes a cached response
type CachedResponse struct {
	StatusCode int
	Bytes      int
	// Age is how long ago it was fetched (or how old it was then)
	Age time.Duration
	// FreshFor is how much longer it is fresh: negative if it is stale
	FreshFor time.Duration
	// Usable is whether it would be served (fresh, or stale while it is
	// refreshed)
	Usable bool
	// Expired is whether it has been soft purged
	Expired bool
}

// explainer is implemented by Cachers which can say how they would handle
// a request
type explainer interface {
	Explain(*http.Request) cache.Explanation
	Peek(hash string) (cache.Response, bool)
}

// Explain says how FetchWithOptions(c, ..., backend, opts) would handle r,
// which it changes as it would be (e.g. removing cookies).  Nothing is
// fetched or cached.
func Explain(c cache.Cacher, backend *url.URL, opts Options, r *http.Request) Explanation {
	var e Explanation
	step := func(outcome, title, detail string) {
		e.Steps = append(e.Steps, Step{Outcome: outcome, Title: title, Detail: detail})
	}
	SwitchBackend(r, backend)
	if r.Method == MethodPurge {
		e.Verdict = "A purge, not a request for a page"
		step(StepStopped, "Purges the cache", "if it comes from an address in control.allowIPs, or has the secret")
		return e
	}
	switch {
	case opts.Hosts == nil:
		step(StepOK, "Any host is served", "hosts isn't set")
	case HostAllowed(r.Host, opts.Hosts):
		step(StepOK, "Host allowed", fmt.Sprintf("%s is in hosts", r.Host))
	default:
		step(StepStopped, "Host turned away", fmt.Sprintf("%s isn't in hosts, so the request gets a 421 and never reaches the backend", r.Host))
		e.Verdict = "Turned away: not one of the hosts"
		return e
	}
	r = withRoute(c, r)
	x, canExplain := c.(explainer)
	var ce cache.Explanation
	if canExplain {
		ce = x.Explain(r)
		e.DefaultTTL, e.StaleWhileRevalidate, e.StaleIfError = ce.DefaultTTL, ce.StaleWhileRevalidate, ce.StaleIfError
		e.BrowserCacheControl = ce.BrowserCacheControl
		switch {
		case ce.NoCacheRoute != 0:
			// shown with the reason it isn't cached, below
		case ce.Route != nil:
			step(StepChanged, fmt.Sprintf("Route %d applies", ce.RouteNumber), fmt.Sprintf("it matches %s, so its settings apply", ce.Route.Describe()))
		default:
			step(StepOK, "No route applies", "the [cache] settings apply")
		}
	}
	if !c.CanCache(r) {
		reason := "the cache says it can't be cached"
		if canExplain {
			reason = ce.Uncacheable
			if ce.NoCacheRoute != 0 {
				reason = fmt.Sprintf("route %d (%s) has cache = false", ce.NoCacheRoute, routeMatch(c, ce.NoCacheRoute))
			}
		}
		step(StepStopped, "Not cacheable", reason)
		e.Verdict = "Not cached: every request goes to the backend"
		return e
	}
	e.Cacheable = true

	before := r.URL.Query()
	queryFilter(c, opts.QueryParams)(r)
	after := r.URL.Query()
	var removed, kept []string
	for name := range before {
		if _, ok := after[name]; ok {
			kept = append(kept, name)
		} else {
			removed = append(removed, name)
		}
	}
	sort.Strings(removed)
	sort.Strings(kept)
	switch {
	case len(before) == 0:
		step(StepOK, "No query parameters", "")
	case len(removed) == 0:
		step(StepOK, "Query parameters kept", strings.Join(kept, ", "))
	default:
		detail := "removed " + strings.Join(removed, ", ") + ", which aren't in queryParams"
		if len(kept) > 0 {
			detail += "; kept " + strings.Join(kept, ", ")
		}
		step(StepChanged, "Query parameters filtered", detail)
	}

	var cookiesBefore []string
	for _, cookie := range utilities.Cookies(r.Header) {
		cookiesBefore = append(cookiesBefore, cookie[0])
	}
	removeUnkeyed(c, r)
	var cookiesKept []string
	for _, cookie := range utilities.Cookies(r.Header) {
		cookiesKept = append(cookiesKept, cookie[0])
	}
	switch {
	case len(cookiesBefore) == 0:
		step(StepOK, "No cookies", "")
	case len(cookiesKept) == len(cookiesBefore):
		step(StepOK, "Cookies passed to the backend", strings.Join(cookiesKept, ", ")+": they're in allowedCookies, and part of the cache key")
	default:
		var dropped []string
		for _, name := range cookiesBefore {
			if !contains(cookiesKept, name) {
				dropped = append(dropped, name)
			}
		}
		detail := "removed " + strings.Join(dropped, ", ") + " before the backend sees them, as they aren't in allowedCookies"
		if len(cookiesKept) > 0 {
			detail += "; kept " + strings.Join(cookiesKept, ", ")
		}
		step(StepChanged, "Cookies removed", detail)
	}

	e.Key = c.Hash(r)
	if canExplain {
		if resp, found := x.Peek(e.Key); found {
			age, _ := strconv.Atoi(resp.Age())
			lifetime := freshnessLifetime(resp)
			cached := &CachedResponse{
				StatusCode: resp.StatusCode(),
				Bytes:      len(resp.Body()),
				Age:        time.Duration(age) * time.Second,
				FreshFor:   time.Duration(lifetime-age) * time.Second,
				Usable:     age < lifetime+utilities.StaleWhileRevalidate(resp.Header()),
			}
			if ex, ok := resp.(expirer); ok {
				cached.Expired = ex.Expired()
			}
			e.Cached = cached
		}
	}
	switch {
	case e.Cached == nil:
		e.Verdict = "Cacheable, not in the cache: the next request fetches and caches it"
	case e.Cached.FreshFor > 0:
		e.Verdict = "Cacheable, in the cache now"
	case e.Cached.Usable:
		e.Verdict = "Cacheable, stale: the next request gets it while it's refreshed"
	default:
		e.Verdict = "Cacheable, but too old to serve: the next request fetches it again"
	}
	return e
}

// routeMatch describes the route numbered n (from 1), if c can say
func routeMatch(c cache.Cacher, n int) string {
	if l, ok := c.(interface{ Routes() []*cache.Route }); ok {
		if routes := l.Routes(); n >= 1 && n <= len(routes) {
			return routes[n-1].Describe()
		}
	}
	return "?"
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
