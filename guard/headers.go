package guard

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strings"
)

// versionHeaders say which software (and versions) the site runs, which
// helps attackers pick their exploits
var versionHeaders = []string{"Server", "X-Powered-By", "X-AspNet-Version", "X-Generator"}

// headerWriter sets security headers on a response as its status is
// written
type headerWriter struct {
	http.ResponseWriter
	r     *http.Request
	opts  Options
	wrote bool
}

func (w *headerWriter) WriteHeader(status int) {
	if !w.wrote && status >= 200 {
		w.wrote = true
		w.secure(w.Header())
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *headerWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

// secure sets the security headers, unless the backend has
func (w *headerWriter) secure(h http.Header) {
	setDefault := func(name, value string) {
		if h.Get(name) == "" {
			h.Set(name, value)
		}
	}
	if w.opts.HSTS != "" && overHTTPS(w.r) {
		setDefault("Strict-Transport-Security", w.opts.HSTS)
	}
	if !w.opts.Headers {
		return
	}
	for _, name := range versionHeaders {
		h.Del(name)
	}
	if w.opts.XMLRPC != XMLRPCOn {
		// It advertises XML-RPC for pingbacks
		h.Del("X-Pingback")
	}
	setDefault("X-Content-Type-Options", "nosniff")
	setDefault("Referrer-Policy", "strict-origin-when-cross-origin")
	if adminPage(w.r.URL.Path) {
		// Nobody else's site may frame them, to trick people into clicking
		setDefault("X-Frame-Options", "SAMEORIGIN")
		if !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors") {
			h.Add("Content-Security-Policy", "frame-ancestors 'self'")
		}
	}
}

// overHTTPS returns whether r came over HTTPS: to Honey, or to a trusted
// proxy in front of it (see clientip.Middleware)
func overHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// adminPage returns whether p is WordPress's dashboard or login page
func adminPage(p string) bool {
	p = strings.ToLower(p)
	return strings.HasPrefix(p, "/wp-admin") || strings.HasPrefix(p, "/wp-login.php")
}

// Flush lets streamed responses be flushed through
func (w *headerWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack lets the proxy take over the connection, e.g. for websockets
func (w *headerWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("guard: the ResponseWriter can't be hijacked")
	}
	return h.Hijack()
}

// Unwrap lets http.ResponseController reach the underlying ResponseWriter
func (w *headerWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
