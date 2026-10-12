package admin

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/davidjwilkins/honey/clientip"
	"github.com/davidjwilkins/honey/events"
	"golang.org/x/crypto/bcrypt"
)

// Logging in: with Options.PasswordHash set, the admin page is only shown
// to those who've logged in with the password, which lets it be served on
// a public address (see Options.Host).  Logging in gives the browser a
// session cookie (HttpOnly, SameSite=Strict, and Secure over HTTPS), kept
// in memory, which lasts for sessionIdle without being used, or
// sessionLifetime at most.  An address which gets the password wrong
// loginAttempts times within loginLockout can't try again until then.

const (
	// SessionCookie is the admin page's session cookie
	SessionCookie = "honey_admin"
	// sessionIdle and sessionLifetime bound how long a session lasts
	sessionIdle     = 12 * time.Hour
	sessionLifetime = 7 * 24 * time.Hour
	// loginAttempts and loginLockout limit guessing the password
	loginAttempts = 5
	loginLockout  = 15 * time.Minute
	// maxSessions bounds the sessions kept
	maxSessions = 1000
)

// HashPassword returns a hash of password, for Options.PasswordHash (and
// the [admin] passwordHash setting)
func HashPassword(password string) (string, error) {
	if len(password) < 12 {
		return "", fmt.Errorf("use a password of at least 12 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	return string(hash), err
}

// CheckPasswordHash returns whether hash is a password hash HashPassword
// could have made
func CheckPasswordHash(hash string) error {
	if _, err := bcrypt.Cost([]byte(hash)); err != nil {
		return fmt.Errorf("it isn't a password hash from honey -hash-password: %w", err)
	}
	return nil
}

type session struct {
	started, used time.Time
}

type logins struct {
	hash []byte
	now  func() time.Time

	mu       sync.Mutex
	sessions map[string]*session
	failures map[string][]time.Time
}

func newLogins(hash string, now func() time.Time) *logins {
	if now == nil {
		now = time.Now
	}
	return &logins{hash: []byte(hash), now: now, sessions: map[string]*session{}, failures: map[string][]time.Time{}}
}

// loggedIn returns whether r has a current session, and updates it
func (l *logins) loggedIn(r *http.Request) bool {
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return false
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.sessions[c.Value]
	if s == nil {
		return false
	}
	if now.Sub(s.used) > sessionIdle || now.Sub(s.started) > sessionLifetime {
		delete(l.sessions, c.Value)
		return false
	}
	s.used = now
	return true
}

// start returns a new session's token
func (l *logins) start() string {
	b := make([]byte, 32)
	rand.Read(b)
	token := hex.EncodeToString(b)
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.sessions) >= maxSessions {
		// The least recently used goes
		oldest := ""
		for t, s := range l.sessions {
			if oldest == "" || s.used.Before(l.sessions[oldest].used) {
				oldest = t
			}
		}
		delete(l.sessions, oldest)
	}
	l.sessions[token] = &session{started: now, used: now}
	return token
}

// end ends r's session
func (l *logins) end(r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil {
		l.mu.Lock()
		delete(l.sessions, c.Value)
		l.mu.Unlock()
	}
}

// lockedOut returns how long address must wait to try again, if it must
func (l *logins) lockedOut(address string) time.Duration {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	var recent []time.Time
	for _, t := range l.failures[address] {
		if now.Sub(t) < loginLockout {
			recent = append(recent, t)
		}
	}
	l.failures[address] = recent
	if len(recent) == 0 {
		delete(l.failures, address)
	}
	if len(recent) < loginAttempts {
		return 0
	}
	return loginLockout - now.Sub(recent[len(recent)-loginAttempts])
}

func (l *logins) failed(address string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.failures) < 10000 {
		l.failures[address] = append(l.failures[address], l.now())
	}
}

func (l *logins) succeeded(address string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, address)
}

// loginView is what the login page shows
type loginView struct {
	Next    string
	Problem string
	// Insecure is whether the page isn't served over HTTPS (or to this
	// host), so the password can't be sent
	Insecure bool
}

// secure returns whether r came over HTTPS, or from this host
func secure(r *http.Request) bool {
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return true
	}
	ip := clientip.From(r)
	return ip != nil && ip.IsLoopback()
}

// localNext returns next if it's a path on the admin page, or "/"
func localNext(next string) string {
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(next, "//") ||
		strings.HasPrefix(u.Path, "/login") || strings.ContainsAny(next, "\\\r\n") {
		return "/"
	}
	return u.RequestURI()
}

// login shows the login page, and logs in with its form
func (a *admin) login(w http.ResponseWriter, r *http.Request) {
	v := loginView{Next: localNext(r.FormValue("next")), Insecure: !secure(r)}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		a.render(w, "login", v)
		return
	case http.MethodPost:
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if v.Insecure {
		w.WriteHeader(http.StatusForbidden)
		a.render(w, "login", v)
		return
	}
	address := "unknown"
	if ip := clientip.From(r); ip != nil {
		address = ip.String()
	}
	if wait := a.logins.lockedOut(address); wait > 0 {
		v.Problem = fmt.Sprintf("Too many wrong passwords: try again in %s.", formatDuration(wait.Round(time.Minute)+time.Minute))
		w.Header().Set("Retry-After", fmt.Sprint(int(wait.Seconds())+1))
		w.WriteHeader(http.StatusTooManyRequests)
		a.render(w, "login", v)
		return
	}
	if bcrypt.CompareHashAndPassword(a.logins.hash, []byte(r.PostFormValue("password"))) != nil {
		a.logins.failed(address)
		a.opts.Events.Add(events.Refused, "admin login failed "+address, 10*time.Minute, func(e *events.Event) {
			e.Text = fmt.Sprintf("wrong password for the admin page from %s (%d %s)", address, e.Count, plural(e.Count, "time", "times"))
		})
		v.Problem = "That isn't the password."
		w.WriteHeader(http.StatusUnauthorized)
		a.render(w, "login", v)
		return
	}
	a.logins.succeeded(address)
	a.opts.Events.Add(events.Info, "", 0, events.Text("logged in to the admin page from "+address))
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: a.logins.start(), Path: "/",
		HttpOnly: true, Secure: r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
		SameSite: http.SameSiteStrictMode, MaxAge: int(sessionLifetime.Seconds())})
	http.Redirect(w, r, v.Next, http.StatusSeeOther)
}

// logout ends the session
func (a *admin) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.logins.end(r)
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// requireLogin sends those who haven't logged in to the login page
func (a *admin) requireLogin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login", "/admin.css", "/admin.js":
			next.ServeHTTP(w, r)
			return
		}
		if a.logins.loggedIn(r) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			if accepts := r.Header.Get("Accept"); strings.Contains(accepts, "text/html") || accepts == "" {
				http.Redirect(w, r, "/login?"+url.Values{"next": {r.URL.RequestURI()}}.Encode(), http.StatusSeeOther)
				return
			}
		}
		http.Error(w, "Log in to the admin page first.", http.StatusUnauthorized)
	})
}

// publicHost returns whether hostport is the admin page's public host
func (a *admin) publicHost(hostport string) bool {
	if a.opts.Host == "" {
		return false
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return strings.EqualFold(host, a.opts.Host)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
