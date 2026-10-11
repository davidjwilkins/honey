package cache

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	// DefaultMaxObjectBytes is the default limit on the size of a single
	// response body which may be cached.
	DefaultMaxObjectBytes = 10 << 20
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
	// MaxObjectBytes is the largest response body which will be cached.
	// Larger responses are streamed to the client without being cached.
	// Defaults to DefaultMaxObjectBytes, and is never more than MaxBytes.
	MaxObjectBytes int64
	// StaleIfError is how long past their freshness lifetime responses may
	// be served if the backend errors or can't be reached, unless they
	// have their own stale-if-error directive.  Zero means only responses
	// with stale-if-error are served stale.
	StaleIfError time.Duration
	// SkipStaticFiles stops requests for static files (images, css, js,
	// fonts, media, documents, archives...) from being cached.
	SkipStaticFiles bool
	// DisableBrotli stops compressible responses (html, css, js, json,
	// svg...) from being brotli compressed for clients which accept it.
	DisableBrotli bool
	// DisableGzip stops compressible responses from being gzip compressed
	// for clients which accept gzip but not brotli.
	DisableGzip bool
	// CompressMinBytes is the smallest response which is compressed (with
	// brotli or gzip).  Defaults to DefaultCompressMinBytes.
	CompressMinBytes int64
}

type defaultCacher struct {
	routes             []*Route
	allowedCookies     map[string]bool
	allowedCookieNames []string
	defaultTTL         time.Duration
	staleIfError       time.Duration
	maxObjectBytes     int64
	skipStaticFiles    bool
	brotli             bool
	gzip               bool
	compressMinBytes   int
	recompressor       *recompressor
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
	if opts.MaxObjectBytes <= 0 {
		opts.MaxObjectBytes = DefaultMaxObjectBytes
	}
	if opts.MaxObjectBytes > opts.MaxBytes {
		opts.MaxObjectBytes = opts.MaxBytes
	}
	if opts.CompressMinBytes <= 0 {
		opts.CompressMinBytes = DefaultCompressMinBytes
	}
	return &defaultCacher{
		allowedCookies:     make(map[string]bool),
		allowedCookieNames: []string{},
		defaultTTL:         opts.DefaultTTL,
		staleIfError:       opts.StaleIfError,
		maxObjectBytes:     opts.MaxObjectBytes,
		skipStaticFiles:    opts.SkipStaticFiles,
		brotli:             !opts.DisableBrotli,
		gzip:               !opts.DisableGzip,
		compressMinBytes:   int(opts.CompressMinBytes),
		recompressor:       &recompressor{},
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
// HEAD request, is not for a static file (if SkipStaticFiles
// is set), does not have an Authorization header, and does not
// match a route with NoCache set
func (c *defaultCacher) CanCache(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if c.skipStaticFiles && utilities.IsStaticFile(r.URL.Path) {
		return false
	}
	if r.Header.Get("Authorization") != "" {
		return false
	}
	return !c.routing(r).noCache
}

// AddSkipPrefix prevents requests whose path starts with prefix
// from being cached.
func (c *defaultCacher) AddSkipPrefix(prefix string) {
	c.AddRoute(Route{Prefix: prefix, NoCache: true})
}

// AddSkipRegex prevents requests whose path and query string
// (e.g. /page?preview=true) match regex from being cached.
func (c *defaultCacher) AddSkipRegex(regex *regexp.Regexp) {
	c.AddRoute(Route{Regex: regex, NoCache: true})
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
// response for that url.
func (c *defaultCacher) Hash(r *http.Request) string {
	hash := baseKey(r)
	if vary, found := c.store.get(varyKey(hash), time.Now()); found {
		hash += utilities.GetVaryHeadersHash(r.Header, r, c.allowedCookieNames, vary.(string))
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
	defaultTTL, staleIfError := c.defaultTTL, c.staleIfError
	if route := c.Route(r.Request); route != nil {
		if route.DefaultTTL > 0 {
			defaultTTL = route.DefaultTTL
		}
		if route.StaleIfError != nil {
			staleIfError = *route.StaleIfError
		}
	}
	if !noCache && !maxAge && !sMaxAge && r.Header.Get("Expires") == "" {
		cc = addDirective(cc, fmt.Sprintf("max-age=%d", int(defaultTTL/time.Second)))
	}
	// https://www.rfc-editor.org/rfc/rfc5861#section-4 - the cache keeps
	// responses for this long past their freshness, in case it needs them
	if _, found := utilities.Directive(cc, "stale-if-error"); !found && staleIfError >= time.Second {
		cc = addDirective(cc, fmt.Sprintf("stale-if-error=%d", int(staleIfError/time.Second)))
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
		now:        now,
		response:   r,
		status:     r.Status,
		statusCode: r.StatusCode,
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
	if (c.brotli || c.gzip) && shouldCompress(r.StatusCode, r.Header, resp.body, c.compressMinBytes) {
		if c.brotli {
			if compressed := compressBrotli(resp.body, fastBrotliQuality); len(compressed) < len(resp.body) {
				resp.brotli.Store(&compressed)
			}
		}
		if c.gzip {
			if compressed := compressGzip(resp.body); len(compressed) < len(resp.body) {
				resp.gzip = compressed
			}
		}
		if (resp.brotliBody() != nil || resp.gzip != nil) && !headerListContains(r.Header.Values("Vary"), "Accept-Encoding") {
			r.Header.Add("Vary", "Accept-Encoding")
		}
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
	// Bodies are cached unencoded, and encoded for each client as they're
	// served, so a response encoded by the backend can't be cached (and
	// Accept-Encoding isn't part of the cache key).
	if encoding := r.Header().Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return
	}
	vary := utilities.CacheVary(strings.Join(r.Header().Values("Vary"), ","))
	if vary != "" {
		c.store.set(varyKey(base), vary, int64(len(base)+len(vary)), time.Time{})
	} else {
		c.store.delete(varyKey(base))
	}
	// Key on the cookies of the request the response was fetched for (as
	// Hash does), not the cookies the response sets.
	fetchedFor := &http.Request{Header: r.RequestHeaders()}
	key := base + utilities.GetVaryHeadersHash(fetchedFor.Header, fetchedFor, c.allowedCookieNames, vary)
	if int64(len(r.Body())) > c.maxObjectBytes {
		c.store.delete(key)
		return
	}

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

// headerListContains returns whether a comma separated header (such as
// Vary) contains name
func headerListContains(values []string, name string) bool {
	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(item), name) {
				return true
			}
		}
	}
	return false
}

// responseSize approximates the memory used to cache a response
func responseSize(key string, r Response) int64 {
	size := len(key) + len(r.Body()) + 256
	if impl, ok := r.(*responseImpl); ok {
		// Recompressing in the background only makes this smaller
		size += len(impl.brotliBody()) + len(impl.gzip)
	}
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
	// Recompress responses which are actually being served from the cache
	if impl, ok := r.(*responseImpl); ok && impl.loads.Add(1) == 1 {
		c.recompressor.add(impl)
	}
	return r.(Response), true
}

// Purge removes every cached response for u - every method, and every
// variant of it (for responses which Vary).  If prefix is true, it removes
// every cached response whose URL starts with u instead (e.g. everything
// under https://www.example.com/blog/).  It returns the number of
// responses removed.
func (c *defaultCacher) Purge(u *url.URL, prefix bool) int {
	target := u.String()
	responses := 0
	c.store.deleteMatching(func(key string) bool {
		vary := strings.HasPrefix(key, varyKey(""))
		key = strings.TrimPrefix(key, varyKey(""))
		_, keyURL, found := strings.Cut(key, " :: ")
		if !found {
			return false
		}
		var match bool
		if prefix {
			match = strings.HasPrefix(keyURL, target)
		} else {
			// Variant keys are the URL followed by "::" and the values of
			// the headers the response Varies on (see GetVaryHeadersHash)
			match = keyURL == target || strings.HasPrefix(keyURL, target+"::") || strings.HasPrefix(keyURL, target+" :: ")
		}
		if match && !vary {
			responses++
		}
		return match
	})
	return responses
}

// Stats returns the number of entries in the cache, and their approximate
// size in bytes.
func (c *defaultCacher) Stats() (entries int, bytes int64) {
	return c.store.stats()
}

// MaxObjectSize returns the size of the largest response body which
// will be cached.
func (c *defaultCacher) MaxObjectSize() int64 {
	return c.maxObjectBytes
}

func (c *defaultCacher) AllowedCookies() []string {
	return c.allowedCookieNames
}
