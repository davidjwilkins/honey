package admin

import (
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

const testPassword = "correct horse battery staple"

type testClock struct{ t time.Time }

func (c *testClock) now() time.Time { return c.t }

func loginAdmin(t *testing.T) (http.Handler, *testClock, *events.Log) {
	t.Helper()
	hash, err := HashPassword(testPassword)
	require.NoError(t, err)
	clock := &testClock{t: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)}
	log := events.New(0)
	return Handler(Options{Started: clock.t, PasswordHash: hash, Host: "honey.example.com", Events: log, now: clock.now}), clock, log
}

// request sends a request for path on host (as if over HTTPS, unless it's
// an IP address), with the session cookie, if there is one
func request(h http.Handler, method, host, path, session string, form url.Values) *httptest.ResponseRecorder {
	var r *http.Request
	if form != nil {
		r = httptest.NewRequest(method, "http://"+host+path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "https://"+host)
	} else {
		r = httptest.NewRequest(method, "http://"+host+path, nil)
	}
	if strings.HasPrefix(host, "127.") {
		r.RemoteAddr = "127.0.0.1:5555"
	} else {
		r.Header.Set("X-Forwarded-Proto", "https")
		r.RemoteAddr = "203.0.113.9:1234"
	}
	r.Header.Set("Accept", "text/html")
	if session != "" {
		r.AddCookie(&http.Cookie{Name: SessionCookie, Value: session})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func sessionFrom(w *httptest.ResponseRecorder) string {
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookie {
			return c.Value
		}
	}
	return ""
}

func TestLogin(t *testing.T) {
	h, clock, log := loginAdmin(t)

	w := request(h, http.MethodGet, "honey.example.com", "/cache?path=/blog/", "", nil)
	assert.Equal(t, http.StatusSeeOther, w.Code, "not logged in")
	assert.Equal(t, "/login?next=%2Fcache%3Fpath%3D%2Fblog%2F", w.Header().Get("Location"))
	w = request(h, http.MethodGet, "honey.example.com", "/login?next=%2Fcache", "", nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `name="password"`)
	assert.Equal(t, http.StatusOK, request(h, http.MethodGet, "honey.example.com", "/admin.css", "", nil).Code, "the login page's styles")
	assert.Equal(t, http.StatusSeeOther, request(h, http.MethodGet, "honey.example.com", "/metrics", "", nil).Code, "the metrics aren't public")
	assert.Equal(t, http.StatusUnauthorized, request(h, http.MethodPost, "honey.example.com", "/purge", "", url.Values{"all": {"1"}}).Code)

	w = request(h, http.MethodPost, "honey.example.com", "/login", "", url.Values{"password": {"guess"}, "next": {"/cache"}})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "That isn&#39;t the password.")
	assert.Empty(t, sessionFrom(w))
	assert.Contains(t, log.Recent(1)[0].Text, "wrong password for the admin page from 203.0.113.9")

	w = request(h, http.MethodPost, "honey.example.com", "/login", "", url.Values{"password": {testPassword}, "next": {"/cache"}})
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/cache", w.Header().Get("Location"))
	cookie := w.Result().Cookies()[0]
	assert.True(t, cookie.HttpOnly)
	assert.True(t, cookie.Secure, "over HTTPS")
	assert.Equal(t, http.SameSiteStrictMode, cookie.SameSite)
	session := cookie.Value
	assert.Len(t, session, 64)

	w = request(h, http.MethodGet, "honey.example.com", "/", session, nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `action="/logout"`)

	// Sessions end when they aren't used
	clock.t = clock.t.Add(13 * time.Hour)
	assert.Equal(t, http.StatusSeeOther, request(h, http.MethodGet, "honey.example.com", "/", session, nil).Code)

	// and when logging out
	w = request(h, http.MethodPost, "honey.example.com", "/login", "", url.Values{"password": {testPassword}})
	session = sessionFrom(w)
	require.NotEmpty(t, session)
	assert.Equal(t, http.StatusSeeOther, request(h, http.MethodPost, "honey.example.com", "/logout", session, url.Values{}).Code)
	assert.Equal(t, http.StatusSeeOther, request(h, http.MethodGet, "honey.example.com", "/", session, nil).Code)

	// Other hosts still aren't served
	assert.Equal(t, http.StatusMisdirectedRequest, request(h, http.MethodGet, "evil.example.net", "/login", "", nil).Code)
}

func TestLoginLockout(t *testing.T) {
	h, clock, _ := loginAdmin(t)
	for i := 0; i < loginAttempts; i++ {
		request(h, http.MethodPost, "honey.example.com", "/login", "", url.Values{"password": {"guess"}})
	}
	w := request(h, http.MethodPost, "honey.example.com", "/login", "", url.Values{"password": {testPassword}})
	assert.Equal(t, http.StatusTooManyRequests, w.Code, "even with the right password")
	assert.Contains(t, w.Body.String(), "Too many wrong passwords")
	clock.t = clock.t.Add(loginLockout)
	assert.Equal(t, http.StatusSeeOther, request(h, http.MethodPost, "honey.example.com", "/login", "", url.Values{"password": {testPassword}}).Code)
}

func TestLoginNeedsHTTPS(t *testing.T) {
	h, _, _ := loginAdmin(t)
	r := httptest.NewRequest(http.MethodPost, "http://honey.example.com/login", strings.NewReader(url.Values{"password": {testPassword}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://honey.example.com")
	r.RemoteAddr = "203.0.113.9:1234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Contains(t, w.Body.String(), "isn't served over HTTPS")

	// but from this host (e.g. through an SSH tunnel) is fine
	w = request(h, http.MethodPost, "127.0.0.1:9090", "/login", "", url.Values{"password": {testPassword}})
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.False(t, w.Result().Cookies()[0].Secure)
}

func TestLocalNext(t *testing.T) {
	for next, want := range map[string]string{
		"/cache?path=/blog/":    "/cache?path=/blog/",
		"":                      "/",
		"https://evil.example/": "/",
		"//evil.example/":       "/",
		"/\\evil.example":       "/",
		"/login?next=/login":    "/",
		"javascript:alert(1)":   "/",
		"/settings#cdn":         "/settings",
	} {
		assert.Equal(t, want, localNext(next), next)
	}
}

func TestPasswordHashes(t *testing.T) {
	_, err := HashPassword("short")
	assert.Error(t, err)
	hash, err := HashPassword(testPassword)
	require.NoError(t, err)
	assert.NoError(t, CheckPasswordHash(hash))
	assert.Error(t, CheckPasswordHash("plain text"))
}

func TestWithoutLoginNothingChanges(t *testing.T) {
	h := Handler(Options{Started: time.Now()})
	assert.Equal(t, http.StatusOK, request(h, http.MethodGet, "127.0.0.1:9090", "/", "", nil).Code)
	assert.Equal(t, http.StatusNotFound, request(h, http.MethodGet, "127.0.0.1:9090", "/login", "", nil).Code)
	assert.Equal(t, http.StatusMisdirectedRequest, request(h, http.MethodGet, "honey.example.com", "/", "", nil).Code, "no public host without a login")
}
