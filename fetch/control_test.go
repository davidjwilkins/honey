package fetch

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"

	"github.com/davidjwilkins/honey/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const secret = "correct-horse-battery-staple"

func newControlledProxy(t *testing.T, o *origin, control Control) *httptest.Server {
	backend, err := url.Parse(o.URL)
	require.NoError(t, err)
	c := cache.NewCacher(cache.Options{})
	proxy := httptest.NewServer(FetchWithControl(c, Forwarder(c), backend, control))
	t.Cleanup(proxy.Close)
	t.Cleanup(o.Close)
	return proxy
}

// countingOrigin responds with the path and how many times it has been
// requested, and records the methods and secret headers it was sent.
type countingOrigin struct {
	*origin
	mu      sync.Mutex
	counts  map[string]int
	methods []string
	secrets []string
}

func newCountingOrigin() *countingOrigin {
	o := &countingOrigin{counts: map[string]int{}}
	o.origin = newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		o.mu.Lock()
		o.counts[r.URL.RequestURI()]++
		count := o.counts[r.URL.RequestURI()]
		o.methods = append(o.methods, r.Method)
		o.secrets = append(o.secrets, r.Header.Get(DefaultSecretHeader))
		o.mu.Unlock()
		w.Header().Set("Cache-Control", "max-age=60")
		io.WriteString(w, r.URL.RequestURI()+" v"+strconv.Itoa(count))
	})
	return o
}

func request(t *testing.T, method, u string, headers ...string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, u, nil)
	require.NoError(t, err)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

func TestUntrustedNoCacheIsIgnored(t *testing.T) {
	o := newCountingOrigin()
	proxy := newControlledProxy(t, o.origin, Control{Secret: secret})

	get(t, proxy.URL+"/page")
	for _, headers := range [][]string{
		{"Cache-Control", "no-cache"},
		{"Pragma", "no-cache"},
		{"Cache-Control", "no-cache", "Pragma", "no-cache"},
		{"Cache-Control", "no-cache", DefaultSecretHeader, "wrong secret"},
	} {
		resp, body := get(t, proxy.URL+"/page", headers...)
		assert.Equal(t, "/page v1", body, "%v", headers)
		assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"), "%v", headers)
	}
	assert.Equal(t, 1, o.Hits(), "untrusted clients mustn't be able to send requests to the backend with no-cache")
}

func TestTrustedNoCacheRefreshes(t *testing.T) {
	cases := map[string]struct {
		control Control
		headers []string
	}{
		"secret":        {Control{Secret: secret}, []string{DefaultSecretHeader, secret}},
		"custom header": {Control{Secret: secret, SecretHeader: "X-Purge-Key"}, []string{"X-Purge-Key", secret}},
		"ip":            {Control{AllowIPs: mustParseIPs(t, "127.0.0.1")}, nil},
		"network":       {Control{AllowIPs: mustParseIPs(t, "10.0.0.0/8", "127.0.0.0/8")}, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			o := newCountingOrigin()
			proxy := newControlledProxy(t, o.origin, tc.control)

			get(t, proxy.URL+"/page")
			resp, body := get(t, proxy.URL+"/page", append([]string{"Cache-Control", "no-cache"}, tc.headers...)...)
			assert.Equal(t, "/page v2", body)
			assert.Equal(t, "MISS", resp.Header.Get("X-Honey-Cache"))
			_, body = get(t, proxy.URL+"/page")
			assert.Equal(t, "/page v2", body, "the refreshed response should be cached")
			for _, sent := range o.secrets {
				assert.Empty(t, sent, "the secret mustn't be sent to the backend")
			}
		})
	}
}

func TestUntrustedIPs(t *testing.T) {
	o := newCountingOrigin()
	proxy := newControlledProxy(t, o.origin, Control{AllowIPs: mustParseIPs(t, "10.0.0.0/8", "::1")})

	get(t, proxy.URL+"/page")
	resp, _ := get(t, proxy.URL+"/page", "Cache-Control", "no-cache")
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"), "127.0.0.1 isn't in the allowed networks")
	resp, _ = request(t, MethodPurge, proxy.URL+"/page")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestPurge(t *testing.T) {
	o := newCountingOrigin()
	proxy := newControlledProxy(t, o.origin, Control{Secret: secret})
	paths := []string{"/page", "/page2", "/blog/a", "/blog/b?p=2", "/about"}
	for _, path := range paths {
		get(t, proxy.URL+path)
	}
	version := func(path string) string {
		_, body := get(t, proxy.URL+path)
		return body
	}

	resp, body := request(t, MethodPurge, proxy.URL+"/page")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "untrusted requests can't purge")
	assert.Equal(t, "/page v1", version("/page"))

	resp, body = request(t, MethodPurge, proxy.URL+"/page", DefaultSecretHeader, secret)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "Purged 1 responses\n", body)
	assert.Equal(t, "/page v2", version("/page"))
	assert.Equal(t, "/page2 v1", version("/page2"))

	resp, body = request(t, MethodPurge, proxy.URL+"/blog/*", DefaultSecretHeader, secret)
	assert.Equal(t, "Purged 2 responses\n", body)
	assert.Equal(t, "/blog/a v2", version("/blog/a"))
	assert.Equal(t, "/blog/b?p=2 v2", version("/blog/b?p=2"))
	assert.Equal(t, "/about v1", version("/about"))

	resp, body = request(t, MethodPurge, proxy.URL+"/*", DefaultSecretHeader, secret)
	assert.Equal(t, "Purged 5 responses\n", body)
	assert.Equal(t, "/about v2", version("/about"))

	for _, method := range o.methods {
		assert.Equal(t, http.MethodGet, method, "PURGE requests mustn't be sent to the backend")
	}
}

func TestDefaultControlTrustsNobody(t *testing.T) {
	o := newCountingOrigin()
	backend, _ := url.Parse(o.URL)
	c := cache.NewCacher(cache.Options{})
	proxy := httptest.NewServer(Fetch(c, Forwarder(c), backend))
	t.Cleanup(proxy.Close)
	t.Cleanup(o.Close)

	get(t, proxy.URL+"/page")
	resp, _ := get(t, proxy.URL+"/page", "Cache-Control", "no-cache", DefaultSecretHeader, "")
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"))
	resp, _ = request(t, MethodPurge, proxy.URL+"/page")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestParseIPs(t *testing.T) {
	networks, err := ParseIPs([]string{"127.0.0.1", "::1", "10.0.0.0/8", "2001:db8::/32"})
	require.NoError(t, err)
	assert.Equal(t, []string{"127.0.0.1/32", "::1/128", "10.0.0.0/8", "2001:db8::/32"}, func() []string {
		var s []string
		for _, n := range networks {
			s = append(s, n.String())
		}
		return s
	}())
	for _, bad := range []string{"localhost", "10.0.0.0/33", "300.1.1.1", ""} {
		_, err := ParseIPs([]string{bad})
		assert.Error(t, err, bad)
	}
}

func TestWithoutDirective(t *testing.T) {
	assert.Equal(t, "max-age=0", withoutDirective("no-cache, max-age=0", "no-cache"))
	assert.Equal(t, "", withoutDirective("No-Cache", "no-cache"))
	assert.Equal(t, `max-age=0, only-if-cached`, withoutDirective(`max-age=0,no-cache="x", only-if-cached`, "no-cache"))
}

func mustParseIPs(t *testing.T, addresses ...string) []*net.IPNet {
	networks, err := ParseIPs(addresses)
	require.NoError(t, err)
	return networks
}
