package guard

import (
	"net"
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/davidjwilkins/honey/clientip"
)

var (
	// wpAdmin is the dashboard, in any directory (for WordPress in a
	// subdirectory)
	wpAdmin = regexp.MustCompile(`(?i)(^|/)wp-admin(/|$)`)
	// publicAdmin are the parts of wp-admin which serve visitors, not the
	// dashboard: themes' and plugins' AJAX calls, and their forms
	publicAdmin = regexp.MustCompile(`(?i)(^|/)wp-admin/(admin-ajax|admin-post)\.php$`)
)

// dashboard returns whether r is for the dashboard or the login page (but
// not a password-protected post's password, which visitors send to
// wp-login.php)
func dashboard(r *http.Request) bool {
	p := path.Clean("/" + r.URL.Path)
	if wpLogin.MatchString(p) {
		return r.URL.Query().Get("action") != "postpass"
	}
	return wpAdmin.MatchString(p) && !publicAdmin.MatchString(p)
}

// allowedIP returns whether r is from one of networks
func allowedIP(r *http.Request, networks []*net.IPNet) bool {
	ip := clientip.From(r)
	if ip == nil {
		return false
	}
	for _, n := range networks {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// usersRoute is the REST API's list of users, and each user
var usersRoute = regexp.MustCompile(`(?i)^/wp/v2/users(/|$)`)

// listsUsers returns why r is blocked as listing the site's users to
// someone who isn't logged in, or "" if it isn't: ?author=N (which
// WordPress redirects to the author's archive, giving away their username),
// and the REST API's users.  A logged in visitor's cookie, or an
// Authorization header, lets it through to WordPress, which decides (see
// the WordPress plugin, which checks the cookie is real).
func listsUsers(r *http.Request) string {
	if r.Header.Get("Authorization") != "" || loggedIn(r) {
		return ""
	}
	query := r.URL.Query()
	if author := strings.TrimSpace(query.Get("author")); author != "" && strings.Trim(author, "0123456789-, ") == "" {
		return "user enumeration (?author=)"
	}
	route := query.Get("rest_route")
	// /wp-json/..., or /blog/wp-json/... for WordPress in a subdirectory
	if p := path.Clean("/" + r.URL.Path); strings.Contains(strings.ToLower(p), "/wp-json/") {
		route = p[strings.Index(strings.ToLower(p), "/wp-json/")+len("/wp-json"):]
	}
	if route != "" && usersRoute.MatchString(path.Clean("/"+route)) {
		return "user enumeration (the REST API's users)"
	}
	return ""
}

// loggedIn returns whether r has WordPress's logged in cookie
func loggedIn(r *http.Request) bool {
	for _, c := range r.Cookies() {
		if strings.HasPrefix(c.Name, "wordpress_logged_in_") {
			return true
		}
	}
	return false
}
