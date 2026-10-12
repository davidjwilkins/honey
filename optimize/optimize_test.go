package optimize

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wpPage is a WordPress page with the usual third-party scripts
const wpPage = `<!DOCTYPE html>
<html lang="en-US"><head>
<meta charset="UTF-8" />
<title>Hello</title>
<link rel='stylesheet' href='/wp-content/themes/t/style.css' />
<script src="/wp-includes/js/jquery/jquery.min.js" id="jquery-core-js"></script>
<!-- Google tag (gtag.js) -->
<script async src="https://www.googletagmanager.com/gtag/js?id=G-ABC"></script>
<script>
  window.dataLayer = window.dataLayer || [];
  function gtag(){dataLayer.push(arguments);}
  gtag('config', 'G-ABC');
</script>
<script type="application/ld+json">{"@context":"https://schema.org","url":"https://connect.facebook.net/"}</script>
<!--[if lt IE 9]><script src="https://connect.facebook.net/old.js"></script><![endif]-->
</head>
<body class="home">
<header><img src="/logo.png" width="40" height="40" alt=""></header>
<main>
<div class="wp-block-cover" style="background-image:url(&quot;/wp-content/uploads/hero.jpg&quot;)"></div>
<img src="/wp-content/uploads/featured-1024.jpg" srcset="/wp-content/uploads/featured-1024.jpg 1024w, /wp-content/uploads/featured-300.jpg 300w" sizes="(max-width: 1024px) 100vw, 1024px" loading="lazy" decoding="async" alt="">
<p>Text</p>
<noscript><img src="/wp-content/uploads/noscript.jpg"></noscript>
</main>
<script type="text/javascript" id="fb-pixel" data-cfasync="false">!function(f,b,e,v,n,t,s){n=f.fbq=function(){};t=b.createElement(e);t.src=v}(window,document,'script','https://connect.facebook.net/en_US/fbevents.js');</script>
<script src="https://widget.intercom.io/widget/abc" async defer data-honey-keep></script>
<script src="/wp-content/plugins/contact/form.js" id="contact-js"></script>
</body>
</html>`

func response(path, body string) (*http.Response, []byte) {
	r := httptest.NewRequest(http.MethodGet, "http://www.example.com"+path, nil)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/html; charset=UTF-8"}}, Request: r}, []byte(body)
}

func TestDelayScripts(t *testing.T) {
	o := New(Options{DelayScripts: true, DelayMatching: []string{"contact/form.js"}, KeepMatching: []string{"gtag/js"}})
	out := string(o.Rewrite(response("/", wpPage)))

	assert.Contains(t, out, `<script src="/wp-includes/js/jquery/jquery.min.js" id="jquery-core-js"></script>`, "the site's own scripts are left alone")
	assert.Contains(t, out, `<script async src="https://www.googletagmanager.com/gtag/js?id=G-ABC"></script>`, "keepScriptsMatching wins")
	assert.Contains(t, out, `<script type="text/honey-delayed">
  window.dataLayer`, "inline scripts calling gtag( are delayed")
	assert.Contains(t, out, `<script type="application/ld+json">{"@context"`, "data isn't a script")
	assert.Contains(t, out, `<!--[if lt IE 9]><script src="https://connect.facebook.net/old.js"></script><![endif]-->`, "nor are comments")
	assert.Contains(t, out, `<script type="text/honey-delayed" data-honey-type="text/javascript" id="fb-pixel" data-cfasync="false">!function(f,b,e,v`, "the pixel is delayed, keeping its type and other attributes")
	assert.Contains(t, out, `<script src="https://widget.intercom.io/widget/abc" async defer data-honey-keep></script>`, "data-honey-keep keeps a script")
	assert.Contains(t, out, `<script type="text/honey-delayed" data-honey-src="/wp-content/plugins/contact/form.js" id="contact-js"></script>`, "delayScriptsMatching adds to the list")
	assert.Equal(t, 1, strings.Count(out, `<script id="honey-delay">`))
	assert.Less(t, strings.Index(out, `<script id="honey-delay">`), strings.LastIndex(out, "</body>"))
	assert.Contains(t, out, "var T=0,", "only on interaction, by default")

	o = New(Options{DelayScripts: true, DelayTimeout: 5 * time.Second})
	assert.Contains(t, string(o.Rewrite(response("/", wpPage))), "var T=5000,")

	plain := `<html><head></head><body><script src="/app.js"></script></body></html>`
	assert.Equal(t, plain, string(o.Rewrite(response("/", plain))), "nothing to delay: no loader")
}

func TestOnlyWholePagesAreRewritten(t *testing.T) {
	o := New(Options{DelayScripts: true})
	for name, change := range map[string]func(r *http.Response) string{
		"a fragment": func(r *http.Response) string { return `<script src="https://connect.facebook.net/x.js"></script>` },
		"an include": func(r *http.Response) string { r.Request.URL.RawQuery = "honey-esi=comments.1"; return wpPage },
		"not HTML":   func(r *http.Response) string { r.Header.Set("Content-Type", "application/json"); return wpPage },
		"an error":   func(r *http.Response) string { r.StatusCode = http.StatusNotFound; return wpPage },
		"compressed": func(r *http.Response) string { r.Header.Set("Content-Encoding", "gzip"); return wpPage },
		"with its CSP": func(r *http.Response) string {
			r.Header.Set("Content-Security-Policy", "script-src 'self'")
			return wpPage
		},
		"a HEAD": func(r *http.Response) string {
			r.Request.Method = http.MethodHead
			return wpPage
		},
	} {
		r, _ := response("/", "")
		body := change(r)
		assert.Equal(t, body, string(o.Rewrite(r, []byte(body))), name)
	}
	assert.Nil(t, New(Options{}), "nothing on")
	var off *Optimizer
	assert.Equal(t, wpPage, string(off.Rewrite(response("/", wpPage))))
}

// report sends a browser's report of path's main image
func report(t *testing.T, h http.Handler, path, viewport, image string) int {
	t.Helper()
	body := `{"p":"` + path + `","v":"` + viewport + `","u":"` + image + `"}`
	r := httptest.NewRequest(http.MethodPost, "http://www.example.com"+BeaconPath, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

func TestPreloadMainImage(t *testing.T) {
	var refreshed []string
	log := events.New(0)
	o := New(Options{PreloadMainImage: true, Events: log, Refresh: func(host, path string) { refreshed = append(refreshed, host+path) }})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "page") })
	h := o.Middleware(next)

	out := string(o.Rewrite(response("/post/", wpPage)))
	assert.NotContains(t, out, "data-honey-main-image", "not known yet")
	assert.Contains(t, out, `<script id="honey-main-image">(function(){var n=["mobile","desktop"],`, "so browsers report it")

	assert.Equal(t, http.StatusNoContent, report(t, h, "/post/", "desktop", "http://www.example.com/wp-content/uploads/elsewhere.jpg"))
	assert.Empty(t, refreshed, "only the page's own images are believed")
	assert.Equal(t, http.StatusNoContent, report(t, h, "/other/", "desktop", "http://www.example.com/wp-content/uploads/featured-1024.jpg"))
	assert.Empty(t, refreshed, "only for pages which have asked")
	assert.Equal(t, http.StatusNoContent, report(t, h, "/post/", "desktop", "http://www.example.com/wp-content/uploads/noscript.jpg"))
	assert.Empty(t, refreshed, "nor images in <noscript>")
	assert.Equal(t, http.StatusBadRequest, report(t, h, "/post/", "watch", "/wp-content/uploads/featured-1024.jpg"))

	assert.Equal(t, http.StatusNoContent, report(t, h, "/post/", "desktop", "http://www.example.com/wp-content/uploads/featured-1024.jpg"))
	assert.Equal(t, []string{"www.example.com/post/"}, refreshed, "it's cached again with the preload")
	assert.Equal(t, http.StatusNoContent, report(t, h, "/post/", "desktop", "http://www.example.com/wp-content/uploads/featured-300.jpg"))
	assert.Len(t, refreshed, 1, "once it's known, it stays known")
	require.NotEmpty(t, log.Recent(1))
	assert.Contains(t, log.Recent(1)[0].Text, "learned the main image of /post/ on desktop screens")

	out = string(o.Rewrite(response("/post/", wpPage)))
	assert.Contains(t, out, `<meta charset="UTF-8" /><link rel="preload" as="image" imagesrcset="/wp-content/uploads/featured-1024.jpg 1024w, /wp-content/uploads/featured-300.jpg 300w" imagesizes="(max-width: 1024px) 100vw, 1024px" fetchpriority="high" media="(min-width: 768px)" data-honey-main-image>`,
		"on desktops, at the top of the <head>, without an href, which browsers would fetch as well")
	assert.Contains(t, out, `loading="lazy"`, "it may not be the main image on phones")
	assert.Contains(t, out, `var n=["mobile"]`, "phones are still to report")

	// Phones say it's the cover's background
	assert.Equal(t, http.StatusNoContent, report(t, h, "/post/", "mobile", "http://www.example.com/wp-content/uploads/hero.jpg"))
	out = string(o.Rewrite(response("/post/", wpPage)))
	assert.Contains(t, out, `<link rel="preload" as="image" href="/wp-content/uploads/hero.jpg" fetchpriority="high" media="(max-width: 767.98px)" data-honey-main-image>`)
	assert.NotContains(t, out, "honey-main-image\">", "nothing left to report")

	// Both say the featured image
	o.images.forget(pageKey("www.example.com", "/post/"), 0)
	o.Rewrite(response("/post/", wpPage))
	report(t, h, "/post/", "mobile", "http://www.example.com/wp-content/uploads/featured-300.jpg")
	out = string(o.Rewrite(response("/post/", wpPage)))
	assert.NotContains(t, out, "data-honey-main-image", "the browser finds it as soon as it reads it, so a preload would only compete with the stylesheets")
	assert.Contains(t, out, `<img fetchpriority="high" src="/wp-content/uploads/featured-1024.jpg" srcset=`, "but it isn't lazy loaded")
	assert.NotContains(t, out, `loading="lazy"`)
	assert.Contains(t, out, `<img src="/logo.png" width="40" height="40" alt="">`)

	// The page changes: it's learned again
	changed := strings.Replace(wpPage, "featured-", "new-", -1)
	out = string(o.Rewrite(response("/post/", changed)))
	assert.NotContains(t, out, "data-honey-main-image")
	assert.Contains(t, out, `var n=["mobile","desktop"]`)

	assert.Equal(t, "page", func() string {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/post/", nil))
		return w.Body.String()
	}(), "other requests pass through")
}

func TestPreloadingLazyLoadedImages(t *testing.T) {
	// A lazy loading plugin's image only starts loading once its script
	// has run, so it's preloaded, for every screen
	page := strings.Replace(wpPage, `<img src="/wp-content/uploads/featured-1024.jpg" srcset=`, `<img src="data:image/gif;base64,R0lGOD" data-src="/wp-content/uploads/featured-1024.jpg" data-srcset=`, 1)
	page = strings.Replace(page, ` sizes="(max-width: 1024px)`, ` data-sizes="(max-width: 1024px)`, 1)
	o := New(Options{PreloadMainImage: true})
	h := o.Middleware(http.NotFoundHandler())
	o.Rewrite(response("/lazy/", page))
	report(t, h, "/lazy/", "mobile", "http://www.example.com/wp-content/uploads/featured-300.jpg")
	report(t, h, "/lazy/", "desktop", "http://www.example.com/wp-content/uploads/featured-1024.jpg")
	out := string(o.Rewrite(response("/lazy/", page)))
	assert.Contains(t, out, `<meta charset="UTF-8" /><link rel="preload" as="image" imagesrcset="/wp-content/uploads/featured-1024.jpg 1024w, /wp-content/uploads/featured-300.jpg 300w" imagesizes="(max-width: 1024px) 100vw, 1024px" fetchpriority="high" data-honey-main-image>`)
	assert.Contains(t, out, `src="data:image/gif;base64,R0lGOD"`, "the plugin's image is left to it")
}

func TestPreloadsComeAfterTheViewport(t *testing.T) {
	// Before it, a phone's browser would pick a desktop's size from the
	// srcset
	page := strings.Replace(wpPage, `<meta charset="UTF-8" />`, `<meta charset="UTF-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />`, 1)
	o := New(Options{PreloadMainImage: true})
	h := o.Middleware(http.NotFoundHandler())
	o.Rewrite(response("/v/", page))
	report(t, h, "/v/", "mobile", "http://www.example.com/wp-content/uploads/hero.jpg")
	out := string(o.Rewrite(response("/v/", page)))
	assert.Contains(t, out, `<meta name="viewport" content="width=device-width, initial-scale=1" /><link rel="preload" as="image" href="/wp-content/uploads/hero.jpg"`)
}

func TestPagesWithoutAnImage(t *testing.T) {
	var refreshed int
	o := New(Options{PreloadMainImage: true, Refresh: func(string, string) { refreshed++ }})
	h := o.Middleware(http.NotFoundHandler())
	o.Rewrite(response("/text/", wpPage))
	report(t, h, "/text/", "mobile", "")
	assert.Zero(t, refreshed, "nothing to preload, and desktops still to report")
	report(t, h, "/text/", "desktop", "")
	assert.Equal(t, 1, refreshed, "the reporting script can go")
	out := string(o.Rewrite(response("/text/", wpPage)))
	assert.NotContains(t, out, "honey-main-image")
	assert.Equal(t, wpPage, out)
}

func TestImageStoreIsBounded(t *testing.T) {
	s := newImageStore(3)
	for _, k := range []string{"a", "b", "c", "d"} {
		s.expect(k, map[uint64]bool{})
	}
	assert.Len(t, s.pages, 3)
}

func TestNormalize(t *testing.T) {
	for raw, want := range map[string]string{
		"http://www.example.com/a.jpg?x=1#y": "/a.jpg?x=1",
		"https://WWW.example.com/a.jpg":      "/a.jpg",
		"/a.jpg":                             "/a.jpg",
		"https://CDN.example.net/a.jpg":      "https://cdn.example.net/a.jpg",
		"data:image/gif;base64,R0lGOD":       "",
		"javascript:alert(1)":                "",
		"":                                   "",
	} {
		assert.Equal(t, want, normalize(raw, "www.example.com", nil), raw)
	}
}
