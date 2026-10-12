package fetch

import (
	"bytes"
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/davidjwilkins/honey/cache"
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
	// visitor is the request the page is for, as the visitor sent it
	// (before e.g. its cookies are removed), so that its includes are
	// fetched as they would be for them
	visitor *http.Request
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
	e.wroteHead = true
	h := e.Header()
	if h.Get(cache.ESIHeader) == "" {
		e.ResponseWriter.WriteHeader(status)
		return
	}
	h.Del(cache.ESIHeader)
	// The page differs between visitors: its validators are the cached
	// page's, and browsers mustn't share or keep it
	for _, name := range []string{"Etag", "Last-Modified", "Content-Length", "Content-Encoding", "Content-Range"} {
		h.Del(name)
	}
	h.Set("Cache-Control", "private, no-cache")
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
		return e.body.Write(p)
	}
	return e.ResponseWriter.Write(p)
}

func (e *esiWriter) Flush() {
	if e.holding {
		return
	}
	if f, ok := e.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// finish writes the page, with its includes, if it was held back
func (e *esiWriter) finish() {
	if !e.holding {
		return
	}
	body := processESI(e.body.Bytes(), e.visitor, e.handler, e.secretHeader)
	h := e.Header()
	if coding, encoded := cache.Compress(e.visitor.Header.Get("Accept-Encoding"), body); coding != "" {
		body = encoded
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
	body = esiComment.ReplaceAll(body, []byte("$1"))
	body = esiRemove.ReplaceAll(body, nil)
	matches := esiInclude.FindAllSubmatchIndex(body, -1)
	if len(matches) == 0 {
		return body
	}
	if len(matches) > maxIncludes {
		matches = matches[:maxIncludes]
	}
	results := make([][]byte, len(matches))
	var wg sync.WaitGroup
	for i, m := range matches {
		attrs := map[string]string{}
		for _, a := range esiAttr.FindAllSubmatch(body[m[2]:m[3]], -1) {
			// e.g. src="/?p=1&amp;honey-esi=comments"
			attrs[string(a[1])] = html.UnescapeString(string(a[2]) + string(a[3]))
		}
		wg.Add(1)
		go func(i int, attrs map[string]string) {
			defer wg.Done()
			for _, src := range []string{attrs["src"], attrs["alt"]} {
				if content, ok := include(src, visitor, handler, secretHeader); ok {
					results[i] = content
					return
				}
			}
			// onerror="continue" or not, a failed include leaves a gap,
			// rather than failing the whole page
		}(i, attrs)
	}
	wg.Wait()
	var b bytes.Buffer
	last := 0
	for i, m := range matches {
		b.Write(body[last:m[0]])
		b.Write(results[i])
		last = m[1]
	}
	b.Write(body[last:])
	return b.Bytes()
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

// withESI returns w wrapped to process the ESI of cached pages for r (as
// the visitor sent it), unless r is itself an include nested too deeply
func withESI(w http.ResponseWriter, r *http.Request, handler http.Handler, secretHeader string) (http.ResponseWriter, func()) {
	if esiDepth(r.Context()) >= maxESIDepth || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return w, func() {}
	}
	visitor := r.Clone(r.Context())
	e := &esiWriter{ResponseWriter: w, visitor: visitor, handler: handler, secretHeader: secretHeader}
	return e, e.finish
}

// esiEligible is implemented by Cachers which can serve cached pages which
// use ESI to visitors with some bypass cookies
type esiEligible interface {
	ESIEligible(r *http.Request) bool
}

// serveESIPage serves r - which can't be cached, because of its bypass
// cookies, but may be served pages which use ESI (see
// cache.Options.ESICookies) - the page cached for visitors without its
// cookies, if there is a fresh one which uses ESI.  Its includes are then
// fetched as the visitor (see esiWriter).  It returns false if there isn't
// one, in which case the request should go to the backend.
func serveESIPage(c cache.Cacher, w http.ResponseWriter, r *http.Request, filterQuery func(*http.Request)) bool {
	anonymous := forESIPage(c, r, filterQuery)
	if anonymous == nil {
		return false
	}
	resp, found := c.Load(c.Hash(anonymous), anonymous)
	if !found || !cache.UsesESI(resp) {
		return false
	}
	if age, err := strconv.Atoi(resp.Age()); err != nil || age >= freshnessLifetime(resp) {
		return false
	}
	_, responded, _ := RespondFromCache(c, w, anonymous)
	return responded
}

// forESIPage returns the request for the page r may be served instead (see
// serveESIPage): r as a visitor without its cookies would send it.  It
// returns nil if r may not be served one.
func forESIPage(c cache.Cacher, r *http.Request, filterQuery func(*http.Request)) *http.Request {
	if e, ok := c.(esiEligible); !ok || !e.ESIEligible(r) {
		return nil
	}
	anonymous := r.Clone(r.Context())
	removeUnkeyed(c, anonymous)
	filterQuery(anonymous)
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
