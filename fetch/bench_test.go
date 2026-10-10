package fetch

import (
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/davidjwilkins/honey/cache"
)

// BenchmarkCacheHits measures serving cached pages through the whole
// handler (without the network), from many goroutines at once.
func BenchmarkCacheHits(b *testing.B) {
	for _, acceptEncoding := range []string{"gzip, deflate, br", "identity"} {
		b.Run(acceptEncoding, func(b *testing.B) {
			var page strings.Builder
			for i := 0; page.Len() < 20<<10; i++ {
				fmt.Fprintf(&page, "<article class=\"post-%d\"><p>Lorem ipsum dolor sit amet %d.</p></article>\n", i, i*7)
			}
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				w.Header().Set("Cache-Control", "max-age=3600")
				io.WriteString(w, page.String())
			}))
			defer origin.Close()
			backend, _ := url.Parse(origin.URL)
			// Without brotli, so that its background recompression of the
			// pages doesn't compete with the hits being measured
			c := cache.NewCacher(cache.Options{DisableBrotli: true})
			handler := Fetch(c, Forwarder(c), backend)
			const pages = 100
			request := func(i int) *http.Request {
				r := httptest.NewRequest(http.MethodGet, fmt.Sprintf("http://honey/page/%d", i%pages), nil)
				r.Header.Set("Accept-Encoding", acceptEncoding)
				return r
			}
			for i := 0; i < pages; i++ {
				handler(httptest.NewRecorder(), request(i))
			}
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					w := httptest.NewRecorder()
					handler(w, request(i))
					if w.Header().Get("X-Honey-Cache") != "HIT" {
						b.Fatalf("expected a cache hit, got %q", w.Header().Get("X-Honey-Cache"))
					}
					i++
				}
			})
		})
	}
}

// BenchmarkCacheMisses measures fetching and caching pages which aren't
// in the cache, including compressing them.
func BenchmarkCacheMisses(b *testing.B) {
	var page strings.Builder
	for i := 0; page.Len() < 20<<10; i++ {
		fmt.Fprintf(&page, "<article class=\"post-%d\"><p>Lorem ipsum dolor sit amet %d.</p></article>\n", i, i*7)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Cache-Control", "max-age=3600")
		io.WriteString(w, page.String())
	}))
	defer origin.Close()
	backend, _ := url.Parse(origin.URL)
	c := cache.NewCacher(cache.Options{MaxBytes: 4 << 20})
	handler := Fetch(c, Forwarder(c), backend)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			r := httptest.NewRequest(http.MethodGet, "http://honey/page/"+strconv.FormatInt(rand.Int63(), 36), nil)
			r.Header.Set("Accept-Encoding", "gzip, deflate, br")
			w := httptest.NewRecorder()
			handler(w, r)
			if w.Code != http.StatusOK {
				b.Fatalf("status %d", w.Code)
			}
		}
	})
}
