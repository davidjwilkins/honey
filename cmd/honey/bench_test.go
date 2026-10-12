package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/davidjwilkins/honey/config"
)

// benchPage is a WordPress page of a typical size (about 40KB), with the
// stylesheets and scripts in its head that a block theme has
var benchPage = `<!DOCTYPE html><html lang="en-US"><head><meta charset="UTF-8" />
<link rel='stylesheet' id='wp-block-library-css' href='/wp-includes/css/dist/block-library/style.min.css?ver=7.0' media='all' />
<link rel='stylesheet' href='/wp-content/themes/twentytwentyfive/style.css?ver=1.3' media='all' />
<script src="/wp-includes/js/jquery/jquery.min.js?ver=3.7.1" id="jquery-core-js"></script>
<title>Hello world! – Honey</title></head><body class="single">` +
	strings.Repeat(`<p class="wp-block-paragraph">Welcome to WordPress. This is your first post. Edit or delete it, then start writing! The quick brown fox jumps over the lazy dog.</p>`+"\n", 280) +
	`</body></html>`

// discard is a ResponseWriter which throws the response away, so that the
// benchmarks measure Honey rather than recording responses
type discard struct {
	header http.Header
	status int
	bytes  int
}

func (d *discard) Header() http.Header         { return d.header }
func (d *discard) WriteHeader(status int)      { d.status = status }
func (d *discard) Write(p []byte) (int, error) { d.bytes += len(p); return len(p), nil }

// browserHeaders are what Chrome sends for a page, with analytics cookies
var browserHeaders = http.Header{
	"Accept":                    {"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"},
	"Accept-Encoding":           {"gzip, deflate, br, zstd"},
	"Accept-Language":           {"en-GB,en;q=0.9"},
	"Cookie":                    {"_ga=GA1.1.1234567890.1700000000; _ga_ABC=GS1.1.1700000000.1.1.1700000000.0.0.0"},
	"User-Agent":                {"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"},
	"Sec-Fetch-Mode":            {"navigate"},
	"Sec-Fetch-Dest":            {"document"},
	"Upgrade-Insecure-Requests": {"1"},
}

func benchHandler(b *testing.B, settings string) http.Handler {
	b.Helper()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		io.WriteString(w, benchPage)
	}))
	b.Cleanup(origin.Close)
	cfg, err := config.Parse(`
hosts = ["www.example.com"]
[backend]
uri = "` + origin.URL + `"
[cache]
defaultTTL = "1h"
` + settings + `
[admin]
listen = "127.0.0.1:0"
`)
	if err != nil {
		b.Fatal(err)
	}
	h := newHandler(cfg)
	// Cache the page, and its compressed copies
	for _, encoding := range []string{"", "br", "gzip"} {
		r := httptest.NewRequest(http.MethodGet, "http://www.example.com/hello-world/", nil)
		r.Header.Set("Accept-Encoding", encoding)
		w := &discard{header: http.Header{}}
		h.ServeHTTP(w, r)
	}
	return h
}

func benchHits(b *testing.B, h http.Handler, header http.Header) {
	b.Helper()
	r := httptest.NewRequest(http.MethodGet, "http://www.example.com/hello-world/", nil)
	r.RemoteAddr = "203.0.113.9:51234"
	w := &discard{header: http.Header{}}
	h.ServeHTTP(w, withHeader(r, header))
	if w.status != http.StatusOK || w.header.Get("X-Honey-Cache") != "HIT" {
		b.Fatalf("not a cache hit: %d %s", w.status, w.header.Get("X-Honey-Cache"))
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		w := &discard{header: http.Header{}}
		for pb.Next() {
			clear(w.header)
			h.ServeHTTP(w, withHeader(r, header))
		}
	})
}

// withHeader returns a copy of r, as a server would receive it, with header
func withHeader(r *http.Request, header http.Header) *http.Request {
	c := *r
	c.Header = header.Clone()
	return &c
}

func BenchmarkHit(b *testing.B) {
	b.Run("browser", func(b *testing.B) {
		benchHits(b, benchHandler(b, ""), browserHeaders)
	})
	b.Run("no-compression", func(b *testing.B) {
		header := browserHeaders.Clone()
		header.Del("Accept-Encoding")
		benchHits(b, benchHandler(b, ""), header)
	})
	b.Run("bare", func(b *testing.B) {
		benchHits(b, benchHandler(b, ""), http.Header{})
	})
	b.Run("security-off", func(b *testing.B) {
		benchHits(b, benchHandler(b, "[security]\nblockPaths = false\nxmlrpc = \"on\"\nheaders = false\nloginAttempts = 0\nprobeLimit = 0\n"), browserHeaders)
	})
}
