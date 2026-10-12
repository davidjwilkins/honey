//go:build e2e

package e2e

import (
	"net/http"
	"net/url"
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

func TestPasswordGuessersAreLockedOut(t *testing.T) {
	// In adminIPs, so it may log in at all
	const attacker = "198.51.100.66"
	jar := newJar(t)
	c := client(jar)
	attempt := func(password string) page {
		get(t, c, "/wp-login.php", "X-Forwarded-For", attacker) // the test cookie
		return do(t, c, http.MethodPost, "/wp-login.php", url.Values{
			"log": {"admin"}, "pwd": {password}, "testcookie": {"1"}, "wp-submit": {"Log In"},
		}, "X-Forwarded-For", attacker)
	}
	for i := 0; i < 10; i++ {
		resp := attempt("guess")
		require.Equal(t, http.StatusOK, resp.StatusCode, "WordPress shows the form again")
		contains(t, resp.Body, "is incorrect", "attempt %d", i+1)
	}
	resp := attempt("password")
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode, "locked out, even with the right password")
	assert.NotEmpty(t, resp.Header.Get("Retry-After"))
	logIn(t, "admin") // from another address

	dashboard := get(t, c, adminPage+"/")
	contains(t, dashboard.Body, `<th scope="row" class="mono">`+attacker+`</th>`, "the admin page lists the lockout")
	contains(t, dashboard.Body, "locked out "+attacker+" from logging in")
	unblock := do(t, c, http.MethodPost, adminPage+"/unblock", url.Values{"address": {attacker}}, "Origin", adminPage)
	require.Equal(t, http.StatusSeeOther, unblock.StatusCode)
	assert.Equal(t, http.StatusFound, attempt("password").StatusCode, "unblocked, it can log in")
}

func TestProbersAreBannedFromWordPress(t *testing.T) {
	const prober = "203.0.113.88"
	c := client(nil)
	cached(t, c, "/hello-world/")
	for _, p := range []string{"/.env", "/.git/config", "/wp-config.php.bak", "/backup.sql", "/vendor/phpunit/phpunit/src/Util/PHP/eval-stdin.php"} {
		assert.Equal(t, http.StatusForbidden, get(t, c, p, "X-Forwarded-For", prober).StatusCode, p)
	}
	resp := get(t, c, "/?s=probing", "X-Forwarded-For", prober)
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode, "banned from WordPress")
	contains(t, resp.Body, "security.probeLimit")
	page := get(t, c, "/hello-world/", "X-Forwarded-For", prober)
	assert.Equal(t, "HIT", page.Cache, "but still served cached pages")
	contains(t, page.Body, "Hello world")
	assert.Equal(t, http.StatusOK, get(t, c, "/?s=probing").StatusCode, "others aren't banned")

	contains(t, get(t, c, adminPage+"/").Body, "WordPress (for probing)", "the admin page lists the ban")
	unblock := do(t, c, http.MethodPost, adminPage+"/unblock", url.Values{"address": {prober}}, "Origin", adminPage)
	require.Equal(t, http.StatusSeeOther, unblock.StatusCode)
	assert.Equal(t, http.StatusOK, get(t, c, "/?s=probing", "X-Forwarded-For", prober).StatusCode)
}

func TestTheDashboardIsOnlyForAdminIPs(t *testing.T) {
	const outsider = "203.0.113.99"
	c := client(newJar(t))
	for _, p := range []string{"/wp-admin/", "/wp-login.php"} {
		resp := get(t, c, p, "X-Forwarded-For", outsider)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode, p)
		contains(t, resp.Body, "security.adminIPs", p)
	}
	ajax := do(t, c, http.MethodPost, "/wp-admin/admin-ajax.php", url.Values{"action": {"nothing"}}, "X-Forwarded-For", outsider)
	assert.NotEqual(t, http.StatusForbidden, ajax.StatusCode, "themes' and plugins' AJAX still reaches WordPress")
	postpass := do(t, c, http.MethodPost, "/wp-login.php?action=postpass", url.Values{"post_password": {"open-sesame"}},
		"X-Forwarded-For", outsider, "Referer", site+"/members-only/")
	assert.Equal(t, http.StatusFound, postpass.StatusCode, "as do posts' passwords")
	logIn(t, "admin") // from the Docker network
}

func TestUsersAreHidden(t *testing.T) {
	c := client(nil)
	for _, p := range []string{"/?author=1", "/wp-json/wp/v2/users", "/wp-json/wp/v2/users/1", "/?rest_route=/wp/v2/users", "/wp-sitemap-users-1.xml"} {
		assert.Equal(t, http.StatusNotFound, get(t, c, p).StatusCode, p)
		// A made-up logged in cookie gets past Honey, but not WordPress
		resp := get(t, c, p, "Cookie", "wordpress_logged_in_abc=admin|0|forged")
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, "%s with a forged cookie", p)
		notContains(t, resp.Body, `"slug":"admin"`, p)
	}
	notContains(t, get(t, c, "/wp-sitemap.xml").Body, "wp-sitemap-users", "the authors' sitemap is left out")
	assert.Equal(t, http.StatusOK, get(t, c, "/wp-json/wp/v2/posts").StatusCode, "the rest of the API works")

	admin := client(logIn(t, "admin"))
	resp := get(t, admin, "/?author=1")
	assert.Equal(t, http.StatusMovedPermanently, resp.StatusCode, "logged in users still get the author's archive")
	assert.Contains(t, resp.Header.Get("Location"), "/author/admin/")
}
