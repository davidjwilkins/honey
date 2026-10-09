package fetch

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func timeoutProxy(t *testing.T, o *origin, opts cache.Options, timeout time.Duration) *httptest.Server {
	backend, err := url.Parse(o.URL)
	require.NoError(t, err)
	c := cache.NewCacher(opts)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = timeout
	proxy := httptest.NewServer(Fetch(c, NewForwarder(c, transport), backend))
	t.Cleanup(proxy.Close)
	t.Cleanup(o.Close)
	return proxy
}

func TestBackendTimeout(t *testing.T) {
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	})
	proxy := timeoutProxy(t, o, cache.Options{}, 200*time.Millisecond)

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _ := get(t, proxy.URL+"/page")
			assert.Equal(t, http.StatusGatewayTimeout, resp.StatusCode)
		}()
	}
	wg.Wait()
	assert.Less(t, time.Since(start), time.Second, "requests should give up after the backend timeout")
}

func TestBackendTimeoutServesStale(t *testing.T) {
	var slow atomic.Bool
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		if slow.Load() {
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Cache-Control", "max-age=1")
		io.WriteString(w, "v"+strconv.Itoa(hit))
	})
	// WordPress doesn't send stale-if-error, so the default window applies
	proxy := timeoutProxy(t, o, cache.Options{StaleIfError: time.Minute}, 200*time.Millisecond)

	get(t, proxy.URL+"/page")
	slow.Store(true)
	time.Sleep(1100 * time.Millisecond)
	start := time.Now()
	resp, body := get(t, proxy.URL+"/page")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "v1", body)
	assert.Equal(t, "STALE", resp.Header.Get("X-Honey-Cache"))
	assert.Less(t, time.Since(start), time.Second)
}

func TestDefaultStaleIfErrorOnServerError(t *testing.T) {
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		if hit > 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Cache-Control", "max-age=1")
		io.WriteString(w, "v1")
	})
	proxy := newProxyWith(t, o, cache.Options{StaleIfError: time.Minute})

	get(t, proxy.URL+"/page")
	time.Sleep(1100 * time.Millisecond)
	resp, body := get(t, proxy.URL+"/page")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "v1", body)
	assert.Equal(t, "STALE", resp.Header.Get("X-Honey-Cache"))
}

func TestBackendsStaleIfErrorTakesPriority(t *testing.T) {
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		if hit > 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Cache-Control", "max-age=1, stale-if-error=0")
		io.WriteString(w, "v1")
	})
	proxy := newProxyWith(t, o, cache.Options{StaleIfError: time.Minute})

	get(t, proxy.URL+"/page")
	time.Sleep(1100 * time.Millisecond)
	resp, _ := get(t, proxy.URL+"/page")
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode, "the backend's own stale-if-error should be respected")
}
