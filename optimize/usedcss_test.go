package optimize

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const themeCSS = `@charset "UTF-8";
:root { --accent: #c00 }
html, body { margin: 0 }
.site-header { background: url(images/header.png) }
.site-header .menu a:hover { color: var(--accent) }
.site-footer { padding: 2em }
.button:focus-visible::after { outline: 2px solid }
#main > p::first-line { font-weight: bold }
.unused, .entry-title { font-size: 2em }
.js .menu-toggle { display: block }
.md\:flex { display: flex }
input::placeholder { color: grey }
a:not(.external) { text-decoration: none }
.modal.is-open { display: block }
@media (min-width: 768px) {
  .site-footer { padding: 4em }
  .site-header { height: 80px }
}
@media print { .site-header { display: none } }
@supports (display: grid) { .gallery { display: grid } }
@font-face { font-family: Theme; src: url("../fonts/theme.woff2") format("woff2") }
@keyframes fade { from { opacity: 0 } to { opacity: 1 } }
@keyframes spin { to { transform: rotate(1turn) } }
.entry-title { animation: fade 1s }
`

const cssPage = `<!DOCTYPE html><html class="no-js"><head><meta charset="utf-8">
<link rel='stylesheet' id='theme-css' href='/wp-content/themes/t/style.css?ver=1.2' media='all' />
<link rel="stylesheet" href="https://cdn.other.com/lib.css">
<link rel="stylesheet" href="/wp-content/themes/t/print.css" media="print">
</head><body>
<header class="site-header"><nav class="menu"><a href="/">Home</a></nav><button class="menu-toggle">Menu</button></header>
<main id="main"><h1 class="entry-title">Hello</h1><p>Text <input placeholder="Search"></p><div class="md:flex"></div></main>
</body></html>`

// backend serves stylesheets, and counts the requests for them
type backend struct {
	mu       sync.Mutex
	sheets   map[string]string
	requests []string
	hosts    []string
	// slow, if set, holds requests up until it's closed
	slow chan struct{}
}

func (b *backend) fetch(r *http.Request) (*http.Response, error) {
	if b.slow != nil {
		<-b.slow
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests = append(b.requests, r.URL.RequestURI())
	b.hosts = append(b.hosts, r.Host)
	css, ok := b.sheets[r.URL.Path]
	if !ok {
		return &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found", Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": {"text/css"}}, Body: io.NopCloser(strings.NewReader(css))}, nil
}

// cssOptimizer returns an Optimizer which removes unused CSS, and a channel
// of the pages it refreshes
func cssOptimizer(t *testing.T, b *backend) (*Optimizer, chan string) {
	refreshed := make(chan string, 10)
	o := New(Options{RemoveUnusedCSS: true, Fetch: b.fetch, Refresh: func(host, path string) { refreshed <- host + path }})
	require.NotNil(t, o)
	return o, refreshed
}

func notRefreshed(t *testing.T, refreshed chan string) {
	t.Helper()
	select {
	case page := <-refreshed:
		t.Fatalf("%s was refreshed", page)
	case <-time.After(50 * time.Millisecond):
	}
}

func waitFor(t *testing.T, refreshed chan string) string {
	select {
	case page := <-refreshed:
		return page
	case <-time.After(5 * time.Second):
		t.Fatal("the page wasn't refreshed")
		return ""
	}
}

func TestRemoveUnusedCSS(t *testing.T) {
	b := &backend{sheets: map[string]string{"/wp-content/themes/t/style.css": themeCSS}}
	o, refreshed := cssOptimizer(t, b)

	out := string(o.Rewrite(response("/hello/", cssPage)))
	assert.Equal(t, []string{"/wp-content/themes/t/style.css?ver=1.2"}, b.requests, "only the site's own stylesheet is fetched, as the page waits")
	assert.Equal(t, []string{"www.example.com"}, b.hosts)
	notRefreshed(t, refreshed)

	start := strings.Index(out, `<style data-honey-used-css="/wp-content/themes/t/style.css?ver=1.2">`)
	require.GreaterOrEqual(t, start, 0, out)
	end := strings.Index(out[start:], "</style>")
	used := out[start+len(`<style data-honey-used-css="/wp-content/themes/t/style.css?ver=1.2">`) : start+end]

	for _, kept := range []string{
		":root{--accent: #c00}",
		"html, body{margin: 0}",
		`.site-header{background: url("/wp-content/themes/t/images/header.png")}`,
		".site-header .menu a:hover{color: var(--accent)}",
		"#main > p::first-line{font-weight: bold}",
		".unused, .entry-title{font-size: 2em}",
		".js .menu-toggle{display: block}",
		`.md\:flex{display: flex}`,
		"input::placeholder{color: grey}",
		"a:not(.external){text-decoration: none}",
		"@media (min-width: 768px){.site-header{height: 80px}}",
		`@font-face{font-family: Theme; src: url("/wp-content/themes/fonts/theme.woff2") format("woff2")}`,
		"@keyframes fade{",
		".entry-title{animation: fade 1s}",
	} {
		assert.Contains(t, used, kept)
	}
	for _, removed := range []string{"charset", ".site-footer", ".button", ".modal", "print", ".gallery", "spin"} {
		assert.NotContains(t, used, removed)
	}

	after := out[start+end+len("</style>"):]
	assert.True(t, strings.HasPrefix(after, `<link rel='stylesheet' id='theme-css' href='/wp-content/themes/t/style.css?ver=1.2' media="print" onload="this.onload=null;this.media='all'">`+
		`<noscript><link rel='stylesheet' id='theme-css' href='/wp-content/themes/t/style.css?ver=1.2' media='all' /></noscript>`), after)
	assert.Contains(t, out, `<link rel="stylesheet" href="https://cdn.other.com/lib.css">`, "other sites' stylesheets are left alone")
	assert.Contains(t, out, `<link rel="stylesheet" href="/wp-content/themes/t/print.css" media="print">`, "as are print stylesheets")

	o.Rewrite(response("/other/", cssPage))
	assert.Len(t, b.requests, 1, "stylesheets are kept")
	notRefreshed(t, refreshed)
}

func TestSlowStylesheetsRefreshThePage(t *testing.T) {
	b := &backend{sheets: map[string]string{"/s.css": "p { color: red } .x { color: blue }"}, slow: make(chan struct{})}
	refreshed := make(chan string, 10)
	o := New(Options{RemoveUnusedCSS: true, Fetch: b.fetch, Refresh: func(host, path string) { refreshed <- path },
		stylesheetWait: 20 * time.Millisecond})
	page := `<html><head><link rel="stylesheet" href="/s.css"></head><body><p>Hi</p></body></html>`
	assert.Equal(t, page, string(o.Rewrite(response("/", page))), "it's cached as it is, rather than wait")
	assert.Equal(t, page, string(o.Rewrite(response("/", page))))
	close(b.slow)
	assert.Equal(t, "/", waitFor(t, refreshed), "and refreshed once its stylesheet is fetched")
	notRefreshed(t, refreshed)
	assert.Contains(t, string(o.Rewrite(response("/", page))), `<style data-honey-used-css="/s.css">p{color: red}</style>`)
}

func TestUnusableStylesheets(t *testing.T) {
	b := &backend{sheets: map[string]string{
		"/import.css": "@import url(other.css); a { color: red }",
		"/ok.css":     "p { color: red } .x { color: blue }",
	}}
	o, refreshed := cssOptimizer(t, b)
	page := `<html><head><link rel="stylesheet" href="/import.css"><link rel="stylesheet" href="/missing.css"><link rel="stylesheet" href="/ok.css"></head><body><p>Hi</p></body></html>`
	out := string(o.Rewrite(response("/", page)))
	assert.Contains(t, out, `<link rel="stylesheet" href="/import.css"><link rel="stylesheet" href="/missing.css">`)
	assert.Contains(t, out, `<style data-honey-used-css="/ok.css">p{color: red}</style>`)

	only := `<html><head><link rel="stylesheet" href="/missing.css"></head><body></body></html>`
	o.Rewrite(response("/missing/", only))
	notRefreshed(t, refreshed)
}

func TestStylesheetsAreFetchedAgain(t *testing.T) {
	b := &backend{sheets: map[string]string{"/s.css": "p { color: red }"}}
	clock := &clock{t: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)}
	refreshed := make(chan string, 10)
	o := New(Options{RemoveUnusedCSS: true, Fetch: b.fetch, Refresh: func(host, path string) { refreshed <- path }, now: clock.now})
	page := `<html><head><link rel="stylesheet" href="/s.css"></head><body><p>Hi</p></body></html>`
	o.Rewrite(response("/", page))
	b.mu.Lock()
	b.sheets["/s.css"] = "p { color: blue }"
	b.mu.Unlock()
	clock.t = clock.t.Add(2 * time.Hour)
	assert.Contains(t, string(o.Rewrite(response("/", page))), "p{color: red}", "the old one's used while it's fetched again")
	require.Eventually(t, func() bool {
		return strings.Contains(string(o.Rewrite(response("/", page))), "p{color: blue}")
	}, 5*time.Second, 10*time.Millisecond)
}

func TestUsedCSSStaysInItsStyle(t *testing.T) {
	b := &backend{sheets: map[string]string{"/s.css": `p::after { content: "</STYLE><script>alert(1)</script>" }`}}
	o, _ := cssOptimizer(t, b)
	page := `<html><head><link rel="stylesheet" href="/s.css"></head><body><p>Hi</p></body></html>`
	assert.Contains(t, string(o.Rewrite(response("/", page))), `p::after{content: "<\/STYLE><script>alert(1)</script>"}</style>`)
}

func TestStylesheetsAreBounded(t *testing.T) {
	s := newCSSStore(&Options{now: time.Now})
	for i := 0; i < 10; i++ {
		s.sheets[strings.Repeat("x", i)] = &stylesheet{rules: []usedRule{}, size: maxStylesheetBytes / 4}
		s.bytes += maxStylesheetBytes / 4
	}
	s.sheets["fetching"] = &stylesheet{fetching: true}
	s.evict(maxStylesheetBytes)
	assert.LessOrEqual(t, s.bytes, maxStylesheetBytes)
	assert.Len(t, s.sheets, 5, "4 of a quarter each, and the one being fetched")
	assert.NotNil(t, s.sheets["fetching"])
}

func TestUsedCSSIsBounded(t *testing.T) {
	big := strings.Repeat("p { color: red; background: blue; border: 1px solid green; margin: 0 }\n", maxUsedCSS/100)
	b := &backend{sheets: map[string]string{"/a.css": big, "/b.css": big}}
	o, _ := cssOptimizer(t, b)
	page := `<html><head><link rel="stylesheet" href="/a.css"><link rel="stylesheet" href="/b.css"></head><body><p>Hi</p></body></html>`
	out := string(o.Rewrite(response("/", page)))
	assert.Contains(t, out, `<style data-honey-used-css="/a.css">`)
	assert.Contains(t, out, `<link rel="stylesheet" href="/b.css"></head>`, "too much used CSS: the rest are left alone")
}

func TestWithoutStates(t *testing.T) {
	for sel, want := range map[string]string{
		"a:hover":                          "a",
		":hover":                           "*",
		"a :focus":                         "a *",
		".menu > li:hover > a":             ".menu > li > a",
		".x::before":                       ".x",
		".x:after, :focus-within":          ".x, *",
		"p:first-child":                    "p:first-child",
		"li:nth-child(2n+1):hover":         "li:nth-child(2n+1)",
		"input::-webkit-input-placeholder": "input",
		`a[title=":hover"]`:                `a[title=":hover"]`,
		"::selection":                      "*",
		".a:hover::after .b":               ".a .b",
		"button:not(:disabled)":            "button:not(:disabled)",
		`.md\:hover`:                       `.md\:hover`,
	} {
		assert.Equal(t, want, withoutStates(sel), sel)
	}
}

func TestRequiredNames(t *testing.T) {
	classes, ids := requiredNames(`#main .a.b > p:not(.c) :is(.d, .e) [class~="f"] .md\:flex .\31 0`)
	assert.Equal(t, []string{"a", "b", "md:flex"}, classes)
	assert.Equal(t, []string{"main"}, ids)
}

func TestAbsoluteURLs(t *testing.T) {
	assert.Equal(t,
		`a{background:url("/wp-content/themes/t/img/a.png")} b{background:url("/wp-content/themes/img/b.png")} c{background:url(/c.png)} d{background:url(data:image/png;base64,xx)} e{filter:url(#f)} f{background:url("https://x.com/f.png")}`,
		absoluteURLs(`a{background:url(img/a.png)} b{background:url('../img/b.png')} c{background:url(/c.png)} d{background:url(data:image/png;base64,xx)} e{filter:url(#f)} f{background:url("https://x.com/f.png")}`, "/wp-content/themes/t/style.css?ver=1"))
}
