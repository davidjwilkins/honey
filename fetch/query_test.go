package fetch

import (
	"io"
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

// echoOrigin responds with the request URI it was sent
type echoOrigin struct {
	*origin
	mu   sync.Mutex
	seen []string
}

func newEchoOrigin() *echoOrigin {
	o := &echoOrigin{}
	o.origin = newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		o.mu.Lock()
		o.seen = append(o.seen, r.URL.RequestURI())
		o.mu.Unlock()
		w.Header().Set("Cache-Control", "max-age=60")
		io.WriteString(w, r.URL.RequestURI())
	})
	return o
}

func queryProxy(t *testing.T, o *origin, opts Options) *httptest.Server {
	backend, err := url.Parse(o.URL)
	require.NoError(t, err)
	c := cache.NewDefaultCacher()
	proxy := httptest.NewServer(FetchWithOptions(c, Forwarder(c), backend, opts))
	t.Cleanup(proxy.Close)
	t.Cleanup(o.Close)
	return proxy
}

func TestQueryParamsAllowlist(t *testing.T) {
	o := newEchoOrigin()
	proxy := queryProxy(t, o.origin, Options{QueryParams: []string{"p", "s"}})

	_, body := get(t, proxy.URL+"/?p=12&utm_source=newsletter")
	assert.Equal(t, "/?p=12", body, "unlisted parameters shouldn't reach the backend")
	resp, body := get(t, proxy.URL+"/?utm_campaign=x&p=12")
	assert.Equal(t, "/?p=12", body)
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"), "unlisted parameters shouldn't be part of the cache key")

	// cache busting with random parameters no longer reaches the backend
	for i := 0; i < 10; i++ {
		resp, body := get(t, proxy.URL+"/about?nocache="+strconv.Itoa(i))
		assert.Equal(t, "/about", body)
		if i > 0 {
			assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"))
		}
	}

	_, body = get(t, proxy.URL+"/?s=honey&p=3")
	assert.Equal(t, "/?p=3&s=honey", body)
	resp, _ = get(t, proxy.URL+"/?p=3&s=honey")
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"), "parameter order shouldn't matter")

	assert.Equal(t, []string{"/?p=12", "/about", "/?p=3&s=honey"}, o.seen)
}

func TestQueryParamsKeptByDefault(t *testing.T) {
	o := newEchoOrigin()
	proxy := queryProxy(t, o.origin, Options{})

	_, body := get(t, proxy.URL+"/?p=12&utm_source=newsletter")
	assert.Equal(t, "/?p=12&utm_source=newsletter", body)
	resp, _ := get(t, proxy.URL+"/?p=12&utm_source=other")
	assert.Equal(t, "MISS", resp.Header.Get("X-Honey-Cache"))
}

func TestQueryParamsEmptyAllowlist(t *testing.T) {
	o := newEchoOrigin()
	proxy := queryProxy(t, o.origin, Options{QueryParams: []string{}})

	_, body := get(t, proxy.URL+"/page?a=1&b=2")
	assert.Equal(t, "/page", body, "an empty list removes every parameter")
}

func TestQueryParamsLeaveUncacheableRequestsAlone(t *testing.T) {
	o := newEchoOrigin()
	proxy := queryProxy(t, o.origin, Options{QueryParams: []string{"p"}})

	// NewDefaultCacher doesn't cache WordPress previews
	resp, body := get(t, proxy.URL+"/?p=12&preview=true&nonce=abc")
	assert.Equal(t, "/?p=12&preview=true&nonce=abc", body)
	assert.Equal(t, "NO-CACHE", resp.Header.Get("X-Honey-Cache"))
}

func TestPurgeUsesQueryParamsAllowlist(t *testing.T) {
	o := newEchoOrigin()
	proxy := queryProxy(t, o.origin, Options{QueryParams: []string{"p"}, Control: Control{Secret: secret}})

	get(t, proxy.URL+"/?p=12")
	resp, body := request(t, MethodPurge, proxy.URL+"/?utm_source=x&p=12", DefaultSecretHeader, secret)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "Purged 1 responses\n", body)
}
