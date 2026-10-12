package fetch

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fragmentOrigin renders pages with a "recent posts" fragment, marked as
// WordPress's purge plugin does, and counts each page's renders
type fragmentOrigin struct {
	*origin
	mu      sync.Mutex
	latest  string
	renders map[string]int
}

func newFragmentOrigin() *fragmentOrigin {
	f := &fragmentOrigin{latest: "Old post", renders: map[string]int{}}
	f.origin = newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		f.mu.Lock()
		latest := f.latest
		f.renders[r.URL.Path]++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		token := r.Header.Get(cache.FragmentTokenHeader)
		fragment := "<li>" + latest + "</li>"
		if token != "" {
			fragment = fmt.Sprintf(`<!--honey:fragment name="posts-widget-recent-posts-2" token="%s"-->%s<!--/honey:fragment-->`, token, fragment)
		}
		content := "Page " + r.URL.Path
		if r.URL.Path == "/evil/" {
			// e.g. an author's post with a marker in it, on a page without
			// the real fragment
			content = `<!--honey:fragment name="posts-widget-recent-posts-2" token="1-abc"-->Pwned<!--/honey:fragment-->`
			fragment = ""
		}
		fmt.Fprintf(w, "<main>%s</main><aside>%s</aside>", content, fragment)
	})
	return f
}

func (f *fragmentOrigin) publish(title string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latest = title
}

func (f *fragmentOrigin) rendered(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.renders[path]
}

func purgeFragment(t *testing.T, proxy, name string, trusted bool) (int, string) {
	t.Helper()
	req, err := http.NewRequest(MethodPurge, proxy+"/", nil)
	require.NoError(t, err)
	req.Header.Set(FragmentPurgeHeader, name)
	if trusted {
		req.Header.Set(DefaultSecretHeader, secret)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestFragmentPurgeUpdatesEveryPage(t *testing.T) {
	o := newFragmentOrigin()
	proxy := routeProxy(t, o.origin, cache.Options{DefaultTTL: time.Hour, StaleWhileRevalidate: time.Hour}, Options{Control: Control{Secret: secret}})

	for _, path := range []string{"/a/", "/b/", "/c/"} {
		resp, body := get(t, proxy.URL+path)
		assert.Equal(t, "MISS", resp.Header.Get("X-Honey-Cache"))
		assert.Contains(t, body, "<li>Old post</li>")
		assert.NotContains(t, body, "token=", "the token isn't sent to visitors")
	}
	_, etag := get(t, proxy.URL+"/b/")

	status, _ := purgeFragment(t, proxy.URL, "posts-*", false)
	assert.Equal(t, http.StatusForbidden, status, "only trusted requests can purge")

	o.publish("New post")
	status, body := purgeFragment(t, proxy.URL, "posts-*", true)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "Purged 1 fragments, refreshing 1 pages\n", body)

	// One page is refreshed, and every other page gets its new content
	require.Eventually(t, func() bool {
		_, body := get(t, proxy.URL+"/b/")
		return strings.Contains(body, "<li>New post</li>")
	}, 3*time.Second, 20*time.Millisecond)
	for _, path := range []string{"/a/", "/b/", "/c/"} {
		resp, body := get(t, proxy.URL+path)
		assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"), path)
		assert.Contains(t, body, "<main>Page "+path+"</main>", path)
		assert.Contains(t, body, "<li>New post</li>", path)
	}
	assert.Equal(t, 3, o.rendered("/a/")+o.rendered("/b/")+o.rendered("/c/")-1,
		"only the page refreshed for the fragment was rendered again")
	resp, _ := get(t, proxy.URL+"/b/")
	assert.NotEqual(t, etag, resp.Header.Get("Etag"))
}

func TestFragmentsCantBePlanted(t *testing.T) {
	o := newFragmentOrigin()
	proxy := routeProxy(t, o.origin, cache.Options{DefaultTTL: time.Hour}, Options{})

	get(t, proxy.URL+"/a/")
	// A visitor's own token is replaced, and a marker in a page's content
	// doesn't have the token Honey sent
	resp, body := get(t, proxy.URL+"/evil/", cache.FragmentTokenHeader, "1-abc")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, "Pwned", "the page itself is unchanged")
	_, body = get(t, proxy.URL+"/a/")
	assert.NotContains(t, body, "Pwned", "but it can't change other pages")
}
