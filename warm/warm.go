// Package warm fills the cache with the pages listed in a site's sitemap,
// so that visitors don't have to wait for the backend to build them - e.g.
// after Honey starts, or after everything is purged.
package warm

import (
	"context"
	"encoding/xml"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/davidjwilkins/honey/events"
)

// Defaults for Options
const (
	DefaultRate     = 2
	DefaultMaxPages = 10000
)

// maxSitemapBytes bounds the size of a sitemap (the sitemaps protocol
// allows 50MB uncompressed)
const maxSitemapBytes = 50 << 20

// Options configures a Warmer.
type Options struct {
	// Sitemap is the sitemap (or sitemap index) listing the pages to warm,
	// e.g. https://www.example.com/wp-sitemap.xml.  Only pages on its
	// scheme and host are warmed.
	Sitemap *url.URL
	// Rate is how many pages to request a second.  Defaults to
	// DefaultRate.
	Rate float64
	// MaxPages bounds how many pages are warmed.  Defaults to
	// DefaultMaxPages.
	MaxPages int
	// Interval, if set, is how often to warm the cache again, as well as
	// when Run starts and when Trigger is called.
	Interval time.Duration
	// Logger logs what is warmed.  Defaults to the standard logger.
	Logger *log.Logger
	// Events, if set, is where each warming is logged, e.g. for the admin
	// page.
	Events *events.Log
}

// Warmer requests the pages in a sitemap from a handler (the cache), so
// that they are cached.
type Warmer struct {
	handler http.Handler
	opts    Options
	trigger chan struct{}
}

// New returns a Warmer which requests pages from handler, which should be
// the caching proxy itself (without e.g. its metrics or access log, so
// that the warming isn't counted as visits).
func New(handler http.Handler, opts Options) *Warmer {
	if opts.Rate <= 0 {
		opts.Rate = DefaultRate
	}
	if opts.MaxPages <= 0 {
		opts.MaxPages = DefaultMaxPages
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	return &Warmer{handler: handler, opts: opts, trigger: make(chan struct{}, 1)}
}

// Trigger makes Run warm the cache again, once it has finished any warming
// in progress.  Calls while it is waiting to start are combined.
func (w *Warmer) Trigger() {
	select {
	case w.trigger <- struct{}{}:
	default:
	}
}

// Run warms the cache, and then again whenever Trigger is called or the
// Interval passes, until ctx is done.
func (w *Warmer) Run(ctx context.Context) {
	var tick <-chan time.Time
	if w.opts.Interval > 0 {
		ticker := time.NewTicker(w.opts.Interval)
		defer ticker.Stop()
		tick = ticker.C
	}
	for {
		w.Warm(ctx)
		select {
		case <-ctx.Done():
			return
		case <-w.trigger:
		case <-tick:
		}
	}
}

// Result says what a warming did.
type Result struct {
	// Pages is how many pages were requested, and Cached how many of those
	// were already cached (and fresh)
	Pages, Cached int
	// Errors is how many pages (or sitemaps) couldn't be fetched
	Errors int
}

// Warm requests every page in the sitemap once, at the configured rate,
// and returns what it did.  It stops early if ctx is done.
func (w *Warmer) Warm(ctx context.Context) Result {
	start := time.Now()
	var result Result
	pages, err := w.pages(ctx)
	if err != nil {
		w.opts.Logger.Printf("warming the cache: %v", err)
		result.Errors++
		if len(pages) == 0 {
			w.opts.Events.Add(events.Error, "", 0, events.Text(fmt.Sprintf("couldn't warm the cache: %v", err)))
			return result
		}
	}
	w.opts.Logger.Printf("warming the cache: %d pages from %s", len(pages), w.opts.Sitemap)
	interval := time.Duration(float64(time.Second) / w.opts.Rate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for i, page := range pages {
		if i > 0 {
			select {
			case <-ctx.Done():
				return result
			case <-ticker.C:
			}
		}
		status, header, _ := w.get(ctx, page, false)
		result.Pages++
		switch {
		case status == 0 || status >= 500:
			result.Errors++
		case header.Get("X-Honey-Cache") == "HIT":
			result.Cached++
		}
	}
	took := time.Since(start).Round(time.Millisecond)
	w.opts.Logger.Printf("warmed the cache in %s: %d pages, %d of them already cached, %d errors",
		took, result.Pages, result.Cached, result.Errors)
	kind := events.Warm
	if result.Errors > 0 {
		kind = events.Error
	}
	w.opts.Events.Add(kind, "", 0, events.Text(fmt.Sprintf("warmed %d pages from the sitemap in %s · %d already cached · %d errors",
		result.Pages, took, result.Cached, result.Errors)))
	return result
}

// pages returns the pages listed in the sitemap, and in the sitemaps it
// lists (one level deep, as WordPress's index does), up to MaxPages
func (w *Warmer) pages(ctx context.Context) ([]*url.URL, error) {
	var pages []*url.URL
	seen := map[string]bool{}
	var firstErr error
	sitemaps := []*url.URL{w.opts.Sitemap}
	for depth := 0; depth < 2 && len(sitemaps) > 0; depth++ {
		var next []*url.URL
		for _, sitemap := range sitemaps {
			status, header, body := w.getFresh(ctx, sitemap)
			if status != http.StatusOK {
				if firstErr == nil {
					firstErr = fmt.Errorf("%s: HTTP %d", sitemap, status)
					if location := header.Get("Location"); location != "" {
						// e.g. the site is https, or www
						firstErr = fmt.Errorf("%w, redirecting to %s", firstErr, location)
					}
				}
				continue
			}
			var doc struct {
				Sitemaps []string `xml:"sitemap>loc"`
				URLs     []string `xml:"url>loc"`
			}
			if err := xml.Unmarshal(body, &doc); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("%s: %w", sitemap, err)
				}
				continue
			}
			for _, loc := range doc.Sitemaps {
				if u := w.sameSite(loc); u != nil {
					next = append(next, u)
				}
			}
			for _, loc := range doc.URLs {
				u := w.sameSite(loc)
				if u == nil || seen[u.String()] {
					continue
				}
				seen[u.String()] = true
				pages = append(pages, u)
				if len(pages) >= w.opts.MaxPages {
					return pages, firstErr
				}
			}
		}
		sitemaps = next
	}
	return pages, firstErr
}

// staleRetries and staleRetryDelay bound how long getFresh waits for a
// stale response to be refreshed
var (
	staleRetries    = 10
	staleRetryDelay = 300 * time.Millisecond
)

// getFresh gets u (keeping the body), waiting for it to be refreshed if
// the cache serves it stale - e.g. after a soft purge, the sitemap must be
// the new one, or new pages would be missed.
func (w *Warmer) getFresh(ctx context.Context, u *url.URL) (int, http.Header, []byte) {
	status, header, body := w.get(ctx, u, true)
	for i := 0; i < staleRetries && header.Get("X-Honey-Cache") == "STALE"; i++ {
		select {
		case <-ctx.Done():
			return 0, http.Header{}, nil
		case <-time.After(staleRetryDelay):
		}
		status, header, body = w.get(ctx, u, true)
	}
	return status, header, body
}

// sameSite parses loc, if it is on the sitemap's scheme and host
func (w *Warmer) sameSite(loc string) *url.URL {
	u, err := url.Parse(strings.TrimSpace(loc))
	if err != nil || u.Scheme != w.opts.Sitemap.Scheme || !strings.EqualFold(u.Host, w.opts.Sitemap.Host) {
		return nil
	}
	return u
}

// get requests u from the handler, as a visitor to the site with no
// cookies, and returns the status, the headers and (if keep) the body
func (w *Warmer) get(ctx context.Context, u *url.URL, keep bool) (status int, header http.Header, body []byte) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, http.Header{}, nil
	}
	// As a server receives it: the path in the URL, and the host in Host
	req.URL = &url.URL{Path: u.Path, RawPath: u.RawPath, RawQuery: u.RawQuery}
	req.RequestURI = u.RequestURI()
	req.Host = u.Host
	req.RemoteAddr = "127.0.0.1:0"
	req.Header.Set("X-Forwarded-Proto", u.Scheme)
	req.Header.Set("User-Agent", "Honey cache warmer")
	rw := &discard{header: http.Header{}, keep: keep}
	w.handler.ServeHTTP(rw, req)
	if rw.status == 0 {
		rw.status = http.StatusOK
	}
	if ctx.Err() != nil {
		return 0, http.Header{}, nil
	}
	return rw.status, rw.header, rw.body
}

// discard is a ResponseWriter which keeps the status, the headers and
// (if keep, up to maxSitemapBytes) the body
type discard struct {
	header http.Header
	status int
	keep   bool
	body   []byte
}

func (d *discard) Header() http.Header { return d.header }

func (d *discard) WriteHeader(status int) {
	// Not informational responses (e.g. Early Hints)
	if d.status == 0 && status >= 200 {
		d.status = status
	}
}

func (d *discard) Write(p []byte) (int, error) {
	d.WriteHeader(http.StatusOK)
	if d.keep && len(d.body)+len(p) <= maxSitemapBytes {
		d.body = append(d.body, p...)
	}
	return len(p), nil
}

func (d *discard) Flush() {}
