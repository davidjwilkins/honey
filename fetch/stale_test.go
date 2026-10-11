package fetch

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// versionedOrigin responds with how many times it has been requested,
// without caching headers (like WordPress), and fails while down is set
func versionedOrigin(down *atomic.Bool) *origin {
	return newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		if down != nil && down.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		io.WriteString(w, "v"+strconv.Itoa(hit))
	})
}

func purgeRequest(t *testing.T, proxy string, path string, soft bool) string {
	t.Helper()
	req, err := http.NewRequest(MethodPurge, proxy+path, nil)
	require.NoError(t, err)
	req.Header.Set(DefaultSecretHeader, secret)
	if soft {
		req.Header.Set(PurgeModeHeader, "soft")
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return string(body)
}

// eventually requests path until its body is want
func eventually(t *testing.T, proxy, path, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var seen []string
	for {
		resp, body := get(t, proxy+path)
		seen = append(seen, fmt.Sprintf("%s %s age=%s", resp.Header.Get("X-Honey-Cache"), body, resp.Header.Get("Age")))
		if body == want || time.Now().After(deadline) {
			assert.Equal(t, want, body, "responses: %v", seen)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestStaleWhileRevalidateSetting(t *testing.T) {
	o := versionedOrigin(nil)
	proxy := routeProxy(t, o, cache.Options{DefaultTTL: time.Second, StaleWhileRevalidate: time.Minute}, Options{})

	get(t, proxy.URL+"/")
	time.Sleep(1100 * time.Millisecond)
	resp, body := get(t, proxy.URL+"/")
	assert.Equal(t, "STALE", resp.Header.Get("X-Honey-Cache"), "nobody waits for the backend")
	assert.Equal(t, "v1", body)
	eventually(t, proxy.URL, "/", "v2")
	assert.Equal(t, 2, o.Hits(), "it was only refreshed once")
}

// softPurgeProxy proxies to o, trusting purges with the secret
func softPurgeProxy(t *testing.T, o *origin, opts cache.Options) string {
	t.Helper()
	return routeProxy(t, o, opts, Options{Control: Control{Secret: secret}}).URL
}

func TestSoftPurge(t *testing.T) {
	o := versionedOrigin(nil)
	proxy := softPurgeProxy(t, o, cache.Options{DefaultTTL: time.Hour, StaleWhileRevalidate: time.Hour})

	get(t, proxy+"/blog/post")
	resp, _ := get(t, proxy+"/blog/post")
	require.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"))

	assert.Equal(t, "Expired 1 responses\n", purgeRequest(t, proxy, "/blog/*", true))
	resp, body := get(t, proxy+"/blog/post")
	assert.Equal(t, "STALE", resp.Header.Get("X-Honey-Cache"), "the old version is served once more")
	assert.Equal(t, "v1", body)
	eventually(t, proxy, "/blog/post", "v2")
	resp, _ = get(t, proxy+"/blog/post")
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"), "the new version is fresh")

	assert.Equal(t, "Purged 1 responses\n", purgeRequest(t, proxy, "/blog/post", false))
	resp, body = get(t, proxy+"/blog/post")
	assert.Equal(t, "MISS", resp.Header.Get("X-Honey-Cache"), "a hard purge isn't served again")
	assert.Equal(t, "v3", body)
}

func TestSoftPurgeWithoutStaleWhileRevalidate(t *testing.T) {
	o := versionedOrigin(nil)
	proxy := softPurgeProxy(t, o, cache.Options{DefaultTTL: time.Hour})

	get(t, proxy+"/")
	purgeRequest(t, proxy, "/", true)
	resp, body := get(t, proxy+"/")
	assert.Equal(t, "MISS", resp.Header.Get("X-Honey-Cache"), "it's like a hard purge")
	assert.Equal(t, "v2", body)
}

func TestSoftPurgedResponsesAreServedIfTheBackendFails(t *testing.T) {
	var down atomic.Bool
	o := versionedOrigin(&down)
	proxy := softPurgeProxy(t, o, cache.Options{DefaultTTL: time.Hour, StaleIfError: time.Hour})

	get(t, proxy+"/")
	purgeRequest(t, proxy, "/", true)
	down.Store(true)
	resp, body := get(t, proxy+"/")
	assert.Equal(t, "STALE", resp.Header.Get("X-Honey-Cache"))
	assert.Equal(t, "v1", body)

	purgeRequest(t, proxy, "/", false)
	resp, _ = get(t, proxy+"/")
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode, "but hard purged ones aren't")
}
