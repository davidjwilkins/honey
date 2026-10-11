package fetch

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/davidjwilkins/honey/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostOrigin responds with the Host and X-Forwarded-* headers it was sent
type hostOrigin struct {
	*origin
	mu   sync.Mutex
	seen []http.Header
}

func newHostOrigin() *hostOrigin {
	o := &hostOrigin{}
	o.origin = newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		o.mu.Lock()
		h := r.Header.Clone()
		h.Set("Host", r.Host)
		o.seen = append(o.seen, h)
		o.mu.Unlock()
		w.Header().Set("Cache-Control", "max-age=600")
		io.WriteString(w, r.Header.Get("X-Forwarded-Proto")+"://"+r.Host+r.URL.RequestURI())
	})
	return o
}

func (o *hostOrigin) last() http.Header {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.seen[len(o.seen)-1]
}

func hostProxy(t *testing.T, o *origin, opts Options) (*httptest.Server, *url.URL, cache.Cacher) {
	backend, err := url.Parse(o.URL)
	require.NoError(t, err)
	c := cache.NewCacher(cache.Options{})
	proxy := httptest.NewServer(FetchWithOptions(c, Forwarder(c), backend, opts))
	t.Cleanup(proxy.Close)
	t.Cleanup(o.Close)
	return proxy, backend, c
}

// getHost requests path from proxy, as if for host
func getHost(t *testing.T, proxy *httptest.Server, host, path string, headers ...string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, proxy.URL+path, nil)
	require.NoError(t, err)
	req.Host = host
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

func TestBackendGetsTheClientsHost(t *testing.T) {
	o := newHostOrigin()
	proxy, _, _ := hostProxy(t, o.origin, Options{})

	_, body := getHost(t, proxy, "www.example.com", "/page", "X-Forwarded-Host", "evil.example")
	assert.Equal(t, "http://www.example.com/page", body)
	assert.Equal(t, "www.example.com", o.last().Get("Host"))
	assert.Equal(t, "www.example.com", o.last().Get("X-Forwarded-Host"), "the client's X-Forwarded-Host is replaced")
}

func TestBackendHostOverride(t *testing.T) {
	o := newHostOrigin()
	proxy, _, _ := hostProxy(t, o.origin, Options{BackendHost: "origin.example.com"})

	resp, body := getHost(t, proxy, "www.example.com", "/page")
	assert.Equal(t, "http://origin.example.com/page", body)
	assert.Equal(t, "www.example.com", o.last().Get("X-Forwarded-Host"))
	resp, _ = getHost(t, proxy, "www.example.com", "/page")
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"), "it's cached as usual")
}

func TestCacheIsKeyedByHostAndScheme(t *testing.T) {
	o := newHostOrigin()
	proxy, _, _ := hostProxy(t, o.origin, Options{})

	for _, tc := range []struct{ host, proto, body string }{
		{"www.example.com", "", "http://www.example.com/"},
		{"example.com", "", "http://example.com/"},
		{"WWW.Example.com", "", "http://www.example.com/"},
		{"www.example.com", "https", "https://www.example.com/"},
		{"www.example.com", "nonsense", "http://www.example.com/"},
	} {
		_, body := getHost(t, proxy, tc.host, "/", "X-Forwarded-Proto", tc.proto)
		assert.Equal(t, tc.body, body, "%s %s", tc.host, tc.proto)
	}
	assert.Equal(t, 3, o.Hits(), "one response for each host and scheme")
}

func TestPurgeIgnoresHost(t *testing.T) {
	o := newHostOrigin()
	proxy, _, _ := hostProxy(t, o.origin, Options{Control: Control{Secret: secret}})

	for _, host := range []string{"www.example.com", "example.com"} {
		getHost(t, proxy, host, "/blog/post")
		getHost(t, proxy, host, "/blog/post", "X-Forwarded-Proto", "https")
	}
	req, err := http.NewRequest(MethodPurge, proxy.URL+"/blog/*", nil)
	require.NoError(t, err)
	req.Host = "127.0.0.1:8080" // e.g. WordPress sending it to Honey directly
	req.Header.Set(DefaultSecretHeader, secret)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, "Purged 4 responses\n", string(body))
}
