package guard

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"net/http"
	"path"
	"regexp"
	"strings"
)

// Kinds of login (see loginKind)
const (
	loginForm   = "form"
	loginXMLRPC = "xmlrpc"
	loginAuth   = "auth"
)

var (
	wpLogin = regexp.MustCompile(`(?i)(^|/)wp-login\.php$`)
	// xmlrpcLoginFailed is the fault XML-RPC answers a wrong username or
	// password with
	xmlrpcLoginFailed = regexp.MustCompile(`faultCode</name>\s*<value>\s*<int>403</int>`)
)

// maxSniffBytes is how much of an XML-RPC response is looked at for a
// failed login
const maxSniffBytes = 8 << 10

// loginKind returns which kind of login r is, or "" if it isn't one: a
// post of the login form, an XML-RPC call (which may log in), or a request
// with an Authorization header (e.g. the REST API with an application
// password)
func loginKind(r *http.Request) string {
	switch {
	case r.Method == http.MethodPost && isXMLRPC(r.URL.Path):
		return loginXMLRPC
	case r.Method == http.MethodPost && wpLogin.MatchString(path.Clean("/"+r.URL.Path)):
		// Not e.g. ?action=lostpassword, or a post's password
		if action := r.URL.Query().Get("action"); action == "" || action == "login" {
			return loginForm
		}
	case r.Header.Get("Authorization") != "":
		return loginAuth
	}
	return ""
}

// loginWriter watches the response to a login, to tell whether it failed
type loginWriter struct {
	http.ResponseWriter
	kind     string
	status   int
	loggedIn bool
	sniffed  bytes.Buffer
}

func (w *loginWriter) WriteHeader(status int) {
	if w.status == 0 && status >= 200 {
		w.status = status
		for _, cookie := range w.Header().Values("Set-Cookie") {
			if strings.HasPrefix(strings.TrimSpace(cookie), "wordpress_logged_in_") {
				w.loggedIn = true
			}
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *loginWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.kind == loginXMLRPC && w.sniffed.Len() < maxSniffBytes {
		w.sniffed.Write(p[:min(len(p), maxSniffBytes-w.sniffed.Len())])
	}
	return w.ResponseWriter.Write(p)
}

// outcome returns whether the login failed, and whether it succeeded (it
// may be neither, e.g. an XML-RPC call which doesn't log in)
func (w *loginWriter) outcome() (failed, succeeded bool) {
	switch w.kind {
	case loginForm:
		// The form is shown again (with the error) if it fails, and the
		// visitor is redirected with the logged in cookie if it succeeds
		return w.status == http.StatusOK && !w.loggedIn, w.loggedIn
	case loginAuth:
		return w.status == http.StatusUnauthorized, w.status >= 200 && w.status < 300
	case loginXMLRPC:
		return xmlrpcLoginFailed.Match(w.sniffed.Bytes()), false
	}
	return false, false
}

// Flush lets streamed responses be flushed through
func (w *loginWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack lets the proxy take over the connection, e.g. for websockets
func (w *loginWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("guard: the ResponseWriter can't be hijacked")
	}
	return h.Hijack()
}

// Unwrap lets http.ResponseController reach the underlying ResponseWriter
func (w *loginWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
