package guard

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func from(addr, method, target string) *http.Request {
	r := httptest.NewRequest(method, "http://www.example.com"+target, nil)
	r.RemoteAddr = net.JoinHostPort(addr, "1234")
	return r
}

func TestExploitsAreBlocked(t *testing.T) {
	h := guarded(defaults)
	for _, p := range []string{"/vendor/phpunit/phpunit/src/Util/PHP/eval-stdin.php", "/wp-content/plugins/x/vendor/phpunit/phpunit/Util/PHP/eval-stdin.php",
		"/cgi-bin/luci", "/actuator/health", "/boaform/admin/formLogin", "/HNAP1/", "/wso.php", "/alfa.php"} {
		assert.Equal(t, http.StatusForbidden, serve(h, from("203.0.113.9", http.MethodGet, p)).Code, p)
	}
	for _, p := range []string{"/blog/cmd.php-tips/", "/shell/", "/category/solr-search/", "/wp-content/plugins/x/shell.php"} {
		assert.Equal(t, http.StatusOK, serve(h, from("203.0.113.9", http.MethodGet, p)).Code, p)
	}
}

func TestProbersAreBannedFromTheBackend(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)}
	log := events.New(0)
	limits := NewLimits(LimitOptions{ProbeLimit: 3, ProbeBan: time.Hour, Events: log, now: c.now})
	opts := defaults
	opts.Limits, opts.Events = limits, log
	h := guarded(opts)

	// Pages which don't exist aren't probes: anyone can mistype one
	for i := 0; i < 5; i++ {
		serve(h, from("203.0.113.9", http.MethodGet, "/no-such-page/"))
	}
	_, why := limits.AllowBackend(from("203.0.113.9", http.MethodGet, "/"))
	assert.Empty(t, why)

	serve(h, from("203.0.113.9", http.MethodGet, "/.env"))
	serve(h, from("203.0.113.9", http.MethodGet, "/wp-config.php.bak"))
	guess := xmlrpc(multicall)
	guess.RemoteAddr = "203.0.113.9:1234"
	serve(h, guess)
	wait, why := limits.AllowBackend(from("203.0.113.9", http.MethodGet, "/"))
	assert.Equal(t, "security.probeLimit", why, "banned from WordPress")
	assert.Equal(t, time.Hour, wait)
	_, why = limits.AllowBackend(from("203.0.113.10", http.MethodGet, "/"))
	assert.Empty(t, why, "other addresses aren't")
	assert.Equal(t, []Lockout{{Address: "203.0.113.9", Until: c.t.Add(time.Hour), From: BannedFromBackend}}, limits.Lockouts())
	var texts []string
	for _, e := range log.Recent(10) {
		texts = append(texts, e.Text)
	}
	assert.Contains(t, texts, "banned 203.0.113.9 from WordPress for 1h, after 3 probes for sensitive files or exploits (most recently POST /xmlrpc.php); it's still served cached pages")

	c.t = c.t.Add(time.Hour)
	_, why = limits.AllowBackend(from("203.0.113.9", http.MethodGet, "/"))
	assert.Empty(t, why, "the ban ends")

	off := NewLimits(LimitOptions{})
	off.Probed(from("203.0.113.9", http.MethodGet, "/.env"))
	assert.Empty(t, off.Lockouts())
}

func TestAdminIPs(t *testing.T) {
	_, office, _ := net.ParseCIDR("198.51.100.0/24")
	opts := defaults
	opts.AdminIPs = []*net.IPNet{office}
	h := guarded(opts)
	for _, p := range []string{"/wp-admin/", "/wp-admin/post.php?post=1&action=edit", "/WP-ADMIN/options.php", "/blog/wp-admin/", "/wp-login.php", "/wp-login.php?action=lostpassword"} {
		w := serve(h, from("203.0.113.9", http.MethodGet, p))
		assert.Equal(t, http.StatusForbidden, w.Code, p)
		assert.Contains(t, w.Body.String(), "security.adminIPs", p)
		assert.Equal(t, http.StatusOK, serve(h, from("198.51.100.7", http.MethodGet, p)).Code, "%s from the office", p)
	}
	for _, p := range []string{"/wp-admin/admin-ajax.php", "/wp-admin/admin-post.php", "/", "/wp-adminer/"} {
		assert.Equal(t, http.StatusOK, serve(h, from("203.0.113.9", http.MethodGet, p)).Code, "%s serves visitors", p)
	}
	assert.Equal(t, http.StatusOK, serve(h, from("203.0.113.9", http.MethodPost, "/wp-login.php?action=postpass")).Code, "visitors send posts' passwords to the login page")
	assert.Equal(t, http.StatusOK, serve(guarded(defaults), from("203.0.113.9", http.MethodGet, "/wp-admin/")).Code, "open unless set")
}

func TestHideUsers(t *testing.T) {
	opts := defaults
	opts.HideUsers = true
	h := guarded(opts)
	for _, p := range []string{"/?author=1", "/?author=2&x=y", "/blog/?author=12", "/wp-json/wp/v2/users", "/wp-json/wp/v2/users/1", "/WP-JSON/wp/v2/USERS",
		"/?rest_route=/wp/v2/users", "/?rest_route=/wp/v2/users/1", "/blog/wp-json/wp/v2/users", "/wp-json//wp/v2/users"} {
		w := serve(h, from("203.0.113.9", http.MethodGet, p))
		assert.Equal(t, http.StatusNotFound, w.Code, p)
		assert.Contains(t, w.Body.String(), "security.hideUsers", p)
	}
	for _, p := range []string{"/?author=admin", "/author/admin/", "/wp-json/wp/v2/posts", "/?rest_route=/wp/v2/posts", "/wp-json/wp/v2/usersettings"} {
		assert.Equal(t, http.StatusOK, serve(h, from("203.0.113.9", http.MethodGet, p)).Code, p)
	}
	r := from("203.0.113.9", http.MethodGet, "/wp-json/wp/v2/users")
	r.Header.Set("Cookie", "wordpress_logged_in_abc=admin")
	assert.Equal(t, http.StatusOK, serve(h, r).Code, "logged in users get to WordPress, which checks the cookie")
	r = from("203.0.113.9", http.MethodGet, "/wp-json/wp/v2/users")
	r.SetBasicAuth("admin", "app password")
	assert.Equal(t, http.StatusOK, serve(h, r).Code, "as do apps with an application password")
	assert.Equal(t, http.StatusOK, serve(guarded(defaults), from("203.0.113.9", http.MethodGet, "/?author=1")).Code, "off unless set")
}

func TestBansWithoutRateLimitsDontTrackEveryone(t *testing.T) {
	limits := NewLimits(LimitOptions{ProbeLimit: 3, ProbeBan: time.Hour})
	for i := 0; i < 100; i++ {
		_, why := limits.AllowBackend(from(net.IPv4(203, 0, 113, byte(i)).String(), http.MethodGet, "/"))
		require.Empty(t, why)
	}
	assert.Zero(t, limits.lru.Len())
}
