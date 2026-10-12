package guard

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clock is a time which tests move on
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// loginSite stands in for WordPress's login form, XML-RPC and REST API:
// the right password is "right"
func loginSite(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	switch {
	case strings.HasSuffix(r.URL.Path, "xmlrpc.php"):
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "right") {
			io.WriteString(w, `<methodResponse><params><param><value><string>ok</string></value></param></params></methodResponse>`)
			return
		}
		io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>
<methodResponse>
  <fault>
    <value>
      <struct>
        <member>
          <name>faultCode</name>
          <value><int>403</int></value>
        </member>
        <member>
          <name>faultString</name>
          <value><string>Incorrect username or password.</string></value>
        </member>
      </struct>
    </value>
  </fault>
</methodResponse>`)
	case r.Header.Get("Authorization") != "":
		if _, password, _ := r.BasicAuth(); password != "right" {
			http.Error(w, `{"code":"incorrect_password"}`, http.StatusUnauthorized)
			return
		}
		io.WriteString(w, "[]")
	case r.Method == http.MethodPost && r.FormValue("pwd") == "right":
		http.SetCookie(w, &http.Cookie{Name: "wordpress_logged_in_abc", Value: "admin"})
		http.Redirect(w, r, "/wp-admin/", http.StatusFound)
	case r.Method == http.MethodPost && r.URL.Query().Get("action") == "lostpassword":
		io.WriteString(w, "Check your email")
	default:
		io.WriteString(w, `<form id="loginform">`+map[bool]string{true: "Error: the password is incorrect.", false: ""}[r.Method == http.MethodPost]+`</form>`)
	}
}

func limited(t *testing.T, opts LimitOptions) (http.Handler, *Limits, *clock, *events.Log) {
	c := &clock{t: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)}
	log := events.New(0)
	opts.now, opts.Events = c.now, log
	limits := NewLimits(opts)
	return Middleware(Options{Limits: limits, Events: log}, http.HandlerFunc(loginSite)), limits, c, log
}

func login(h http.Handler, from, password string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "http://www.example.com/wp-login.php", strings.NewReader(url.Values{"log": {"admin"}, "pwd": {password}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = net.JoinHostPort(from, "1234")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestLoginLockout(t *testing.T) {
	h, limits, clock, log := limited(t, LimitOptions{LoginAttempts: 3, LoginLockout: 15 * time.Minute})
	for i := 0; i < 2; i++ {
		assert.Equal(t, http.StatusOK, login(h, "203.0.113.9", "guess").Code)
	}
	assert.Equal(t, http.StatusFound, login(h, "203.0.113.9", "right").Code, "logging in works")
	for i := 0; i < 3; i++ {
		assert.Equal(t, http.StatusOK, login(h, "203.0.113.9", "guess").Code, "logging in again forgot the earlier failures")
	}
	w := login(h, "203.0.113.9", "right")
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "even with the right password, until the lockout ends")
	assert.Equal(t, "900", w.Header().Get("Retry-After"))
	assert.Equal(t, BlockedHeader, w.Header().Get("X-Honey-Cache"))
	assert.Contains(t, w.Body.String(), "security.loginAttempts")
	assert.Equal(t, http.StatusFound, login(h, "203.0.113.10", "right").Code, "other addresses can still log in")

	recent := log.Recent(10)
	require.Len(t, recent, 2)
	assert.Contains(t, recent[1].Text, "locked out 203.0.113.9 from logging in for 15m, after 3 failed logins (most recently POST /wp-login.php)")
	assert.Contains(t, recent[0].Text, "blocked 1 request for logins from locked out addresses")

	lockouts := limits.Lockouts()
	require.Len(t, lockouts, 1)
	assert.Equal(t, Lockout{Address: "203.0.113.9", Until: clock.t.Add(15 * time.Minute), From: LockedOutOfLogins}, lockouts[0])

	clock.t = clock.t.Add(15 * time.Minute)
	assert.Equal(t, http.StatusFound, login(h, "203.0.113.9", "right").Code, "the lockout ends")
	assert.Empty(t, limits.Lockouts())
}

func TestFailuresAreCountedOverTheLockoutTime(t *testing.T) {
	h, limits, clock, _ := limited(t, LimitOptions{LoginAttempts: 3, LoginLockout: 15 * time.Minute})
	login(h, "203.0.113.9", "guess")
	login(h, "203.0.113.9", "guess")
	clock.t = clock.t.Add(16 * time.Minute)
	login(h, "203.0.113.9", "guess")
	assert.Empty(t, limits.Lockouts(), "the first two were too long ago")
	login(h, "203.0.113.9", "guess")
	login(h, "203.0.113.9", "guess")
	assert.Len(t, limits.Lockouts(), 1)
}

func TestUnblock(t *testing.T) {
	h, limits, _, _ := limited(t, LimitOptions{LoginAttempts: 1, LoginLockout: time.Hour})
	login(h, "203.0.113.9", "guess")
	assert.False(t, limits.Unblock("203.0.113.10"))
	assert.True(t, limits.Unblock("203.0.113.9"))
	assert.Equal(t, http.StatusFound, login(h, "203.0.113.9", "right").Code)
}

func TestIPv6NetworksAreLockedOutTogether(t *testing.T) {
	h, limits, _, _ := limited(t, LimitOptions{LoginAttempts: 2, LoginLockout: time.Hour})
	login(h, "2001:db8:1:2::1", "guess")
	login(h, "2001:db8:1:2:ffff::9", "guess")
	assert.Equal(t, []Lockout{{Address: "2001:db8:1:2::/64", Until: limits.Lockouts()[0].Until, From: LockedOutOfLogins}}, limits.Lockouts(), "one computer usually has a whole /64")
	assert.Equal(t, http.StatusTooManyRequests, login(h, "2001:db8:1:2::abcd", "right").Code)
	assert.Equal(t, http.StatusFound, login(h, "2001:db8:1:3::1", "right").Code)
}

func TestAllowedAddressesArentLockedOut(t *testing.T) {
	_, office, _ := net.ParseCIDR("198.51.100.0/24")
	h, limits, _, _ := limited(t, LimitOptions{LoginAttempts: 1, LoginLockout: time.Hour, Allow: []*net.IPNet{office}})
	login(h, "198.51.100.7", "guess")
	login(h, "198.51.100.7", "guess")
	assert.Empty(t, limits.Lockouts())
	assert.Equal(t, http.StatusFound, login(h, "198.51.100.7", "right").Code)
}

func TestOtherLogins(t *testing.T) {
	h, limits, _, _ := limited(t, LimitOptions{LoginAttempts: 2, LoginLockout: time.Hour})
	send := func(r *http.Request) int {
		r.RemoteAddr = "203.0.113.9:1234"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	// Not logins
	for i := 0; i < 3; i++ {
		send(httptest.NewRequest(http.MethodPost, "/wp-login.php?action=lostpassword", strings.NewReader("user_login=admin")))
		send(httptest.NewRequest(http.MethodGet, "/wp-login.php", nil))
		send(httptest.NewRequest(http.MethodPost, "/xmlrpc.php", strings.NewReader("<methodCall><methodName>wp.getUsersBlogs</methodName><params>right</params></methodCall>")))
	}
	assert.Empty(t, limits.Lockouts())

	// XML-RPC answers a wrong password with a fault
	for i := 0; i < 2; i++ {
		send(httptest.NewRequest(http.MethodPost, "/xmlrpc.php", strings.NewReader("<methodCall><methodName>wp.getUsersBlogs</methodName><params>guess</params></methodCall>")))
	}
	assert.Len(t, limits.Lockouts(), 1)
	limits.Unblock("203.0.113.9")

	// The REST API with an application password
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodGet, "/wp-json/wp/v2/users/me", nil)
		r.SetBasicAuth("admin", "guess")
		assert.Equal(t, http.StatusUnauthorized, send(r))
	}
	r := httptest.NewRequest(http.MethodGet, "/wp-json/wp/v2/users/me", nil)
	r.SetBasicAuth("admin", "right")
	assert.Equal(t, http.StatusTooManyRequests, send(r))
	assert.Equal(t, http.StatusOK, send(httptest.NewRequest(http.MethodGet, "/", nil)), "only logging in is locked")
}

func TestAllowBackend(t *testing.T) {
	_, limits, clock, log := limited(t, LimitOptions{Rate: 2, Burst: 3})
	from := func(addr string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = addr + ":1234"
		return r
	}
	for i := 0; i < 3; i++ {
		_, why := limits.AllowBackend(from("203.0.113.9"))
		assert.Empty(t, why, "a burst")
	}
	wait, why := limits.AllowBackend(from("203.0.113.9"))
	assert.Equal(t, "security.rateLimit", why)
	assert.Equal(t, 500*time.Millisecond, wait)
	_, why = limits.AllowBackend(from("203.0.113.10"))
	assert.Empty(t, why, "each address has its own limit")

	clock.t = clock.t.Add(500 * time.Millisecond)
	_, why = limits.AllowBackend(from("203.0.113.9"))
	assert.Empty(t, why, "at the rate")
	_, why = limits.AllowBackend(from("203.0.113.9"))
	assert.NotEmpty(t, why)
	require.NotEmpty(t, log.Recent(1))
	assert.Equal(t, "rate-limited 203.0.113.9: refused 2 requests to WordPress (it may send 2 a second)", log.Recent(1)[0].Text)

	off := NewLimits(LimitOptions{})
	for i := 0; i < 100; i++ {
		_, why := off.AllowBackend(from("203.0.113.9"))
		require.Empty(t, why, "off by default")
	}
	assert.Equal(t, 20, NewLimits(LimitOptions{Rate: 2}).opts.Burst, "bursts of 10 times the rate by default")
}
