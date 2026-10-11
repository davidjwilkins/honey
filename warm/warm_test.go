package warm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// site is a handler serving a WordPress-like sitemap index, and recording
// the requests it gets
type site struct {
	mu       sync.Mutex
	requests []string
	cached   map[string]bool
}

func (s *site) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	page := r.Header.Get("X-Forwarded-Proto") + "://" + r.Host + r.URL.RequestURI()
	s.requests = append(s.requests, page)
	if s.cached[page] {
		w.Header().Set("X-Honey-Cache", "HIT")
	}
	s.cached[page] = true
	s.mu.Unlock()
	switch r.URL.Path {
	case "/wp-sitemap.xml":
		io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>
<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <sitemap><loc>https://www.example.com/wp-sitemap-posts-post-1.xml</loc></sitemap>
  <sitemap><loc>https://www.example.com/wp-sitemap-posts-page-1.xml</loc></sitemap>
  <sitemap><loc>https://evil.example/sitemap.xml</loc></sitemap>
</sitemapindex>`)
	case "/wp-sitemap-posts-post-1.xml":
		io.WriteString(w, `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>https://www.example.com/</loc></url>
  <url><loc>https://www.example.com/hello-world/</loc></url>
  <url><loc>https://www.example.com/?p=2</loc></url>
  <url><loc>http://www.example.com/insecure/</loc></url>
  <url><loc>https://other.example/elsewhere/</loc></url>
</urlset>`)
	case "/wp-sitemap-posts-page-1.xml":
		io.WriteString(w, `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>https://www.example.com/about/</loc></url>
  <url><loc>https://www.example.com/hello-world/</loc></url>
</urlset>`)
	default:
		if r.Header.Get("Cookie") != "" {
			w.WriteHeader(http.StatusBadRequest)
		}
	}
}

func (s *site) pages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var pages []string
	for _, r := range s.requests {
		if !bytes.Contains([]byte(r), []byte("sitemap")) {
			pages = append(pages, r)
		}
	}
	return pages
}

func newWarmer(t *testing.T, s *site, opts Options) *Warmer {
	t.Helper()
	sitemap, err := url.Parse("https://www.example.com/wp-sitemap.xml")
	require.NoError(t, err)
	opts.Sitemap = sitemap
	if opts.Rate == 0 {
		opts.Rate = 1000
	}
	opts.Logger = log.New(io.Discard, "", 0)
	return New(s, opts)
}

func TestWarm(t *testing.T) {
	s := &site{cached: map[string]bool{}}
	w := newWarmer(t, s, Options{})
	result := w.Warm(context.Background())
	assert.Equal(t, []string{
		"https://www.example.com/",
		"https://www.example.com/hello-world/",
		"https://www.example.com/?p=2",
		"https://www.example.com/about/",
	}, s.pages(), "each page on the site once, as a visitor would ask for it")
	assert.Equal(t, Result{Pages: 4}, result)

	result = w.Warm(context.Background())
	assert.Equal(t, Result{Pages: 4, Cached: 4}, result)
}

func TestMaxPages(t *testing.T) {
	s := &site{cached: map[string]bool{}}
	result := newWarmer(t, s, Options{MaxPages: 2}).Warm(context.Background())
	assert.Equal(t, 2, result.Pages)
	assert.Len(t, s.pages(), 2)
}

func TestRate(t *testing.T) {
	s := &site{cached: map[string]bool{}}
	start := time.Now()
	newWarmer(t, s, Options{Rate: 20}).Warm(context.Background())
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 150*time.Millisecond, "4 pages at 20 a second take at least 3 intervals")
	assert.Less(t, elapsed, time.Second)
}

func TestMissingSitemap(t *testing.T) {
	sitemap, _ := url.Parse("https://www.example.com/nope.xml")
	var logs bytes.Buffer
	w := New(http.NotFoundHandler(), Options{Sitemap: sitemap, Logger: log.New(&logs, "", 0)})
	assert.Equal(t, Result{Errors: 1}, w.Warm(context.Background()))
	assert.Contains(t, logs.String(), "https://www.example.com/nope.xml: HTTP 404")
}

func TestRedirectedSitemap(t *testing.T) {
	sitemap, _ := url.Parse("http://www.example.com/wp-sitemap.xml")
	var logs bytes.Buffer
	w := New(http.RedirectHandler("https://www.example.com/wp-sitemap.xml", http.StatusMovedPermanently),
		Options{Sitemap: sitemap, Logger: log.New(&logs, "", 0)})
	w.Warm(context.Background())
	assert.Contains(t, logs.String(), "HTTP 301, redirecting to https://www.example.com/wp-sitemap.xml")
}

func TestStopsWhenCancelled(t *testing.T) {
	s := &site{cached: map[string]bool{}}
	ctx, cancel := context.WithCancel(context.Background())
	w := newWarmer(t, s, Options{Rate: 5})
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	result := w.Warm(ctx)
	assert.Less(t, result.Pages, 4)
}

func TestRunAndTrigger(t *testing.T) {
	s := &site{cached: map[string]bool{}}
	w := newWarmer(t, s, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	waitFor := func(n int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for len(s.pages()) < n && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		require.GreaterOrEqual(t, len(s.pages()), n)
	}
	waitFor(4)
	// However they fall around the end of the first run, these make one
	// or two more runs, and no more
	w.Trigger()
	w.Trigger()
	w.Trigger()
	waitFor(8)
	time.Sleep(100 * time.Millisecond)
	pages := len(s.pages())
	assert.Contains(t, []int{8, 12}, pages)
}

func TestInterval(t *testing.T) {
	s := &site{cached: map[string]bool{}}
	w := newWarmer(t, s, Options{Interval: 50 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Millisecond)
	defer cancel()
	w.Run(ctx)
	runs := len(s.pages()) / 4
	assert.GreaterOrEqual(t, runs, 3, fmt.Sprint(runs))
}
