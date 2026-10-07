package cache

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/davidjwilkins/honey/utilities"
	blake2b "github.com/minio/blake2b-simd"
)

const (
	// DefaultMaxBytes is the default limit on the approximate memory
	// used by cached responses.
	DefaultMaxBytes = 256 << 20
	// DefaultTTL is the default freshness lifetime given to responses
	// which don't specify one.
	DefaultTTL = 5 * time.Minute
)

// Options configures a Cacher created by NewCacher.
type Options struct {
	// MaxBytes bounds the approximate memory used by cached responses.
	// Once it is exceeded, the least recently used responses are
	// evicted.  Defaults to DefaultMaxBytes.
	MaxBytes int64
	// DefaultTTL is how long a response is considered fresh if it has no
	// explicit expiration (Cache-Control max-age or s-maxage, or Expires).
	// Defaults to DefaultTTL.
	DefaultTTL time.Duration
}

type defaultCacher struct {
	skipPrefixes       []string
	skipRegex          []*regexp.Regexp
	allowedCookies     map[string]bool
	allowedCookieNames []string
	defaultTTL         time.Duration
	// store holds both cached responses (keyed by their hash) and the
	// Vary header last seen for each URL (keyed by varyKey)
	store *store
}

// NewCacher returns an in-memory cacher with no site-specific rules.
func NewCacher(opts Options) *defaultCacher {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.DefaultTTL <= 0 {
		opts.DefaultTTL = DefaultTTL
	}
	return &defaultCacher{
		allowedCookies:     make(map[string]bool),
		allowedCookieNames: []string{},
		defaultTTL:         opts.DefaultTTL,
		store:              newStore(opts.MaxBytes),
	}
}

// NewDefaultCacher returns a cacher optimized
// for Wordpress - it will not cache the WP RSS
// feed, the wp-admin or wp-login pages, or previews.
func NewDefaultCacher() *defaultCacher {
	cacher := NewCacher(Options{})
	cacher.AddSkipRegex(regexp.MustCompile(`/(feed|wp-admin|wp-login)`))
	cacher.AddSkipRegex(regexp.MustCompile(`[?&]preview=true(&|$)`))
	return cacher
}

// CanCache will return true if the method is a GET or
// HEAD request, does not have a static file extension,
// does not have an Authorization header, and does not
// match any of the skip rules added with AddSkipPrefix
// or AddSkipRegex
func (c *defaultCacher) CanCache(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if utilities.IsStaticFile(r.URL.Path) {
		return false
	}
	if r.Header.Get("Authorization") != "" {
		return false
	}
	for _, prefix := range c.skipPrefixes {
		if strings.HasPrefix(r.URL.Path, prefix) {
			return false
		}
	}
	for _, regex := range c.skipRegex {
		if regex.MatchString(r.URL.RequestURI()) {
			return false
		}
	}
	return true
}

// AddSkipPrefix prevents requests whose path starts with prefix
// from being cached.
func (c *defaultCacher) AddSkipPrefix(prefix string) {
	c.skipPrefixes = append(c.skipPrefixes, prefix)
}

// AddSkipRegex prevents requests whose path and query string
// (e.g. /page?preview=true) match regex from being cached.
func (c *defaultCacher) AddSkipRegex(regex *regexp.Regexp) {
	c.skipRegex = append(c.skipRegex, regex)
}

func baseKey(r *http.Request) string {
	return fmt.Sprintf("%s :: %s", r.Method, r.URL.String())
}

func varyKey(base string) string {
	return "vary :: " + base
}

// Hash creates a unique string for a request.  It includes
// the method, the url, and the values of any headers (and
// allowed cookies) listed in the Vary header of the last
// response for that url.  It also includes the X-Honey-Vary
// header - which is used internally on multiplexed requests.
func (c *defaultCacher) Hash(r *http.Request) string {
	hash := baseKey(r)
	if vary, found := c.store.get(varyKey(hash), time.Now()); found {
		hash += utilities.GetVaryHeadersHash(r.Header, r, c.allowedCookieNames, vary.(string))
	}
	if vary := r.Header.Get("X-Honey-Vary"); vary != "" {
		hash += vary
	}
	return hash
}

// AddAllowedCookie adds a name to the list of cookies which
// are allowed through the cache.
func (c *defaultCacher) AddAllowedCookie(name string) {
	c.allowedCookies[name] = true
	c.allowedCookieNames = append(c.allowedCookieNames, name)
}

func addDirective(cc, directive string) string {
	if strings.TrimSpace(cc) == "" {
		return directive
	}
	return cc + ", " + directive
}

func removeDirective(cc, directive string) string {
	var kept []string
	for _, part := range strings.Split(cc, ",") {
		if part = strings.TrimSpace(part); part != "" && part != directive {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, ", ")
}

// Standardize removes set-cookie headers unless they are listed
// in the allowed cookies, fills in the Date, Cache-Control,
// Last-Modified, Expires and Etag headers if they are missing,
// reads the response, and saves it to a Response interface.
func (c *defaultCacher) Standardize(r *http.Response) Response {
	for i := 0; i < len(r.Header["Set-Cookie"]); i++ {
		line := r.Header["Set-Cookie"][i]
		parts := strings.Split(strings.TrimSpace(line), ";")
		if len(parts) == 1 && parts[0] == "" {
			continue
		}
		parts[0] = strings.TrimSpace(parts[0])
		j := strings.Index(parts[0], "=")
		if j < 0 {
			continue
		}
		name := parts[0][:j]
		if allowed, ok := c.allowedCookies[name]; !allowed || !ok {
			r.Header["Set-Cookie"] = append(r.Header["Set-Cookie"][:i], r.Header["Set-Cookie"][i+1:]...)
			i--
		}
	}
	now := time.Now()
	// Changes are made to r.Header so that the requester whose request
	// populated the cache sees the same headers as later cache hits.
	if r.Header.Get("Date") == "" {
		r.Header.Set("Date", now.UTC().Format(http.TimeFormat))
	}
	cc := r.Header.Get("Cache-Control")
	if _, private := utilities.Directive(cc, "private"); !private {
		if _, public := utilities.Directive(cc, "public"); !public {
			cc = addDirective(cc, "public")
		}
	}
	_, noCache := utilities.Directive(cc, "no-cache")
	_, maxAge := utilities.Directive(cc, "max-age")
	_, sMaxAge := utilities.Directive(cc, "s-maxage")
	if !noCache && !maxAge && !sMaxAge && r.Header.Get("Expires") == "" {
		cc = addDirective(cc, fmt.Sprintf("max-age=%d", int(c.defaultTTL/time.Second)))
	}
	r.Header.Set("Cache-Control", cc)

	if r.Header.Get("Last-Modified") == "" {
		r.Header.Set("Last-Modified", now.UTC().Format(http.TimeFormat))
	}
	if r.Header.Get("Expires") == "" {
		if seconds, found := utilities.DirectiveSeconds(cc, "max-age"); found {
			r.Header.Set("Expires", now.Add(time.Duration(seconds)*time.Second).UTC().Format(http.TimeFormat))
		}
	}

	resp := responseImpl{
		now:      now,
		response: r,
	}
	if r.Request != nil {
		resp.baseKey = baseKey(r.Request)
	}
	resp.initialAge, _ = strconv.Atoi(r.Header.Get("Age"))
	resp.body, _ = io.ReadAll(r.Body)
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(resp.body))
	if _, noStore := utilities.Directive(cc, "no-store"); !noStore {
		hasher := blake2b.New256()
		hasher.Write(resp.body)
		r.Header.Set("Etag", `"`+base64.StdEncoding.EncodeToString(hasher.Sum(nil))+`"`)
	}
	resp.headers = r.Header.Clone()

	// no-cache="set-cookie" allows the response to be cached, as long as
	// the Set-Cookie header isn't
	if strings.Contains(cc, `no-cache="set-cookie"`) {
		resp.headers.Del("Set-Cookie")
		resp.headers.Set("Cache-Control", removeDirective(cc, `no-cache="set-cookie"`))
	}
	resp.cookies = make(map[string]*http.Cookie)
	for _, cookie := range resp.response.Cookies() {
		resp.cookies[cookie.Name] = cookie
	}
	if r.Request != nil {
		resp.requestHeaders = r.Request.Header
	} else {
		resp.requestHeaders = http.Header{}
	}

	return &resp
}

// Cache will store the Response in the cache for later retrieval.  It is
// kept until it can no longer be served - not even as stale content (per
// stale-while-revalidate or stale-if-error) - or until it is evicted to
// keep the cache within its size limit.
func (c *defaultCacher) Cache(hash string, r Response) {
	// The hash may already include the Vary values from a previous response
	// for this URL; store the response under the key for its own Vary.
	base := hash
	if impl, ok := r.(*responseImpl); ok && impl.baseKey != "" {
		base = impl.baseKey
	}
	vary := r.Header().Get("Vary")
	if vary != "" {
		c.store.set(varyKey(base), vary, int64(len(base)+len(vary)), time.Time{})
	} else {
		c.store.delete(varyKey(base))
	}
	key := base + utilities.GetVaryHeadersHash(r.RequestHeaders(), r, c.allowedCookieNames, vary)

	now := time.Now()
	age, _ := strconv.Atoi(r.Age())
	keep := utilities.FreshnessLifetime(r.Header()) - age
	staleIfError, forever := utilities.StaleIfError(r.Header().Get("Cache-Control"))
	var removeAt time.Time
	if !forever {
		stale := utilities.StaleWhileRevalidate(r.Header())
		if staleIfError > stale {
			stale = staleIfError
		}
		keep += stale
		if keep <= 0 {
			// It can never be served, so don't let it replace anything
			c.store.delete(key)
			return
		}
		removeAt = now.Add(time.Duration(keep) * time.Second)
	}
	c.store.set(key, r, responseSize(key, r), removeAt)
}

// responseSize approximates the memory used to cache a response
func responseSize(key string, r Response) int64 {
	size := len(key) + len(r.Body()) + 256
	for name, values := range r.Header() {
		for _, value := range values {
			size += len(name) + len(value)
		}
	}
	return int64(size)
}

// Load returns a Response from the cache.  It returns the Response, if found, and
// a boolean indicating whether or not it was found.  The Response may be stale:
// it is up to the caller to check whether it is still fresh enough to use.
func (c *defaultCacher) Load(hash string, request *http.Request) (Response, bool) {
	r, ok := c.store.get(hash, time.Now())
	if !ok {
		return nil, false
	}
	return r.(Response), true
}

// Stats returns the number of entries in the cache, and their approximate
// size in bytes.
func (c *defaultCacher) Stats() (entries int, bytes int64) {
	return c.store.stats()
}

func (c *defaultCacher) AllowedCookies() []string {
	return c.allowedCookieNames
}
