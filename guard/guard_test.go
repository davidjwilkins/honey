package guard

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/davidjwilkins/honey/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wordpress stands in for WordPress: it says what it got, with the
// headers WordPress and Apache send
func wordpress(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Server", "Apache/2.4.68 (Debian)")
	w.Header().Set("X-Powered-By", "PHP/8.3.33")
	w.Header().Set("X-Pingback", "https://www.example.com/xmlrpc.php")
	w.Header().Set("Content-Type", "text/html")
	io.WriteString(w, "WordPress got "+r.Method+" "+r.URL.Path+" "+string(body))
}

func guarded(opts Options) http.Handler {
	return Middleware(opts, http.HandlerFunc(wordpress))
}

var defaults = Options{BlockPaths: true, XMLRPC: XMLRPCSafe, Headers: true}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestBlockedPaths(t *testing.T) {
	log := events.New(0)
	opts := defaults
	opts.Events = log
	h := guarded(opts)
	for _, p := range []string{
		"/wp-config.php", "/wp-config.php.bak", "/wp-config.php~", "/blog/wp-config.old", "/WP-CONFIG.PHP.save", "/wp-config-sample.php",
		"/.env", "/.git/config", "/wp-content/.htaccess", "/.DS_Store", "/a/../.env", "//.git//HEAD",
		"/wp-content/debug.log", "/backup.sql", "/db.sql.gz", "/wp-content/themes/x/functions.php.orig", "/index.php.swp",
		"/wp-content/uploads/2026/10/shell.php", "/wp-content/uploads/shell.PHP5", "/wp-content/uploads/a/shell.php.jpg",
		"/wp-content/uploads/shell.phtml", "/wp-content/uploads/x.php/extra", "/wp-content/./uploads/shell.php",
		"/wp-content/uploads/sites/2/shell.phar", "/wp-content/blogs.dir/2/files/shell.php",
		"/wp-admin/install.php", "/wp-admin/setup-config.php", "/readme.html",
	} {
		w := serve(h, httptest.NewRequest(http.MethodGet, "http://www.example.com"+p, nil))
		assert.Equal(t, http.StatusForbidden, w.Code, p)
		assert.Equal(t, BlockedHeader, w.Header().Get("X-Honey-Cache"), p)
		assert.Contains(t, w.Body.String(), "blocked by Honey (security.blockPaths)", p)
	}
	for _, p := range []string{
		"/", "/hello-world/", "/wp-login.php", "/wp-admin/", "/wp-admin/upgrade.php", "/wp-content/uploads/2026/10/photo.jpg",
		"/wp-content/uploads/php-tips.pdf", "/wp-content/plugins/x/script.php", "/.well-known/acme-challenge/token",
		"/catalog/", "/wp-content/themes/x/style.css", "/log/", "/blog/old-post/", "/wp-json/wp/v2/posts",
	} {
		w := serve(h, httptest.NewRequest(http.MethodGet, "http://www.example.com"+p, nil))
		assert.Equal(t, http.StatusOK, w.Code, p)
	}

	recent := log.Recent(10)
	require.NotEmpty(t, recent)
	assert.Equal(t, events.Refused, recent[0].Kind)
	assert.Contains(t, recent[0].Text, "blocked 3 requests for the installer, most recently GET \"/readme.html\"")

	off := guarded(Options{})
	assert.Equal(t, http.StatusOK, serve(off, httptest.NewRequest(http.MethodGet, "/.env", nil)).Code, "unless it's off")
	purge := httptest.NewRequest("PURGE", "/wp-config.php", nil)
	assert.Equal(t, http.StatusOK, serve(h, purge).Code, "purges are Honey's to handle")
}

func xmlrpc(body string, headers ...string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "http://www.example.com/xmlrpc.php", strings.NewReader(body))
	r.Header.Set("Content-Type", "text/xml")
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	return r
}

func call(method string) string {
	return `<?xml version="1.0"?><methodCall><methodName>` + method + `</methodName><params></params></methodCall>`
}

const multicall = `<?xml version="1.0"?>
<methodCall><methodName>system.multicall</methodName><params><param><value><array><data>
<value><struct><member><name>methodName</name><value><string>wp.getUsersBlogs</string></value></member>
<member><name>params</name><value><array><data><value><string>admin</string></value><value><string>password1</string></value></data></array></value></member></struct></value>
</data></array></value></param></params></methodCall>`

func TestXMLRPC(t *testing.T) {
	h := guarded(defaults)
	w := serve(h, xmlrpc(call("wp.getUsersBlogs")))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "WordPress got POST /xmlrpc.php "+call("wp.getUsersBlogs"), w.Body.String(), "the request reaches WordPress whole")
	assert.Equal(t, http.StatusOK, serve(h, httptest.NewRequest(http.MethodGet, "/xmlrpc.php", nil)).Code)

	for name, body := range map[string]string{
		"multicall":           multicall,
		"pingback":            call("pingback.ping"),
		"entities":            call("system&#46;multicall"),
		"hex entities":        call("system&#x2E;multicall"),
		"comment":             call("system.<!-- x -->multicall"),
		"CDATA":               call("<![CDATA[system.multicall]]>"),
		"spaces":              call("\n  system.multicall\t "),
		"NUL":                 call("system.multicall\x00"),
		"case":                call("System.Multicall"),
		"the last one counts": `<methodCall><methodName>demo.sayHello</methodName><methodName>system.multicall</methodName></methodCall>`,
		"a default namespace": `<methodCall xmlns="urn:x"><methodName>system.multicall</methodName></methodCall>`,
		"malformed":           `<methodCall><methodName>system.multicall</methodName`,
		"another encoding":    `<?xml version="1.0" encoding="UTF-16"?><methodCall><methodName>system.multicall</methodName></methodCall>`,
	} {
		w := serve(h, xmlrpc(body))
		assert.NotEqual(t, http.StatusOK, w.Code, name)
		assert.Equal(t, BlockedHeader, w.Header().Get("X-Honey-Cache"), name)
	}
	assert.Equal(t, http.StatusOK, serve(h, xmlrpc(`<methodCall><x:methodName xmlns:x="urn:x">system.multicall</x:methodName><methodName>demo.sayHello</methodName></methodCall>`)).Code,
		"a prefixed element isn't a methodName to WordPress")

	for _, p := range []string{"/blog/xmlrpc.php", "/xmlrpc.php/x", "/./XMLRPC.PHP"} {
		r := xmlrpc(multicall)
		r.URL.Path = p
		assert.Equal(t, http.StatusForbidden, serve(h, r).Code, p)
	}

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(multicall))
	zw.Close()
	assert.Equal(t, http.StatusUnsupportedMediaType, serve(h, xmlrpc(gz.String(), "Content-Encoding", "gzip")).Code, "compressed requests can't be checked")
	assert.Equal(t, http.StatusRequestEntityTooLarge, serve(h, xmlrpc(strings.Repeat(" ", maxXMLRPCBytes+1)+call("system.multicall"))).Code)

	opts := defaults
	opts.XMLRPC = XMLRPCOff
	w = serve(guarded(opts), httptest.NewRequest(http.MethodGet, "/XMLRPC.php", nil))
	assert.Equal(t, http.StatusForbidden, w.Code, "off blocks it all")
	assert.Contains(t, w.Body.String(), `security.xmlrpc = "off"`)

	opts.XMLRPC = XMLRPCOn
	assert.Equal(t, http.StatusOK, serve(guarded(opts), xmlrpc(multicall)).Code, "on allows it all")
}

func TestSecurityHeaders(t *testing.T) {
	h := guarded(defaults)
	w := serve(h, httptest.NewRequest(http.MethodGet, "/hello-world/", nil))
	assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "strict-origin-when-cross-origin", w.Header().Get("Referrer-Policy"))
	for _, name := range []string{"Server", "X-Powered-By", "X-Pingback"} {
		assert.Empty(t, w.Header().Get(name), "%s gives away what the site runs", name)
	}
	assert.Empty(t, w.Header().Get("X-Frame-Options"), "pages may be framed (e.g. embedded posts)")
	assert.Empty(t, w.Header().Get("Strict-Transport-Security"), "HSTS isn't on")

	for _, p := range []string{"/wp-login.php", "/wp-admin/post.php"} {
		w = serve(h, httptest.NewRequest(http.MethodGet, p, nil))
		assert.Equal(t, "SAMEORIGIN", w.Header().Get("X-Frame-Options"), p)
		assert.Equal(t, "frame-ancestors 'self'", w.Header().Get("Content-Security-Policy"), p)
	}

	opts := defaults
	opts.HSTS = "max-age=31536000"
	h = guarded(opts)
	w = serve(h, httptest.NewRequest(http.MethodGet, "http://www.example.com/", nil))
	assert.Empty(t, w.Header().Get("Strict-Transport-Security"), "only over HTTPS")
	r := httptest.NewRequest(http.MethodGet, "https://www.example.com/", nil)
	r.TLS = &tls.ConnectionState{}
	assert.Equal(t, "max-age=31536000", serve(h, r).Header().Get("Strict-Transport-Security"))
	r = httptest.NewRequest(http.MethodGet, "http://www.example.com/", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	assert.Equal(t, "max-age=31536000", serve(h, r).Header().Get("Strict-Transport-Security"), "or HTTPS to a trusted proxy")

	opts = defaults
	opts.XMLRPC = XMLRPCOn
	assert.NotEmpty(t, serve(guarded(opts), httptest.NewRequest(http.MethodGet, "/", nil)).Header().Get("X-Pingback"), "pingbacks are on")

	w = serve(guarded(Options{}), httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, "PHP/8.3.33", w.Header().Get("X-Powered-By"), "unless headers are off")
	assert.Empty(t, w.Header().Get("X-Content-Type-Options"))
}

func TestBackendHeadersWin(t *testing.T) {
	h := Middleware(defaults, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		w.WriteHeader(http.StatusOK)
	}))
	w := serve(h, httptest.NewRequest(http.MethodGet, "/wp-login.php", nil))
	assert.Equal(t, "no-referrer", w.Header().Get("Referrer-Policy"))
	assert.Equal(t, "DENY", w.Header().Get("X-Frame-Options"))
	assert.Equal(t, []string{"frame-ancestors 'none'"}, w.Header().Values("Content-Security-Policy"))
}
