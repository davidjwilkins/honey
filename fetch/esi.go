package fetch

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/davidjwilkins/honey/cache"
	"github.com/davidjwilkins/honey/utilities"
)

// Limits on Edge Side Includes (see cache/esi.go)
const (
	// maxESIDepth is how deeply includes may include others
	maxESIDepth = 3
	// maxIncludes is how many includes a page may have
	maxIncludes = 20
	// includeTimeout bounds how long an include may take
	includeTimeout = 10 * time.Second
	// ESIParam is the query parameter which is always kept on cacheable
	// requests, even if queryParams doesn't list it, so that it can name
	// what an include's URL renders (e.g. /my-post/?honey-esi=comments)
	ESIParam = "honey-esi"
)

var (
	esiComment = regexp.MustCompile(`(?s)<!--esi(.*?)-->`)
	esiRemove  = regexp.MustCompile(`(?s)<esi:remove>.*?</esi:remove>`)
	esiInclude = regexp.MustCompile(`(?s)<esi:include\s([^>]*?)/?>(?:\s*</esi:include>)?`)
	esiAttr    = regexp.MustCompile(`([a-z]+)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
)

type esiDepthKey struct{}

func esiDepth(ctx context.Context) int {
	depth, _ := ctx.Value(esiDepthKey{}).(int)
	return depth
}

// headersNotForIncludes are the visitor's request headers which aren't
// sent with their requests for a page's includes: they're about the page,
// or are the cache's to set
var headersNotForIncludes = []string{
	"Accept-Encoding", "Cache-Control", "Pragma", "Range", "If-Range",
	"If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since",
	"Content-Length", "Content-Type", "Surrogate-Capability", cache.FragmentTokenHeader,
}

// esiWriter is a ResponseWriter which, if the response is a cached page
// which uses ESI (see cache.ESIHeader), holds it back, fills in its
// includes for the visitor, and then writes it.  Anything else passes
// straight through.
type esiWriter struct {
	http.ResponseWriter
	// request is the request the page is for, which the proxy changes as
	// it handles it (e.g. removing cookies), and host and cookies its Host
	// and Cookie headers as the visitor sent them
	request *http.Request
	host    string
	cookies []string
	// visitor is request as the visitor sent it, so that the page's
	// includes are fetched as they would be for them: made only for pages
	// which use ESI
	visitor *http.Request
	// cached is the cached page being written, if the cache says (see
	// noteCached), whose parsed form is kept with it
	cached cache.Response
	// personal is whether the page is being served to a visitor with
	// bypass cookies (see serveESIPage), rather than one without
	personal bool
	// handler fetches includes: the caching proxy
	handler      http.Handler
	secretHeader string

	status    int
	wroteHead bool
	holding   bool
	body      bytes.Buffer
}

func (e *esiWriter) WriteHeader(status int) {
	if e.wroteHead {
		return
	}
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		// Informational, e.g. Early Hints: the response is still to come
		e.ResponseWriter.WriteHeader(status)
		return
	}
	e.wroteHead = true
	h := e.Header()
	mark := h.Get(cache.ESIHeader)
	h.Del(cache.ESIHeader)
	// Pages which only use ESI for visitors with bypass cookies are
	// served to everyone else as they are
	if mode, _, _ := strings.Cut(mark, " "); mode == "" || (mode == "personal" && !e.personal) {
		e.ResponseWriter.WriteHeader(status)
		return
	}
	// The page differs between visitors: its validators are the cached
	// page's, and browsers mustn't share or keep it
	for _, name := range []string{"Etag", "Last-Modified", "Content-Length", "Content-Encoding", "Content-Range"} {
		h.Del(name)
	}
	h.Set("Cache-Control", "private, no-cache")
	e.visitor = e.request.Clone(e.request.Context())
	e.visitor.Host = e.host
	if e.cookies != nil {
		e.visitor.Header["Cookie"] = e.cookies
	} else {
		e.visitor.Header.Del("Cookie")
	}
	if e.visitor.Method == http.MethodHead || status == http.StatusNotModified || status == http.StatusNoContent {
		e.ResponseWriter.WriteHeader(status)
		return
	}
	e.status, e.holding = status, true
}

func (e *esiWriter) Write(p []byte) (int, error) {
	if !e.wroteHead {
		e.WriteHeader(http.StatusOK)
	}
	if e.holding {
		if e.cached != nil {
			// finish uses the cached page's parsed form
			return len(p), nil
		}
		return e.body.Write(p)
	}
	return e.ResponseWriter.Write(p)
}

// noteCached tells e the cached response which is about to be written
func (e *esiWriter) noteCached(resp cache.Response) {
	e.cached = resp
}

// notePersonal tells e that the page is being served to a visitor with
// bypass cookies
func (e *esiWriter) notePersonal() {
	e.personal = true
}

// personalNoter is implemented by ResponseWriters which want to know
// whether a page is being served to a visitor with bypass cookies (see
// esiWriter)
type personalNoter interface {
	notePersonal()
}

// cachedResponseNoter is implemented by ResponseWriters which want to know
// which cached response is written to them (see esiWriter)
type cachedResponseNoter interface {
	noteCached(cache.Response)
}

// noteCached tells w which cached response is about to be written to it, if
// it wants to know
func noteCached(w http.ResponseWriter, resp cache.Response) {
	if n, ok := w.(cachedResponseNoter); ok {
		n.noteCached(resp)
	}
}

func (e *esiWriter) Flush() {
	if e.holding {
		return
	}
	if f, ok := e.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// finish writes the page, with its includes, if it was held back: gzipped
// with its text compressed once (see esiPage.gzip) for visitors which
// accept it, as compressing the whole page for every visitor costs far
// more than the rest of serving it.
func (e *esiWriter) finish() {
	if !e.holding {
		return
	}
	var page *esiPage
	if e.cached != nil {
		page, _ = cache.Derived(e.cached, func(body []byte) any { return parseESIPage(body) }).(*esiPage)
	}
	if page == nil {
		page = parseESIPage(e.body.Bytes())
	}
	contents := page.fetch(e.visitor, e.handler, e.secretHeader)
	h := e.Header()
	accept := e.visitor.Header.Get("Accept-Encoding")
	var body []byte
	coding := ""
	switch {
	case page.size(contents) < cache.DefaultCompressMinBytes:
		body = page.plain(contents)
	case utilities.PreferredEncoding(accept, "gzip") == "gzip":
		body, coding = page.gzip(contents), "gzip"
	default:
		coding, body = cache.Compress(accept, page.plain(contents))
	}
	if coding != "" {
		h.Set("Content-Encoding", coding)
		if !headerListContains(h.Values("Vary"), "Accept-Encoding") {
			h.Add("Vary", "Accept-Encoding")
		}
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	e.ResponseWriter.WriteHeader(e.status)
	e.ResponseWriter.Write(body)
}

// processESI returns body with its ESI processed for visitor: the content
// of <!--esi ... --> comments revealed, <esi:remove> elements removed, and
// <esi:include> elements replaced with what their src returns, requested
// from handler as visitor would request it.
func processESI(body []byte, visitor *http.Request, handler http.Handler, secretHeader string) []byte {
	page := parseESIPage(body)
	return page.plain(page.fetch(visitor, handler, secretHeader))
}

// include requests src (resolved against the page's URL, and only on the
// page's own site) from handler, as visitor would, and returns its body if
// it succeeds
func include(src string, visitor *http.Request, handler http.Handler, secretHeader string) ([]byte, bool) {
	if src == "" {
		return nil, false
	}
	scheme := visitor.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		scheme = "http"
	}
	page := &url.URL{Scheme: scheme, Host: visitor.Host, Path: visitor.URL.Path, RawQuery: visitor.URL.RawQuery}
	ref, err := url.Parse(src)
	if err != nil {
		return nil, false
	}
	u := page.ResolveReference(ref)
	if !strings.EqualFold(u.Host, visitor.Host) || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, false
	}
	header := visitor.Header.Clone()
	for _, name := range append(headersNotForIncludes, secretHeader) {
		header.Del(name)
	}
	header.Set("X-Forwarded-Proto", u.Scheme)
	ctx, cancel := context.WithTimeout(context.WithValue(visitor.Context(), esiDepthKey{}, esiDepth(visitor.Context())+1), includeTimeout)
	defer cancel()
	r := (&http.Request{
		Method:     http.MethodGet,
		URL:        &url.URL{Path: u.Path, RawPath: u.RawPath, RawQuery: u.RawQuery},
		RequestURI: u.RequestURI(),
		Host:       visitor.Host,
		Header:     header,
		Proto:      "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		RemoteAddr: visitor.RemoteAddr,
	}).WithContext(ctx)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		return nil, false
	}
	return w.Body.Bytes(), true
}

// withESI returns w wrapped to process the ESI of cached pages for r, before
// the proxy changes it (it must be the request the proxy handles), unless r
// is itself an include nested too deeply
func withESI(w http.ResponseWriter, r *http.Request, handler http.Handler, secretHeader string) (http.ResponseWriter, func()) {
	if esiDepth(r.Context()) >= maxESIDepth || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return w, func() {}
	}
	// The cookies (which are replaced rather than changed, so this list
	// is the visitor's), not a copy of the whole request: most pages don't
	// use ESI
	e := &esiWriter{ResponseWriter: w, request: r, host: r.Host, cookies: r.Header["Cookie"], handler: handler, secretHeader: secretHeader}
	return e, e.finish
}

// esiEligible is implemented by Cachers which can serve cached pages which
// use ESI to visitors with some bypass cookies
type esiEligible interface {
	ESIEligible(r *http.Request) bool
	ESIServes(resp cache.Response, r *http.Request) bool
}

// serveESIPage serves r - which can't be cached, because of its bypass
// cookies, but may be served pages which use ESI (see
// cache.Options.ESICookies) - the page cached for visitors without its
// cookies, if there is a fresh one which uses ESI.  Its includes are then
// fetched as the visitor (see esiWriter).  It returns false if there isn't
// one, in which case the request should go to the backend.
func serveESIPage(c cache.Cacher, w http.ResponseWriter, r *http.Request, filterQuery func(*http.Request), earlyHints bool) bool {
	anonymous := forESIPage(c, r, filterQuery)
	if anonymous == nil {
		return false
	}
	resp, found := c.Load(c.Hash(anonymous), anonymous)
	if !found || !c.(esiEligible).ESIServes(resp, r) {
		return false
	}
	if age, err := strconv.Atoi(resp.Age()); err != nil || age >= freshnessLifetime(resp) {
		return false
	}
	// The page waits for its includes
	if earlyHints && wantsEarlyHints(r) {
		sendEarlyHints(w, cache.EarlyHints(resp))
	}
	if p, ok := w.(personalNoter); ok {
		p.notePersonal()
	}
	_, responded, _ := RespondFromCache(c, w, anonymous)
	return responded
}

// forESIPage returns the request for the page r may be served instead (see
// serveESIPage): r as a visitor without its cookies would send it.  It
// returns nil if r may not be served one - including if it has query
// parameters which the cache ignores, as they may matter to a visitor with
// bypass cookies (e.g. WooCommerce's ?add-to-cart=).
func forESIPage(c cache.Cacher, r *http.Request, filterQuery func(*http.Request)) *http.Request {
	if e, ok := c.(esiEligible); !ok || !e.ESIEligible(r) {
		return nil
	}
	anonymous := r.Clone(r.Context())
	removeUnkeyed(c, anonymous)
	params := len(anonymous.URL.Query())
	filterQuery(anonymous)
	if len(anonymous.URL.Query()) != params {
		return nil
	}
	for _, name := range conditionalHeaders {
		anonymous.Header.Del(name)
	}
	anonymous.Header.Del("Cache-Control")
	anonymous.Header.Del("Pragma")
	if !c.CanCache(anonymous) {
		return nil
	}
	return anonymous
}
