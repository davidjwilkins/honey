package fetch

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stallingHandler writes the first part of a body, flushes it, and then
// sends nothing more until released (or the request is cancelled)
func stallingHandler(w http.ResponseWriter, r *http.Request, release <-chan struct{}) {
	w.Header().Set("Cache-Control", "max-age=60")
	w.Header().Set("Content-Length", "20")
	io.WriteString(w, "first half")
	w.(http.Flusher).Flush()
	select {
	case <-release:
		io.WriteString(w, "secondhalf")
	case <-r.Context().Done():
	}
}

func TestStallTimeoutFailsStalledBody(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stallingHandler(w, r, release)
	}))
	defer backend.Close()

	client := &http.Client{Transport: StallTimeout(http.DefaultTransport, 200*time.Millisecond)}
	resp, err := client.Get(backend.URL)
	require.NoError(t, err)
	start := time.Now()
	_, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Error(t, err)
	var netErr net.Error
	assert.True(t, errors.As(err, &netErr) && netErr.Timeout(), "a stall should be reported as a timeout: %v", err)
	assert.Less(t, time.Since(start), time.Second)
}

func TestStallTimeoutIgnoresSlowReaders(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("x", 64<<10))
	}))
	defer backend.Close()

	client := &http.Client{Transport: StallTimeout(http.DefaultTransport, 100*time.Millisecond)}
	resp, err := client.Get(backend.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	total := 0
	buf := make([]byte, 16<<10)
	for {
		// a slow client, taking longer than the timeout between reads
		time.Sleep(150 * time.Millisecond)
		n, err := resp.Body.Read(buf)
		total += n
		if err == io.EOF {
			break
		}
		require.NoError(t, err, "time spent waiting for the reader shouldn't count")
	}
	assert.Equal(t, 64<<10, total)
}

func stallProxy(t *testing.T, o *origin, opts cache.Options) *httptest.Server {
	backend, err := url.Parse(o.URL)
	require.NoError(t, err)
	c := cache.NewCacher(opts)
	proxy := httptest.NewServer(Fetch(c, NewForwarder(c, StallTimeout(http.DefaultTransport, 200*time.Millisecond)), backend))
	t.Cleanup(proxy.Close)
	t.Cleanup(o.Close)
	return proxy
}

func TestStalledCacheFill(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		stallingHandler(w, r, release)
	})
	proxy := stallProxy(t, o, cache.Options{})

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, body := get(t, proxy.URL+"/page")
			assert.Equal(t, http.StatusGatewayTimeout, resp.StatusCode, "a stalled response shouldn't be served, even partly")
			assert.NotContains(t, body, "first half")
		}()
	}
	wg.Wait()
	assert.Less(t, time.Since(start), time.Second)
	assert.Equal(t, 1, o.Hits(), "requests waiting on a stalled fetch shouldn't each try the backend again")
}

func TestStalledCacheFillServesStale(t *testing.T) {
	var stall atomic.Bool
	release := make(chan struct{})
	defer close(release)
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		if stall.Load() {
			stallingHandler(w, r, release)
			return
		}
		w.Header().Set("Cache-Control", "max-age=1")
		io.WriteString(w, "v"+strconv.Itoa(hit))
	})
	proxy := stallProxy(t, o, cache.Options{StaleIfError: time.Minute})

	get(t, proxy.URL+"/page")
	stall.Store(true)
	time.Sleep(1100 * time.Millisecond)
	resp, body := get(t, proxy.URL+"/page")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "v1", body)
	assert.Equal(t, "STALE", resp.Header.Get("X-Honey-Cache"))
	assert.Contains(t, resp.Header.Get("X-Honey-Stale"), "stalled")
}

func TestStalledStreamIsCutOff(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		w.Header().Set("Content-Length", strconv.Itoa(4<<10))
		io.WriteString(w, strings.Repeat("x", 2<<10))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	// too large to cache, so it is streamed
	backend, err := url.Parse(o.URL)
	require.NoError(t, err)
	c := cache.NewCacher(cache.Options{MaxObjectBytes: 1 << 10})
	proxy := httptest.NewServer(Fetch(c, NewForwarder(c, StallTimeout(http.DefaultTransport, 200*time.Millisecond)), backend))
	t.Cleanup(proxy.Close)
	t.Cleanup(o.Close)

	start := time.Now()
	// Depending on whether the headers were flushed before the stall, the
	// client sees the connection close before or during the body
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(proxy.URL + "/video.mp4")
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	assert.Error(t, err, "a stalled stream should be cut off, not left hanging")
	assert.NotContains(t, err.Error(), "Client.Timeout", "it should be cut off by Honey, not by the client giving up")
	assert.Less(t, time.Since(start), 2*time.Second)
}
