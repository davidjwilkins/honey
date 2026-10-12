//go:build e2e

package e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tooBig is a header value larger than Apache accepts, so that WordPress's
// server answers 400 Bad Request
var tooBig = strings.Repeat("a", 16<<10)

func TestOversizedHeadersDontBreakPages(t *testing.T) {
	c := client(nil)

	_, path := newPost(t, "Not yet cached")
	bad := get(t, c, path, "X-Big", tooBig)
	require.Equal(t, http.StatusBadRequest, bad.StatusCode, "Apache should reject the header")
	assert.Equal(t, "NO-CACHE", bad.Cache)
	resp := get(t, c, path)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "one visitor's bad request mustn't be cached for everyone")
	contains(t, resp.Body, "Not yet cached")

	id, path := newPost(t, "Before the attack")
	cached(t, c, path)
	wp(t, "post", "update", id, "--post_title=After the attack")
	// The bad request gets the stale page, and its refresh fails
	bad = get(t, c, path, "X-Big", tooBig)
	assert.Equal(t, http.StatusOK, bad.StatusCode)
	assert.Equal(t, "STALE", bad.Cache)
	time.Sleep(500 * time.Millisecond)
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp := get(t, c, path)
		require.Equal(t, http.StatusOK, resp.StatusCode, "the failed refresh mustn't replace the page: %s", describe(resp))
		if strings.Contains(resp.Body, "After the attack") {
			break
		}
		require.True(t, time.Now().Before(deadline), "the page was never refreshed")
		time.Sleep(50 * time.Millisecond)
	}
}

func TestMethodOverridesDontPoisonTheAPI(t *testing.T) {
	c := client(nil)
	newPost(t, "In the API")
	for _, header := range []string{"X-HTTP-Method-Override", "X-HTTP-Method", "X-Method-Override"} {
		path := "/wp-json/wp/v2/posts?per_page=1&e2e=" + strings.ToLower(header)
		// WordPress would answer with the route's schema for OPTIONS
		poisoned := get(t, c, path, header, "OPTIONS")
		require.Equal(t, http.StatusOK, poisoned.StatusCode)
		resp := get(t, c, path)
		assert.Equal(t, "HIT", resp.Cache, header)
		assert.True(t, strings.HasPrefix(resp.Body, "["), "%s: everyone should get the list of posts, not %.100s", header, resp.Body)
	}
}

func TestOtherHostsAreTurnedAway(t *testing.T) {
	c := client(nil)
	for host, status := range map[string]int{"localhost:8080": http.StatusOK, "random-1234.example": http.StatusMisdirectedRequest} {
		req, err := http.NewRequest(http.MethodGet, site+"/", nil)
		require.NoError(t, err)
		req.Host = host
		resp, err := c.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, status, resp.StatusCode, host)
	}
}
