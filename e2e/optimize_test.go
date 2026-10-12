//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newPostWith publishes a post with content (as the admin, who may add
// scripts), and returns its path
func newPostWith(t *testing.T, title, content string) string {
	t.Helper()
	slug := fmt.Sprintf("e2e-%d-%d", time.Now().UnixNano(), posts.Add(1))
	wp(t, "post", "create", "--porcelain", "--user=admin", "--post_status=publish", "--post_name="+slug,
		"--post_title="+title, "--post_content="+content)
	return "/" + slug + "/"
}

func TestThirdPartyScriptsAreDelayed(t *testing.T) {
	path := newPostWith(t, "Tracked post", `<!-- wp:html -->
<script async src="https://www.googletagmanager.com/gtag/js?id=G-E2E"></script>
<script id="honey-e2e-delayed">window.honeyDelayedRan = (window.honeyDelayedRan || 0) + 1;</script>
<script id="honey-e2e-kept">window.honeyKeptRan = true;</script>
<!-- /wp:html -->`)
	page := cached(t, client(nil), path)
	assert.Contains(t, page.Body, `<script type="text/honey-delayed" data-honey-src="https://www.googletagmanager.com/gtag/js?id=G-E2E" async></script>`,
		"Google's tag waits for the visitor")
	assert.Contains(t, page.Body, `<script type="text/honey-delayed" id="honey-e2e-delayed">`, "as do scripts matching optimize.delayScriptsMatching")
	assert.Contains(t, page.Body, `<script id="honey-e2e-kept">`, "but not the site's own")
	assert.Equal(t, 1, strings.Count(page.Body, `<script id="honey-delay">`))

	// Logged in users get WordPress's page as it is
	admin := get(t, client(logIn(t, "admin")), path)
	assert.Equal(t, "NO-CACHE", admin.Cache)
	notContains(t, admin.Body, "honey-delayed")
}

func TestGoogleFontsAreSelfHosted(t *testing.T) {
	path := newPostWith(t, "Fancy post", `<!-- wp:html -->
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Lobster&display=swap">
<!-- /wp:html -->`)
	page := cached(t, client(nil), path)
	assert.Contains(t, page.Body, `<link href="/.well-known/honey/fonts/css2?family=Lobster&amp;display=swap" rel="stylesheet">`)
	notContains(t, page.Body, "fonts.googleapis.com/css2?family=Lobster")
	// (Fetching it from Google is left to the unit tests, so that these
	// don't depend on Google)
	resp := get(t, client(nil), "/.well-known/honey/fonts/s/../../wp-config.php")
	assert.NotEqual(t, http.StatusOK, resp.StatusCode)
}

func TestMainImagesArePreloaded(t *testing.T) {
	image := "/wp-includes/images/w-logo-blue.png"
	path := newPostWith(t, "Pictured post", `<!-- wp:image -->
<figure class="wp-block-image"><img src="`+image+`" alt="WordPress"/></figure>
<!-- /wp:image -->`)
	c := client(nil)
	page := cached(t, c, path)
	assert.Contains(t, page.Body, `<script id="honey-main-image">`, "browsers are asked for the main image")
	notContains(t, page.Body, "data-honey-main-image")

	report := func(viewport, image string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, site+"/.well-known/honey/main-image",
			strings.NewReader(`{"p":"`+path+`","v":"`+viewport+`","u":"`+site+image+`"}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
		resp, err := c.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	}
	report("desktop", "/wp-content/uploads/not-on-the-page.jpg")
	report("desktop", image)
	report("mobile", image)

	// The page is cached again, with the image loaded first (an <img> on
	// every screen isn't preloaded: the browser finds it straight away)
	eventually(t, c, path, `fetchpriority="high"`)
	page = cached(t, c, path)
	assert.Regexp(t, `<img fetchpriority="high"[^>]* src="`+regexp.QuoteMeta(image)+`"`, page.Body)
	notContains(t, page.Body, "data-honey-main-image")
	notContains(t, page.Body, `<script id="honey-main-image">`, "nothing left to report")

	// It's in the admin page's events
	events := get(t, c, adminPage+"/")
	assert.Contains(t, events.Body, "learned the main image")
}
