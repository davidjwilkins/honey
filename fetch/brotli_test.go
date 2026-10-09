package fetch

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/davidjwilkins/honey/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// page is compressible HTML, large enough to be worth compressing
var page = func() string {
	var b strings.Builder
	for i := 0; b.Len() < 20<<10; i++ {
		fmt.Fprintf(&b, "<article class=\"post-%d\"><h2><a href=\"/2018/02/post-%d/\">Post %d</a></h2><p>Lorem ipsum dolor sit amet %d.</p></article>\n", i, i, i, i*7)
	}
	return b.String()
}()

// getRaw makes a request without the client decoding the response
func getRaw(t *testing.T, u string, headers ...string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, u, nil)
	require.NoError(t, err)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	return resp, body
}

func decodeBrotli(t *testing.T, body []byte) string {
	t.Helper()
	decoded, err := io.ReadAll(brotli.NewReader(strings.NewReader(string(body))))
	require.NoError(t, err)
	return string(decoded)
}

func decodeGzip(t *testing.T, body []byte) string {
	t.Helper()
	r, err := gzip.NewReader(strings.NewReader(string(body)))
	require.NoError(t, err)
	decoded, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(decoded)
}

func htmlOrigin(contentType string, body string) *origin {
	return newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("Content-Type", contentType)
		io.WriteString(w, body)
	})
}

func TestBrotliServedToClientsWhichAcceptIt(t *testing.T) {
	var acceptEncoding []string
	var mu sync.Mutex
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		mu.Lock()
		acceptEncoding = append(acceptEncoding, r.Header.Get("Accept-Encoding"))
		mu.Unlock()
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, page)
	})
	proxy := newProxy(t, o)

	resp, body := getRaw(t, proxy.URL+"/page", "Accept-Encoding", "gzip, deflate, br")
	assert.Equal(t, "MISS", resp.Header.Get("X-Honey-Cache"))
	assert.Equal(t, "br", resp.Header.Get("Content-Encoding"), "the requester which filled the cache should get brotli too")
	assert.Less(t, len(body), len(page)/4)
	assert.Equal(t, page, decodeBrotli(t, body))
	assert.Contains(t, resp.Header.Values("Vary"), "Accept-Encoding")
	brEtag := resp.Header.Get("Etag")
	assert.True(t, strings.HasSuffix(brEtag, `-br"`), brEtag)

	resp, body = getRaw(t, proxy.URL+"/page", "Accept-Encoding", "br")
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"))
	assert.Equal(t, "br", resp.Header.Get("Content-Encoding"))
	assert.Equal(t, fmt.Sprint(len(body)), resp.Header.Get("Content-Length"))
	assert.Equal(t, page, decodeBrotli(t, body))

	resp, body = getRaw(t, proxy.URL+"/page", "Accept-Encoding", "gzip")
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"), "clients which don't accept brotli share the same cache entry")
	assert.Equal(t, "gzip", resp.Header.Get("Content-Encoding"))
	assert.Equal(t, page, decodeGzip(t, body))
	gzipEtag := resp.Header.Get("Etag")
	assert.True(t, strings.HasSuffix(gzipEtag, `-gzip"`), gzipEtag)

	resp, body = getRaw(t, proxy.URL+"/page", "Accept-Encoding", "identity")
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"))
	assert.Equal(t, "", resp.Header.Get("Content-Encoding"))
	assert.Equal(t, page, string(body))
	assert.NotContains(t, []string{brEtag, gzipEtag}, resp.Header.Get("Etag"), "each encoding needs its own Etag")

	resp, _ = getRaw(t, proxy.URL+"/page", "Accept-Encoding", "br", "If-None-Match", brEtag)
	assert.Equal(t, http.StatusNotModified, resp.StatusCode)

	assert.Equal(t, 1, o.Hits())
	assert.Equal(t, []string{"gzip"}, acceptEncoding, "the backend should only be asked for gzip, which Go decodes")
}

func TestBrotliMultiplexedRequestsWithDifferentEncodings(t *testing.T) {
	release := make(chan struct{})
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		<-release
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Vary", "Accept-Encoding")
		io.WriteString(w, page)
	})
	proxy := newProxy(t, o)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(br bool) {
			defer wg.Done()
			if br {
				resp, body := getRaw(t, proxy.URL+"/page", "Accept-Encoding", "gzip, br")
				assert.Equal(t, "br", resp.Header.Get("Content-Encoding"))
				assert.Equal(t, page, decodeBrotli(t, body))
			} else {
				resp, body := getRaw(t, proxy.URL+"/page", "Accept-Encoding", "identity")
				assert.Equal(t, "", resp.Header.Get("Content-Encoding"))
				assert.Equal(t, page, string(body))
			}
		}(i%2 == 0)
	}
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
	assert.Equal(t, 1, o.Hits(), "requests shouldn't be split up by Accept-Encoding")
}

func TestBrotliFromGzippingBackend(t *testing.T) {
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Vary", "Accept-Encoding")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			io.WriteString(gz, page)
			gz.Close()
			return
		}
		io.WriteString(w, page)
	})
	proxy := newProxy(t, o)

	_, body := getRaw(t, proxy.URL+"/page", "Accept-Encoding", "br")
	assert.Equal(t, page, decodeBrotli(t, body))
	resp, body := getRaw(t, proxy.URL+"/page")
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"))
	assert.Equal(t, page, string(body), "the cache should hold the decoded body")
	assert.Equal(t, 1, o.Hits())
}

func TestBrotliNotUsedForUnsuitableResponses(t *testing.T) {
	cases := map[string]*origin{
		"image":   htmlOrigin("image/png", page),
		"small":   htmlOrigin("text/html", "<p>too small to bother</p>"),
		"no type": htmlOrigin("", page),
		"no-transform": newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
			w.Header().Set("Cache-Control", "max-age=60, no-transform")
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, page)
		}),
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			proxy := newProxy(t, o)
			for i := 0; i < 2; i++ {
				resp, _ := getRaw(t, proxy.URL+"/page", "Accept-Encoding", "br")
				assert.Equal(t, "", resp.Header.Get("Content-Encoding"))
				assert.NotContains(t, resp.Header.Values("Vary"), "Accept-Encoding")
			}
		})
	}
}

func TestBrotliNotUsedForRanges(t *testing.T) {
	o := htmlOrigin("text/html", page)
	proxy := newProxy(t, o)

	getRaw(t, proxy.URL+"/page", "Accept-Encoding", "br")
	resp, body := getRaw(t, proxy.URL+"/page", "Accept-Encoding", "br", "Range", "bytes=0-8")
	assert.Equal(t, http.StatusPartialContent, resp.StatusCode)
	assert.Equal(t, "", resp.Header.Get("Content-Encoding"))
	assert.Equal(t, page[:9], string(body), "ranges apply to the unencoded body")
}

func TestBackendEncodedResponsesArentCached(t *testing.T) {
	// A backend which brotli encodes even though it wasn't asked to
	encoded := compressForTest(page)
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		w.Header().Set("Cache-Control", "max-age=60")
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Encoding", "br")
		w.Write(encoded)
	})
	proxy := newProxy(t, o)

	for i := 0; i < 2; i++ {
		resp, body := getRaw(t, proxy.URL+"/page", "Accept-Encoding", "br")
		assert.Equal(t, "br", resp.Header.Get("Content-Encoding"))
		assert.Equal(t, page, decodeBrotli(t, body))
	}
	assert.Equal(t, 2, o.Hits(), "a response the cache can't decode shouldn't be cached for everyone")
}

func compressForTest(s string) []byte {
	var b strings.Builder
	w := brotli.NewWriter(&b)
	io.WriteString(w, s)
	w.Close()
	return []byte(b.String())
}

func TestGzipFallback(t *testing.T) {
	o := htmlOrigin("text/html", page)
	proxy := newProxy(t, o)

	resp, body := getRaw(t, proxy.URL+"/page", "Accept-Encoding", "gzip, deflate")
	assert.Equal(t, "MISS", resp.Header.Get("X-Honey-Cache"))
	assert.Equal(t, "gzip", resp.Header.Get("Content-Encoding"), "the requester which filled the cache should get gzip too")
	assert.Equal(t, page, decodeGzip(t, body))
	assert.Contains(t, resp.Header.Values("Vary"), "Accept-Encoding")

	etag := resp.Header.Get("Etag")
	resp, _ = getRaw(t, proxy.URL+"/page", "Accept-Encoding", "gzip", "If-None-Match", etag)
	assert.Equal(t, http.StatusNotModified, resp.StatusCode)

	resp, _ = getRaw(t, proxy.URL+"/page", "Accept-Encoding", "br;q=0.5, gzip")
	assert.Equal(t, "gzip", resp.Header.Get("Content-Encoding"), "the client's q-values decide")
	resp, _ = getRaw(t, proxy.URL+"/page", "Accept-Encoding", "br, gzip")
	assert.Equal(t, "br", resp.Header.Get("Content-Encoding"), "brotli is preferred when the client has no preference")
	assert.Equal(t, 1, o.Hits())
}

func TestGzipWithoutBrotli(t *testing.T) {
	o := htmlOrigin("text/html", page)
	proxy := newProxyWith(t, o, cache.Options{DisableBrotli: true})

	resp, body := getRaw(t, proxy.URL+"/page", "Accept-Encoding", "gzip, br")
	assert.Equal(t, "gzip", resp.Header.Get("Content-Encoding"), "with brotli off, gzip is still used")
	assert.Equal(t, page, decodeGzip(t, body))
}

func TestCompressionDisabled(t *testing.T) {
	o := htmlOrigin("text/html", page)
	proxy := newProxyWith(t, o, cache.Options{DisableBrotli: true, DisableGzip: true})

	resp, body := getRaw(t, proxy.URL+"/page", "Accept-Encoding", "gzip, br")
	assert.Equal(t, "", resp.Header.Get("Content-Encoding"))
	assert.Equal(t, page, string(body))
}
