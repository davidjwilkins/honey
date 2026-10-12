package cache

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/davidjwilkins/honey/events"
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
	// StaleWhileRevalidate is how long past their freshness lifetime
	// responses may be served while they are refreshed in the background,
	// unless they have their own stale-while-revalidate directive (or
	// no-cache).  Visitors then never wait for the backend for a page
	// which has been cached, as long as it is requested within this long
	// of expiring.  Zero means only responses with stale-while-revalidate
	// are.
	StaleWhileRevalidate time.Duration
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
	// BypassCookies are cookie name prefixes which mark a visitor whose
	// pages may be personal, such as a logged in user (see
	// WordPressBypassCookies).  Requests with any of them are neither
	// served from the cache nor cached, and responses which set one
	// aren't cached.
	BypassCookies []string
	// StripCookies are cookie name prefixes which responses may set
	// without being personal, such as a language cookie which the page
	// doesn't depend on (e.g. Polylang's pll_language).  They are removed
	// from responses, which are cached.  Responses which set any other
	// cookie (unless it is allowed - see AddAllowedCookie) aren't cached,
	// and keep it.
	StripCookies []string
	// ESICookies are bypass cookie name prefixes (see BypassCookies) whose
	// visitors may be served cached pages which use Edge Side Includes
	// for the parts which differ between visitors (see esi.go) - e.g.
	// commenters, whose comments section is an include.  Pages which don't
	// use ESI go to the backend for them, as for any bypass cookie.
	ESICookies []string
	// Events, if set, is where things worth knowing about (such as
	// responses not being cached because of their cookies) are logged,
	// e.g. for the admin page.
	Events *events.Log
	// BrowserCacheControl, if set, replaces the Cache-Control header sent
	// to clients with successful responses which are served from (or
	// stored in) the cache - e.g. "no-cache", so that browsers check back
	// (cheaply, with the Etag) and see purges straight away, or "public,
	// max-age=31536000, immutable" for versioned assets.  It doesn't
	// change how long Honey caches them.
	BrowserCacheControl string
	// RewritePage, if set, changes the body of each response as it's
	// cached - e.g. to make pages load faster (see the optimize package).
	// It's given the backend's response, and returns body itself if it
	// doesn't change it.
	RewritePage func(r *http.Response, body []byte) []byte
}

type defaultCacher struct {
	routes             []*Route
	allowedCookies     map[string]bool
	allowedCookieNames []string
	defaultTTL         time.Duration
	staleIfError       time.Duration
	staleWhileReval    time.Duration
	maxObjectBytes     int64
	skipStaticFiles    bool
	brotli             bool
	gzip               bool
	compressMinBytes   int
	bypassCookies      []string
	stripCookies       []string
	esiCookies         []string
	browserCC          string
	rewritePage        func(r *http.Response, body []byte) []byte
	recompressor       *recompressor
	// store holds both cached responses (keyed by their hash) and the
	// Vary header last seen for each URL (keyed by varyKey)
	store *store
	// loggedCookies are the cookies logUnknownCookie has logged
	loggedMu      sync.Mutex
	loggedCookies map[string]bool
	events        *events.Log
	// fragments are the current content of pages' shared fragments
	fragments *fragments
	// hostHints are the Early Hints of hosts' recently cached pages
	hostHints hostHints
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
		staleWhileReval:    opts.StaleWhileRevalidate,
		maxObjectBytes:     opts.MaxObjectBytes,
		skipStaticFiles:    opts.SkipStaticFiles,
		brotli:             !opts.DisableBrotli,
		gzip:               !opts.DisableGzip,
		compressMinBytes:   int(opts.CompressMinBytes),
		bypassCookies:      append([]string(nil), opts.BypassCookies...),
		stripCookies:       append([]string(nil), opts.StripCookies...),
		esiCookies:         append([]string(nil), opts.ESICookies...),
		loggedCookies:      make(map[string]bool),
		fragments:          newFragments(),
		events:             opts.Events,
		browserCC:          opts.BrowserCacheControl,
		rewritePage:        opts.RewritePage,
		recompressor:       &recompressor{},
		store:              newStore(opts.MaxBytes),
	}
}

// NewDefaultCacher returns a cacher optimized
// for Wordpress - it will not cache the WP RSS
// feed, the wp-admin or wp-login pages, or previews,
// or pages for logged in users, commenters or shoppers
// (see WordPressBypassCookies).
func NewDefaultCacher() *defaultCacher {
	cacher := NewCacher(Options{BypassCookies: WordPressBypassCookies})
	cacher.AddSkipRegex(regexp.MustCompile(`/(feed|wp-admin|wp-login)`))
	cacher.AddSkipRegex(regexp.MustCompile(`[?&]preview=true(&|$)`))
	return cacher
}

// CanCache will return true if the method is a GET or
// HEAD request, is not for a static file (if SkipStaticFiles
// is set), does not have an Authorization header or any of the
// BypassCookies, and does not match a route with NoCache set
func (c *defaultCacher) CanCache(r *http.Request) bool {
	return c.uncacheable(r) == ""
}

// uncacheable returns why r can't be cached (see CanCache), or "" if it can
func (c *defaultCacher) uncacheable(r *http.Request) string {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return "only GET and HEAD requests are cached"
	}
	if c.skipStaticFiles && utilities.IsStaticFile(r.URL.Path) {
		return "static files aren't cached (staticFiles = false)"
	}
	if r.Header.Get("Authorization") != "" {
		return "it has an Authorization header"
	}
	if name := c.bypassCookie(r); name != "" {
		return fmt.Sprintf("it has the cookie %s, which marks visitors whose pages may be personal (bypassCookies)", name)
	}
	if c.routing(r).noCache {
		return "a route stops it being cached"
	}
	return ""
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

// baseKey identifies the resource r is for: its method, and its URL as the
// client asked for it - with the host and scheme it sent the request to,
// which fetch.SwitchBackend records in X-Forwarded-Host and
// X-Forwarded-Proto before replacing them with the backend's.
func baseKey(r *http.Request) string {
	u := *r.URL
	if host := r.Header.Get("X-Forwarded-Host"); host != "" {
		u.Host = strings.ToLower(host)
	} else if r.Host != "" {
		u.Host = strings.ToLower(r.Host)
	}
	switch proto := strings.ToLower(r.Header.Get("X-Forwarded-Proto")); proto {
	case "http", "https":
		u.Scheme = proto
	}
	return fmt.Sprintf("%s :: %s", r.Method, u.String())
}

func varyKey(base string) string {
	return "vary :: " + base
}

// Hash creates a unique string for a request.  It includes
// the method, the url, the values of any headers (and
// allowed cookies) listed in the Vary header of the last
// response for that url, and any allowed cookies.
func (c *defaultCacher) Hash(r *http.Request) string {
	hash := baseKey(r)
	if vary, found := c.store.get(varyKey(hash), time.Now()); found {
		hash += utilities.GetVaryHeadersHash(r.Header, r, c.allowedCookieNames, vary.(string))
	}
	return hash + c.cookieKey(r.Header)
}

// cookieKey returns the allowed cookies in a request's header, for its
// cache key.  They are passed to the backend (unlike other cookies), so
// its response may depend on them, whether or not it says so with Vary.
func (c *defaultCacher) cookieKey(header http.Header) string {
	if len(c.allowedCookieNames) == 0 {
		return ""
	}
	var allowed []string
	for _, cookie := range utilities.Cookies(header) {
		if c.allowedCookies[cookie[0]] {
			allowed = append(allowed, cookie[0]+"="+cookie[1])
		}
	}
	if len(allowed) == 0 {
		return ""
	}
	sort.Strings(allowed)
	return " :: cookies :: " + strings.Join(allowed, "; ")
}

// AddAllowedCookie adds a name to the list of cookies which
// are allowed through the cache: they are passed to the
// backend on cacheable requests, and are part of the cache
// key, and responses may set them.
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
// If the response says its Set-Cookie header is private, the
// requester keeps all of it, but it isn't cached.
func (c *defaultCacher) Standardize(r *http.Response) Response {
	for i := 0; !setCookieIsPrivate(r.Header.Get("Cache-Control")) && i < len(r.Header["Set-Cookie"]); i++ {
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
	defaultTTL, staleIfError, staleWhileRevalidate := c.defaultTTL, c.staleIfError, c.staleWhileReval
	if route := c.Route(r.Request); route != nil {
		if route.DefaultTTL > 0 {
			defaultTTL = route.DefaultTTL
		}
		if route.StaleIfError != nil {
			staleIfError = *route.StaleIfError
		}
		if route.StaleWhileRevalidate != nil {
			staleWhileRevalidate = *route.StaleWhileRevalidate
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
	// https://www.rfc-editor.org/rfc/rfc5861#section-3 - but not for
	// responses which must be revalidated before they're used
	if _, found := utilities.Directive(cc, "stale-while-revalidate"); !found && !noCache && staleWhileRevalidate >= time.Second {
		cc = addDirective(cc, fmt.Sprintf("stale-while-revalidate=%d", int(staleWhileRevalidate/time.Second)))
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
	resp.body = exactly(resp.body)
	r.Body.Close()
	if c.rewritePage != nil {
		if rewritten := c.rewritePage(r, resp.body); len(rewritten) != len(resp.body) || !bytes.Equal(rewritten, resp.body) {
			resp.body = exactly(rewritten)
			if r.Header.Get("Content-Length") != "" {
				r.Header.Set("Content-Length", strconv.Itoa(len(resp.body)))
			}
			r.ContentLength = int64(len(resp.body))
		}
	}
	// Shared fragments (see fragments.go) are kept separately, and their
	// markers' tokens removed
	if r.Request != nil && isHTML(r.Header) {
		if resp.segments = parseFragments(resp.body, r.Request.Header.Get(FragmentTokenHeader)); resp.segments != nil {
			resp.body, resp.segments = assemble(resp.segments, func(string) []byte { return nil })
			if r.Header.Get("Content-Length") != "" {
				r.Header.Set("Content-Length", strconv.Itoa(len(resp.body)))
			}
			r.ContentLength = int64(len(resp.body))
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(resp.body))
	// Edge Side Includes, if the backend uses them (see esi.go)
	esi := usesESI(r)
	mark := ""
	if esi {
		mark = esiMark(r.Header, resp.body)
	}
	r.Header.Del("Surrogate-Control")
	r.Header.Del(ESICookiesHeader)
	r.Header.Del(ESIHeader)
	if esi {
		r.Header.Set(ESIHeader, mark)
	}
	_, noStore := utilities.Directive(cc, "no-store")
	c.encode(&resp, r.StatusCode, r.Header, !noStore)
	resp.headers = r.Header.Clone()

	// no-cache="set-cookie" and private="set-cookie" allow the response
	// to be cached, as long as the Set-Cookie header isn't
	if setCookieIsPrivate(cc) {
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

// encode sets resp's Etag in header (if etag), and compresses its body
// for clients which accept brotli or gzip, if it's worth it
func (c *defaultCacher) encode(resp *responseImpl, statusCode int, header http.Header, etag bool) {
	if etag {
		hasher := blake2b.New256()
		hasher.Write(resp.body)
		header.Set("Etag", `"`+base64.StdEncoding.EncodeToString(hasher.Sum(nil))+`"`)
	}
	// Pages with Edge Side Includes are compressed once they're put
	// together for each visitor - unless they're only put together for
	// some, and served to everyone else as they are
	if mode, _, _ := strings.Cut(header.Get(ESIHeader), " "); mode != "" && mode != "personal" {
		return
	}
	if (c.brotli || c.gzip) && shouldCompress(statusCode, header, resp.body, c.compressMinBytes) {
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
		if (resp.brotliBody() != nil || resp.gzip != nil) && !headerListContains(header.Values("Vary"), "Accept-Encoding") {
			header.Add("Vary", "Accept-Encoding")
		}
	}
}

// Cache will store the Response in the cache for later retrieval.  It is
// kept until it can no longer be served - not even as stale content (per
// stale-while-revalidate or stale-if-error) - or until it is evicted to
// keep the cache within its size limit.  Responses which a shared cache
// mustn't store - those marked private or no-store, or fetched for a
// request with one of the BypassCookies - aren't stored.
func (c *defaultCacher) Cache(hash string, r Response) {
	if !c.storable(r) {
		return
	}
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
	key := base + utilities.GetVaryHeadersHash(fetchedFor.Header, fetchedFor, c.allowedCookieNames, vary) +
		c.cookieKey(fetchedFor.Header)
	if int64(len(r.Body())) > c.maxObjectBytes {
		c.store.delete(key)
		return
	}

	if impl, ok := r.(*responseImpl); ok {
		if impl.segments != nil {
			c.recordFragments(impl)
		}
		c.learnHints(impl)
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
	if impl, ok := r.(*responseImpl); ok && impl.segments != nil {
		r = c.withCurrentFragments(hash, impl)
	}
	// Recompress responses which are actually being served from the cache
	if impl, ok := r.(*responseImpl); ok && impl.loads.Add(1) == 1 {
		c.recompressor.add(impl)
	}
	return r.(Response), true
}

// Purge removes every cached response for u's path and query - every
// method, every host and scheme, and every variant of it (for responses
// which Vary).  If prefix is true, it removes every cached response whose
// path starts with u's instead (e.g. everything under /blog/).  It returns
// the number of responses removed.
//
// The host is ignored so that purges work however they reach Honey (e.g.
// sent to 127.0.0.1 by WordPress, for a site cached as www.example.com).
// If one backend serves several sites, purging a path purges it on each.
func (c *defaultCacher) Purge(u *url.URL, prefix bool) int {
	match := purgeMatcher(u, prefix)
	responses := 0
	c.store.deleteMatching(func(key string) bool {
		matched, vary := match(key)
		if matched && !vary {
			responses++
		}
		return matched
	})
	return responses
}

// Count returns how many cached responses Purge(u, prefix) would remove.
func (c *defaultCacher) Count(u *url.URL, prefix bool) int {
	match := purgeMatcher(u, prefix)
	responses := 0
	c.store.forEach(func(key string, value interface{}) {
		if matched, vary := match(key); matched && !vary {
			responses++
		}
	})
	return responses
}

// Expire soft purges the cached responses Purge(u, prefix) would remove:
// they are treated as stale from now on, but they are kept, so that they
// can still be served while they are refreshed (if they allow
// stale-while-revalidate) or if the backend fails (if they allow
// stale-if-error).  So visitors needn't wait for the backend after a
// purge, as long as they can have the old version once more.  It returns
// the number of responses expired.
func (c *defaultCacher) Expire(u *url.URL, prefix bool) int {
	match := purgeMatcher(u, prefix)
	responses := 0
	c.store.forEach(func(key string, value interface{}) {
		if matched, vary := match(key); matched && !vary {
			if r, ok := value.(*responseImpl); ok && !r.expired.Swap(true) {
				responses++
			}
		}
	})
	return responses
}

// CachedPaths returns the paths (with any query strings) of the cached
// responses to GET requests whose paths start with prefix, for any host,
// up to max of them - e.g. to purge them from a CDN too
func (c *defaultCacher) CachedPaths(prefix string, max int) []string {
	seen := map[string]bool{}
	var paths []string
	c.store.forEach(func(key string, value interface{}) {
		if len(paths) >= max || strings.HasPrefix(key, varyKey("")) {
			return
		}
		method, keyURL, found := strings.Cut(key, " :: ")
		if !found || method != http.MethodGet {
			return
		}
		p := withoutOrigin(keyURL)
		// Variant keys are the URL followed by "::" and Vary values
		if i := strings.Index(p, "::"); i >= 0 {
			p = strings.TrimSpace(p[:i])
		}
		if strings.HasPrefix(p, prefix) && !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	})
	sort.Strings(paths)
	return paths
}

// purgeMatcher returns a function which says whether a store key is for
// u (see Purge), and whether it is a Vary record rather than a response.
func purgeMatcher(u *url.URL, prefix bool) func(key string) (matched, vary bool) {
	target := withoutOrigin(u.String())
	return func(key string) (bool, bool) {
		vary := strings.HasPrefix(key, varyKey(""))
		key = strings.TrimPrefix(key, varyKey(""))
		_, keyURL, found := strings.Cut(key, " :: ")
		if !found {
			return false, vary
		}
		keyURL = withoutOrigin(keyURL)
		var match bool
		if prefix {
			match = strings.HasPrefix(keyURL, target)
		} else {
			// Variant keys are the URL followed by "::" and the values of
			// the headers the response Varies on (see GetVaryHeadersHash)
			match = keyURL == target || strings.HasPrefix(keyURL, target+"::") || strings.HasPrefix(keyURL, target+" :: ")
		}
		return match, vary
	}
}

// withoutOrigin returns rawURL without its scheme and host, e.g. "/blog/"
// for "https://www.example.com/blog/" (and any text after the URL, such as
// a variant key's Vary values)
func withoutOrigin(rawURL string) string {
	scheme := strings.Index(rawURL, "://")
	if scheme < 0 {
		return rawURL
	}
	rest := rawURL[scheme+len("://"):]
	path := strings.IndexAny(rest, "/?:")
	if path < 0 {
		return "/"
	}
	// A port isn't the path
	if rest[path] == ':' {
		if end := strings.IndexAny(rest[path:], "/?"); end >= 0 {
			path += end
		} else {
			return "/"
		}
	}
	if rest[path] == '?' {
		return "/" + rest[path:]
	}
	return rest[path:]
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
