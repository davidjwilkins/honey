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

func TestSensitiveFilesAreBlocked(t *testing.T) {
	c := client(nil)
	// PHP which got into uploads, e.g. through a vulnerable plugin
	wp(t, "eval", `wp_mkdir_p(WP_CONTENT_DIR . "/uploads"); file_put_contents(WP_CONTENT_DIR . "/uploads/e2e-shell.php", "<?php echo 'SHELL' . 'RAN';");`)
	for _, p := range []string{"/wp-config.php", "/wp-config.php.bak", "/.env", "/.git/config", "/wp-content/debug.log",
		"/wp-content/uploads/e2e-shell.php", "/wp-admin/install.php", "/readme.html"} {
		resp := get(t, c, p)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode, p)
		assert.Equal(t, "BLOCKED", resp.Cache, p)
		notContains(t, resp.Body, "SHELLRAN", p)
	}
	for _, p := range []string{"/", "/wp-login.php", "/wp-includes/css/dashicons.min.css"} {
		assert.Equal(t, http.StatusOK, get(t, c, p).StatusCode, p)
	}
	contains(t, get(t, c, adminPage+"/").Body, "for PHP in uploads", "the admin page shows what was blocked")
}

func TestXMLRPCBruteForcingIsBlocked(t *testing.T) {
	post := func(body string) *http.Response {
		req, err := http.NewRequest(http.MethodPost, site+"/xmlrpc.php", strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "text/xml")
		resp, err := client(nil).Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		return resp
	}
	call := func(method, params string) string {
		return `<?xml version="1.0"?><methodCall><methodName>` + method + `</methodName><params>` + params + `</params></methodCall>`
	}
	assert.Equal(t, http.StatusOK, post(call("system.listMethods", "")).StatusCode, "XML-RPC still works")
	guess := `<value><struct><member><name>methodName</name><value><string>wp.getUsersBlogs</string></value></member>` +
		`<member><name>params</name><value><array><data><value><string>admin</string></value><value><string>guess</string></value></data></array></value></member></struct></value>`
	resp := post(call("system.multicall", `<param><value><array><data>`+strings.Repeat(guess, 50)+`</data></array></value></param>`))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "50 password guesses in one request")
	assert.Equal(t, "BLOCKED", resp.Header.Get("X-Honey-Cache"))
	resp = post(call("pingback.ping", `<param><value><string>https://victim.example/</string></value></param><param><value><string>`+site+`/</string></value></param>`))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "pingbacks")
}

func TestSecurityHeaders(t *testing.T) {
	c := client(nil)
	page := get(t, c, "/")
	assert.Equal(t, "nosniff", page.Header.Get("X-Content-Type-Options"))
	assert.NotEmpty(t, page.Header.Get("Referrer-Policy"))
	for _, name := range []string{"Server", "X-Powered-By", "X-Pingback"} {
		assert.Empty(t, page.Header.Get(name), "%s gives away what the site runs", name)
	}
	login := get(t, c, "/wp-login.php")
	assert.Equal(t, "SAMEORIGIN", login.Header.Get("X-Frame-Options"))
	assert.Contains(t, login.Header.Get("Content-Security-Policy"), "frame-ancestors 'self'")
}
