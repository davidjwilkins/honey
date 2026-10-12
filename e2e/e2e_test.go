//go:build e2e

// Package e2e tests Honey in front of a real WordPress, with the purge
// plugin installed.  Run it with ./e2e/run.sh, which starts WordPress,
// Honey and a database with Docker, and sets WordPress up.
package e2e

import (
	"context"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// site is WordPress's address, through Honey
const site = "http://localhost:8080"

// adminPage is Honey's admin page
const adminPage = "http://127.0.0.1:9090"

// client requests pages through Honey, as a visitor with the cookies in
// jar (or none, if it is nil), without following redirects
func client(jar http.CookieJar) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			// localhost may resolve to ::1, which Docker isn't listening on
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if host, port, err := net.SplitHostPort(addr); err == nil && host == "localhost" {
					addr = net.JoinHostPort("127.0.0.1", port)
				}
				return dialer.DialContext(ctx, network, addr)
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func newJar(t *testing.T) http.CookieJar {
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	return jar
}

type page struct {
	*http.Response
	Body  string
	Cache string // X-Honey-Cache
}

func do(t *testing.T, c *http.Client, method, path string, form url.Values, headers ...string) page {
	t.Helper()
	target := path
	if !strings.HasPrefix(path, "http") {
		target = site + path
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, target, body)
	require.NoError(t, err)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := c.Do(req)
	require.NoError(t, err)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	return page{Response: resp, Body: string(b), Cache: resp.Header.Get("X-Honey-Cache")}
}

var (
	scripts = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	tags    = regexp.MustCompile(`(?s)<[^>]*>`)
	spaces  = regexp.MustCompile(`\s+`)
)

// text returns the text of the page's main element (or else body), for
// failure messages
func text(body string) string {
	if i := strings.Index(body, "<main"); i >= 0 {
		body = body[i:]
	}
	body = scripts.ReplaceAllString(body, " ")
	body = spaces.ReplaceAllString(tags.ReplaceAllString(body, " "), " ")
	if len(body) > 1500 {
		body = body[:1500] + "..."
	}
	return body
}

// describe returns the headers which say how a response may be cached,
// for failure messages
func describe(p page) string {
	var parts []string
	for _, name := range []string{"Age", "Cache-Control", "Expires", "Date", "Vary", "Set-Cookie", "Etag", "X-Honey-Stale"} {
		for _, value := range p.Header.Values(name) {
			parts = append(parts, name+": "+value)
		}
	}
	return strings.Join(parts, "; ")
}

// contains checks that body contains s, without printing the whole body
func contains(t *testing.T, body, s string, msgAndArgs ...interface{}) {
	t.Helper()
	if !strings.Contains(body, s) {
		assert.Fail(t, fmt.Sprintf("the page doesn't contain %q", s), msgAndArgs...)
	}
}

// notContains checks that body doesn't contain s
func notContains(t *testing.T, body, s string, msgAndArgs ...interface{}) {
	t.Helper()
	if strings.Contains(body, s) {
		assert.Fail(t, fmt.Sprintf("the page contains %q", s), msgAndArgs...)
	}
}

func get(t *testing.T, c *http.Client, path string, headers ...string) page {
	t.Helper()
	return do(t, c, http.MethodGet, path, nil, headers...)
}

// wp runs a WP-CLI command against the WordPress, and returns its output
func wp(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("docker", append([]string{"compose", "run", "--rm", "-T", "cli", "wp"}, args...)...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	require.NoError(t, err, "wp %s", strings.Join(args, " "))
	return strings.TrimSpace(string(out))
}

var posts atomic.Int32

// newPost publishes a post in the News category, and returns its path
func newPost(t *testing.T, title string) (id, path string) {
	t.Helper()
	return newPostIn(t, title, "news")
}

// newPostIn publishes a post in a category, and returns its path
func newPostIn(t *testing.T, title, category string) (id, path string) {
	t.Helper()
	slug := fmt.Sprintf("e2e-%d-%d", time.Now().UnixNano(), posts.Add(1))
	id = wp(t, "post", "create", "--porcelain", "--post_status=publish", "--post_name="+slug,
		"--post_title="+title, "--post_content=Some content.", "--post_category="+category)
	return id, "/" + slug + "/"
}

// logIn logs in to WordPress as user, and returns their cookies
func logIn(t *testing.T, user string) http.CookieJar {
	t.Helper()
	jar := newJar(t)
	c := client(jar)
	get(t, c, "/wp-login.php") // sets the test cookie
	resp := do(t, c, http.MethodPost, "/wp-login.php", url.Values{
		"log": {user}, "pwd": {"password"}, "testcookie": {"1"}, "wp-submit": {"Log In"},
		"redirect_to": {site + "/wp-admin/"},
	})
	require.Equal(t, http.StatusFound, resp.StatusCode, "logging in as %s: %s", user, resp.Body)
	u, _ := url.Parse(site)
	var loggedIn bool
	for _, cookie := range jar.Cookies(u) {
		loggedIn = loggedIn || strings.HasPrefix(cookie.Name, "wordpress_logged_in_")
	}
	require.True(t, loggedIn, "logging in as %s", user)
	return jar
}

// eventually requests path until its body contains want, and returns the
// first response, which should be the old version served while the new
// one was fetched
func eventually(t *testing.T, c *http.Client, path, want string) page {
	t.Helper()
	first := get(t, c, path)
	deadline := time.Now().Add(10 * time.Second)
	for resp := first; ; resp = get(t, c, path) {
		if strings.Contains(resp.Body, want) {
			return first
		}
		if time.Now().After(deadline) {
			assert.Fail(t, fmt.Sprintf("%s never contained %q", path, want),
				"the last response was %d %s, with the text: %s", resp.StatusCode, resp.Cache, text(resp.Body))
			return first
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// cached requests path until it is served from the cache, and returns
// that response: a page which has been soft purged (e.g. the home page,
// when a post is published) is refreshed in the background, so the
// request after the stale one may come before the refresh is done
func cached(t *testing.T, c *http.Client, path string) page {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp := get(t, c, path)
		if resp.Cache == "HIT" {
			return resp
		}
		if time.Now().After(deadline) {
			require.Fail(t, fmt.Sprintf("%s was never served from the cache", path), "the last response was %d %s: %s", resp.StatusCode, resp.Cache, describe(resp))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestPagesAreCached(t *testing.T) {
	c := client(nil)
	_, path := newPost(t, "Cached post")
	first := get(t, c, path)
	require.Equal(t, http.StatusOK, first.StatusCode)
	contains(t, first.Body, "Cached post")
	assert.Equal(t, "MISS", first.Cache)
	second := get(t, c, path)
	assert.Equal(t, "HIT", second.Cache)
	assert.Equal(t, first.Body, second.Body)

	compressed := get(t, c, path, "Accept-Encoding", "br")
	assert.Equal(t, "br", compressed.Header.Get("Content-Encoding"))
}

func TestRedirectsKeepTheSitesAddress(t *testing.T) {
	// WordPress redirects to a page's canonical address, built from the
	// Host it is sent: if Honey sent the backend's, it would redirect
	// visitors to http://wordpress/... (and cache that for everyone)
	c := client(nil)
	id, path := newPost(t, "Redirected post")
	for i := 0; i < 2; i++ {
		resp := get(t, c, "/?p="+id)
		assert.Equal(t, http.StatusMovedPermanently, resp.StatusCode)
		assert.Equal(t, site+path, resp.Header.Get("Location"))
	}
	assert.Equal(t, http.StatusOK, get(t, c, path).StatusCode)
}

func TestLoggedInUsersArentCached(t *testing.T) {
	_, path := newPost(t, "Logged in post")
	admin := client(logIn(t, "admin"))
	anonymous := client(nil)

	for i := 0; i < 2; i++ {
		resp := get(t, admin, path)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "NO-CACHE", resp.Cache)
		contains(t, resp.Body, "wpadminbar", "logged in users get the admin bar")
		contains(t, resp.Body, "wp-admin-bar-honey-purge", "with the plugin's purge button")

		resp = get(t, anonymous, path)
		notContains(t, resp.Body, "wpadminbar", "visitors never get a logged in user's page")
	}
	assert.Equal(t, "HIT", get(t, anonymous, path).Cache)
}

func TestEditingAPostPurgesIt(t *testing.T) {
	c := client(nil)
	id, path := newPost(t, "Before editing")
	for _, p := range []string{path, "/", "/category/news/"} {
		cached(t, c, p)
	}

	wp(t, "post", "update", id, "--post_title=After editing")
	for _, p := range []string{path, "/", "/category/news/"} {
		// A soft purge: the old version is served once more, without
		// waiting for WordPress, while the new one is fetched
		first := eventually(t, c, p, "After editing")
		assert.Equal(t, "STALE", first.Cache, p)
		resp := get(t, c, p)
		assert.Equal(t, "HIT", resp.Cache, "%s: %s", p, describe(resp))
		notContains(t, resp.Body, "Before editing", p)
	}
}

func TestPasswordProtectingAPostHidesItAtOnce(t *testing.T) {
	c := client(nil)
	id, path := newPost(t, "Soon protected")
	wp(t, "post", "update", id, "--post_content=Not for everyone.")
	eventually(t, c, path, "Not for everyone.")
	cached(t, c, path)

	wp(t, "post", "update", id, "--post_password=sesame")
	resp := get(t, c, path)
	assert.NotEqual(t, "STALE", resp.Cache, "a hard purge: the old version is never served again")
	notContains(t, resp.Body, "Not for everyone.")
}

func TestChangingTheSlugPurgesTheOldAddress(t *testing.T) {
	c := client(nil)
	id, path := newPost(t, "Moving post")
	cached(t, c, path)

	wp(t, "post", "update", id, "--post_name="+strings.Trim(path, "/")+"-moved")
	resp := get(t, c, path)
	assert.NotEqual(t, "HIT", resp.Cache, "the old address isn't served from the cache")
	assert.Equal(t, http.StatusMovedPermanently, resp.StatusCode, "WordPress redirects it to the new one")
}

func TestUnpublishingAPostPurgesIt(t *testing.T) {
	c := client(nil)
	id, path := newPost(t, "Soon a draft")
	cached(t, c, path)

	wp(t, "post", "update", id, "--post_status=draft")
	assert.Equal(t, http.StatusNotFound, get(t, c, path).StatusCode)
}

func TestCommentsAppearOnceApproved(t *testing.T) {
	id, path := newPost(t, "Commented post")
	anonymous := client(nil)
	cached(t, anonymous, path)

	commenter := client(newJar(t))
	resp := do(t, commenter, http.MethodPost, "/wp-comments-post.php", url.Values{
		"comment_post_ID": {id}, "author": {"Jane Commenter"}, "email": {"jane@example.com"},
		"comment": {"What a lovely honey cache."}, "wp-comment-cookies-consent": {"yes"},
	})
	require.Equal(t, http.StatusFound, resp.StatusCode, resp.Body)

	eventually(t, anonymous, path, "What a lovely honey cache.")
	// The comments may be refreshed before the page around them is
	page := cached(t, anonymous, path)
	contains(t, page.Body, "What a lovely honey cache.")
	notContains(t, page.Body, "jane@example.com")

	// The commenter gets the cached page too, with the comments section
	// (an Edge Side Include) fetched for them, so that their details are
	// filled in on the form for them only
	page = get(t, commenter, path)
	assert.Equal(t, "HIT", page.Cache, describe(page))
	contains(t, page.Body, "Commented post")
	contains(t, page.Body, "What a lovely honey cache.")
	contains(t, page.Body, "jane@example.com")
	assert.Equal(t, "private, no-cache", page.Header.Get("Cache-Control"))
	notContains(t, get(t, anonymous, path).Body, "jane@example.com")
}

func TestCommentsAreEdgeSideIncludes(t *testing.T) {
	_, path := newPost(t, "Post with comments")
	anonymous := client(nil)
	page := cached(t, anonymous, path)
	contains(t, page.Body, `id="commentform"`, "the comments section is filled in: %s", text(page.Body))
	notContains(t, page.Body, "esi:")
	assert.Equal(t, "private, no-cache", page.Header.Get("Cache-Control"))
	assert.Empty(t, page.Header.Get("Etag"))
	assert.Empty(t, page.Header.Get("Surrogate-Control"))

	include := get(t, anonymous, path+"?honey-esi=comments.0000000000000000")
	assert.Equal(t, http.StatusNotFound, include.StatusCode, "only the comments sections WordPress has shown")

	// Logged in users still go to WordPress, which shows them the page
	// without an include
	admin := get(t, client(logIn(t, "admin")), path)
	assert.Equal(t, "NO-CACHE", admin.Cache)
	contains(t, admin.Body, `id="commentform"`)
	notContains(t, admin.Body, "esi:")

	tested := get(t, anonymous, adminPage+"/test?"+url.Values{"url": {site + path}}.Encode())
	contains(t, tested.Body, "Uses ESI", "the admin page says the page uses ESI")
	tested = get(t, anonymous, adminPage+"/test?"+url.Values{"url": {site + path}, "cookies": {"comment_author_abc=Jane"}}.Encode())
	contains(t, tested.Body, "Served the cached page, with ESI", "and that commenters get it")
}

func TestPasswordProtectedPosts(t *testing.T) {
	anonymous := client(nil)
	visitor := client(newJar(t))
	for i := 0; i < 2; i++ {
		notContains(t, get(t, anonymous, "/members-only/").Body, "The secret is honey.")
	}

	resp := do(t, visitor, http.MethodPost, "/wp-login.php?action=postpass", url.Values{"post_password": {"open-sesame"}},
		"Referer", site+"/members-only/")
	require.Equal(t, http.StatusFound, resp.StatusCode, resp.Body)
	page := get(t, visitor, "/members-only/")
	assert.Equal(t, "NO-CACHE", page.Cache)
	contains(t, page.Body, "The secret is honey.")
	notContains(t, get(t, anonymous, "/members-only/").Body, "The secret is honey.")
}

func TestChangingTheSiteTitlePurgesEverything(t *testing.T) {
	c := client(nil)
	_, path := newPost(t, "Titled post")
	for _, p := range []string{path, "/"} {
		cached(t, c, p)
	}

	title := fmt.Sprintf("Honey test %d", time.Now().UnixNano())
	wp(t, "option", "update", "blogname", title)
	for _, p := range []string{path, "/"} {
		first := eventually(t, c, p, title)
		assert.Equal(t, "STALE", first.Cache, p)
	}
}

// purgeEverything presses the plugin's "Purge cache" button as admin
func purgeEverything(t *testing.T) {
	t.Helper()
	admin := client(logIn(t, "admin"))
	dashboard := get(t, admin, "/wp-admin/")
	link := regexp.MustCompile(`id=["']wp-admin-bar-honey-purge["']><a [^>]*href=["']([^"']+)["']`).FindStringSubmatch(dashboard.Body)
	require.Len(t, link, 2, "the admin bar has the purge button")
	resp := get(t, admin, strings.NewReplacer("&#038;", "&", "&amp;", "&").Replace(link[1]))
	require.Equal(t, http.StatusFound, resp.StatusCode, resp.Body)
}

func TestWarmingAfterPurgingEverything(t *testing.T) {
	c := client(nil)
	_, path := newPost(t, "Warmed post")
	purgeEverything(t)
	// Honey fetches every page in the sitemap again, at 50 a second
	time.Sleep(5 * time.Second)
	for _, p := range []string{path, "/", "/hello-world/"} {
		assert.Equal(t, "HIT", get(t, c, p).Cache, "%s was cached before anyone asked for it", p)
	}
}

func TestPurgeButton(t *testing.T) {
	c := client(nil)
	id, path := newPost(t, "Purged by button")
	cached(t, c, path)
	// Change it behind the plugin's back, so that only the button purges it
	wp(t, "db", "query", "UPDATE wp_posts SET post_title = 'Changed quietly' WHERE ID = "+id)
	notContains(t, get(t, c, path).Body, "Changed quietly")

	admin := client(logIn(t, "admin"))
	dashboard := get(t, admin, "/wp-admin/")
	require.Equal(t, http.StatusOK, dashboard.StatusCode)
	link := regexp.MustCompile(`id=["']wp-admin-bar-honey-purge["']><a [^>]*href=["']([^"']+)["']`).FindStringSubmatch(dashboard.Body)
	require.Len(t, link, 2, "the admin bar has the purge button")
	purgeURL := strings.NewReplacer("&#038;", "&", "&amp;", "&").Replace(link[1])
	resp := get(t, admin, purgeURL)
	require.Equal(t, http.StatusFound, resp.StatusCode, resp.Body)
	assert.Contains(t, resp.Header.Get("Location"), "honey-purged=1")
	// A hard purge: the old version is never served again (whether the
	// page is fetched now, or the warmer already has)
	contains(t, get(t, c, path).Body, "Changed quietly")

	// Editors can't use it
	editor := client(logIn(t, "editor"))
	notContains(t, get(t, editor, "/wp-admin/").Body, "wp-admin-bar-honey-purge")
	resp = get(t, editor, purgeURL)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestStaticFiles(t *testing.T) {
	c := client(nil)
	const css = "/wp-includes/css/dashicons.min.css"
	first := get(t, c, css)
	require.Equal(t, http.StatusOK, first.StatusCode)
	assert.Equal(t, "HIT", get(t, c, css).Cache)
	assert.Equal(t, "br", get(t, c, css, "Accept-Encoding", "br").Header.Get("Content-Encoding"))
}

func TestAdminPage(t *testing.T) {
	c := client(nil)
	_, path := newPost(t, "Purged from the admin page")
	cached(t, c, path)

	page := get(t, c, adminPage+"/")
	require.Equal(t, http.StatusOK, page.StatusCode)
	contains(t, page.Body, "Served from cache")
	count := get(t, c, adminPage+"/count?"+url.Values{"url": {site + path}}.Encode())
	assert.JSONEq(t, `{"count":1}`, count.Body, "the page says how many responses a purge would remove")
	resp := do(t, c, http.MethodPost, adminPage+"/purge", url.Values{"url": {site + path}}, "Origin", adminPage)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	contains(t, get(t, c, adminPage+resp.Header.Get("Location")).Body, "Purged 1 cached response")
	assert.Equal(t, "MISS", get(t, c, path).Cache)

	stats := get(t, c, adminPage+"/stats").Body
	contains(t, stats, "purged "+site+path+" · 1 response · from the admin page", "the purge is in the events")
	contains(t, stats, "warmed ", "and so is warming the cache")
}

func TestApprovingACommentPurgesThePost(t *testing.T) {
	c := client(nil)
	id, path := newPost(t, "Moderated post")
	comment := wp(t, "comment", "create", "--porcelain", "--comment_post_ID="+id, "--comment_approved=0",
		"--comment_author=Held Back", "--comment_content=A comment awaiting moderation.")
	page := cached(t, c, path)
	notContains(t, page.Body, "A comment awaiting moderation.")

	wp(t, "comment", "approve", comment)
	first := eventually(t, c, path, "A comment awaiting moderation.")
	assert.Equal(t, "STALE", first.Cache)

	// Removing it is a hard purge: it is never served again
	wp(t, "comment", "spam", comment)
	page = get(t, c, path)
	assert.NotEqual(t, "STALE", page.Cache)
	notContains(t, page.Body, "A comment awaiting moderation.")
}

func TestRenamingACategoryPurgesIt(t *testing.T) {
	c := client(nil)
	wp(t, "term", "create", "category", "Gardening", "--slug=gardening")
	newPostIn(t, "A gardening post", "gardening")
	page := cached(t, c, "/category/gardening/")
	require.Equal(t, http.StatusOK, page.StatusCode)
	contains(t, page.Body, "A gardening post")

	wp(t, "term", "update", "category", "gardening", "--by=slug", "--name=Beekeeping")
	eventually(t, c, "/category/gardening/", "Beekeeping")
}

func TestAdminTestAndSettings(t *testing.T) {
	c := client(nil)
	_, path := newPost(t, "Tested on the admin page")
	cached(t, c, path)

	page := get(t, c, adminPage+"/test?"+url.Values{"url": {site + path}, "cookies": {"_ga=1"}}.Encode())
	require.Equal(t, http.StatusOK, page.StatusCode)
	contains(t, page.Body, "Cacheable, in the cache now")
	contains(t, page.Body, "removed _ga before the backend sees them")
	// queryParams isn't set, so any parameter makes it another page
	page = get(t, c, adminPage+"/test?"+url.Values{"url": {site + path + "?utm_source=x"}}.Encode())
	contains(t, page.Body, "Cacheable, not in the cache")
	page = get(t, c, adminPage+"/test?"+url.Values{"url": {site + "/wp-admin/"}}.Encode())
	contains(t, page.Body, "Not cached: every request goes to the backend")

	settings := get(t, c, adminPage+"/settings")
	require.Equal(t, http.StatusOK, settings.StatusCode)
	notContains(t, settings.Body, "e2e-secret-0123456789", "the secret is never shown")
	export := do(t, c, http.MethodPost, adminPage+"/settings/export",
		url.Values{"cache.defaultTTL": {"10m"}}, "Origin", adminPage)
	require.Equal(t, http.StatusOK, export.StatusCode, export.Body)
	contains(t, export.Body, `defaultTTL = "10m"`)
	contains(t, export.Body, `secret = "e2e-secret-0123456789"`, "the export keeps the secret")
	contains(t, export.Body, `sitemap = "http://localhost:8080/wp-sitemap.xml"`, "and the settings which weren't changed")
}

func TestAdminCacheBrowser(t *testing.T) {
	c := client(nil)
	_, path := newPost(t, "Browsed on the admin page")
	cached(t, c, path)
	get(t, c, path)

	list := get(t, c, adminPage+"/cache?"+url.Values{"path": {path}}.Encode())
	require.Equal(t, http.StatusOK, list.StatusCode)
	contains(t, list.Body, `class="mono path">`+path+`<`)
	// (not its comments, which are under it: see TestCommentsAreEdgeSideIncludes)
	link := regexp.MustCompile(`href="(/cache/entry\?key=[^"]+)" class="mono path">` + regexp.QuoteMeta(path) + `<`).FindStringSubmatch(list.Body)
	require.Len(t, link, 2, "the post links to its details")

	entry := get(t, c, adminPage+html.UnescapeString(link[1]))
	require.Equal(t, http.StatusOK, entry.StatusCode)
	contains(t, entry.Body, "Browsed on the admin page", "its body is shown")
	contains(t, entry.Body, "Content-Type: text/html")

	resp := do(t, c, http.MethodPost, adminPage+"/purge",
		url.Values{"url": {site + path}, "prefix": {"1"}, "return": {"/cache?path=" + url.QueryEscape(path)}}, "Origin", adminPage)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.True(t, strings.HasPrefix(resp.Header.Get("Location"), "/cache?"), "the purge returns to the cache browser")
	contains(t, get(t, c, adminPage+resp.Header.Get("Location")).Body, "Nothing cached matches.")
}

// newPage publishes a page with content, and returns its path
func newPage(t *testing.T, title, content string) string {
	t.Helper()
	slug := fmt.Sprintf("e2e-page-%d-%d", time.Now().UnixNano(), posts.Add(1))
	wp(t, "post", "create", "--post_type=page", "--post_status=publish", "--post_name="+slug,
		"--post_title="+title, "--post_content="+content)
	return "/" + slug + "/"
}

func TestSharedFragments(t *testing.T) {
	c := client(nil)
	// The same Latest Posts block on two pages
	block := `<!-- wp:latest-posts {"postsToShow":3} /-->`
	first := newPage(t, "First page with recent posts", block)
	second := newPage(t, "Second page with recent posts", block)
	cached(t, c, first)
	resp := get(t, c, first)
	contains(t, resp.Body, `<!--honey:fragment name="posts-block-`, "the block is marked as a shared fragment: %s", describe(resp))
	notContains(t, resp.Body, "token=", "without the token Honey sent")
	time.Sleep(1500 * time.Millisecond)
	// The second page is the last one seen with the fragment, so it's the
	// one Honey refreshes
	cached(t, c, second)

	title := fmt.Sprintf("Fragment test post %d", time.Now().UnixNano())
	newPost(t, title)
	resp = eventually(t, c, first, title)
	resp = get(t, c, first)
	assert.Equal(t, "HIT", resp.Cache, "the first page wasn't fetched again: %s", describe(resp))
	age, _ := strconv.Atoi(resp.Header.Get("Age"))
	assert.GreaterOrEqual(t, age, 2, "it's the copy cached before the post was published, with the new list")
	contains(t, resp.Body, "First page with recent posts")
	contains(t, get(t, c, second).Body, title)

	admin := get(t, c, adminPage+"/cache")
	contains(t, admin.Body, `class="mono">posts-block-`, "the admin page lists the fragment")
}

func TestEarlyHints(t *testing.T) {
	_, path := newPost(t, "Hinted post")
	anonymous := client(nil)
	cached(t, anonymous, path)

	// The admin waits for WordPress, so is sent the page's stylesheets first
	admin := client(logIn(t, "admin"))
	var hints []string
	trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
		if code == http.StatusEarlyHints {
			hints = append(hints, header.Values("Link")...)
		}
		return nil
	}}
	req, err := http.NewRequest(http.MethodGet, site+path, nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "text/html")
	resp, err := admin.Do(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	require.NoError(t, err)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	assert.Equal(t, "NO-CACHE", resp.Header.Get("X-Honey-Cache"))
	require.NotEmpty(t, hints, "Early Hints were sent")
	assert.Contains(t, strings.Join(hints, "\n"), "rel=preload; as=style", "for the theme's stylesheets")
	// WordPress sends Link headers of its own (e.g. for the REST API)
	notContains(t, strings.Join(resp.Header.Values("Link"), "\n"), "rel=preload", "they're only in the 103")

	// The cache browser shows them
	list := get(t, anonymous, adminPage+"/cache?"+url.Values{"path": {path}}.Encode())
	link := regexp.MustCompile(`href="(/cache/entry\?key=[^"]+)" class="mono path">` + regexp.QuoteMeta(path) + `<`).FindStringSubmatch(list.Body)
	require.Len(t, link, 2)
	contains(t, get(t, anonymous, adminPage+html.UnescapeString(link[1])).Body, "Early Hints")
}
