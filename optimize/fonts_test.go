package optimize

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fontsPage = `<!DOCTYPE html><html><head><meta charset="UTF-8">
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link rel='stylesheet' id='theme-fonts-css' href='https://fonts.googleapis.com/css2?family=Inter:wght@400;700&#038;display=swap' media='all' />
<link rel="stylesheet" href="https://fonts.googleapis.com/css?family=Open+Sans:400,700">
<link rel="stylesheet" href="/wp-content/themes/t/style.css">
<style>@import url('https://fonts.googleapis.com/css2?family=Lora&display=swap');
body { font-family: Lora; }</style>
</head><body><p>Hello</p></body></html>`

// googleFonts stands in for Google Fonts: its stylesheets and fonts
type googleFonts struct {
	*httptest.Server
	requests atomic.Int32
	agents   []string
}

func newGoogleFonts(t *testing.T) *googleFonts {
	g := &googleFonts{}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.requests.Add(1)
		g.agents = append(g.agents, r.UserAgent())
		switch {
		case strings.HasPrefix(r.URL.Path, "/css/css2"):
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			io.WriteString(w, "/* latin */\n@font-face {\n  font-family: 'Inter';\n  font-style: normal;\n  font-weight: 400;\n  font-display: block;\n"+
				"  src: url(https://fonts.gstatic.com/s/inter/v18/UcC73FwrK3iLTeHuS_nVMrMxCp50SjIa1ZL7.woff2) format('woff2');\n}\n"+
				"@font-face {\n  font-family: 'Inter';\n  src: url(https://fonts.gstatic.com/s/inter/v18/other.woff2) format('woff2');\n}\n")
		case strings.HasPrefix(r.URL.Path, "/fonts/s/inter/v18/"):
			w.Header().Set("Content-Type", "font/woff2")
			io.WriteString(w, "wOF2 font data")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(g.Close)
	return g
}

func fontsOptimizer(t *testing.T, g *googleFonts) *Optimizer {
	o := New(Options{SelfHostFonts: true, googleCSS: g.URL + "/css/", googleFonts: g.URL + "/fonts/"})
	require.NotNil(t, o)
	return o
}

func TestSelfHostFontsRewrite(t *testing.T) {
	o := fontsOptimizer(t, newGoogleFonts(t))
	out := string(o.Rewrite(response("/", fontsPage)))
	assert.Contains(t, out, `<link href="/.well-known/honey/fonts/css2?family=Inter:wght@400;700&amp;display=swap" rel='stylesheet' id='theme-fonts-css' media='all' />`)
	assert.Contains(t, out, `<link href="/.well-known/honey/fonts/css?family=Open+Sans:400,700" rel="stylesheet">`)
	assert.Contains(t, out, `@import url('/.well-known/honey/fonts/css2?family=Lora&display=swap');`)
	assert.Contains(t, out, `<link rel="stylesheet" href="/wp-content/themes/t/style.css">`, "other stylesheets are left alone")
	assert.NotContains(t, out, "googleapis")
	assert.NotContains(t, out, "gstatic", "nor are there preconnects to Google")

	plain := `<html><head><link rel="preconnect" href="https://fonts.gstatic.com"></head><body></body></html>`
	assert.Equal(t, plain, string(o.Rewrite(response("/", plain))), "preconnects stay unless Google Fonts are moved")
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://www.example.com"+path, nil))
	return w
}

func TestSelfHostFontsServe(t *testing.T) {
	g := newGoogleFonts(t)
	o := fontsOptimizer(t, g)
	h := o.Middleware(http.NotFoundHandler())
	o.Rewrite(response("/", fontsPage))

	css := get(h, "/.well-known/honey/fonts/css2?family=Inter:wght@400;700&display=swap")
	require.Equal(t, http.StatusOK, css.Code, css.Body.String())
	assert.Equal(t, "text/css; charset=utf-8", css.Header().Get("Content-Type"))
	assert.Equal(t, "public, max-age=86400", css.Header().Get("Cache-Control"))
	body := css.Body.String()
	assert.Contains(t, body, "src: url(/.well-known/honey/fonts/s/inter/v18/UcC73FwrK3iLTeHuS_nVMrMxCp50SjIa1ZL7.woff2)")
	assert.NotContains(t, body, "gstatic")
	assert.Equal(t, 2, strings.Count(body, "font-display: swap;"))
	assert.Less(t, strings.Index(body, "font-display: swap;"), strings.Index(body, "font-display: block;"), "the page's own choice comes later, and wins")
	assert.Contains(t, g.agents[0], "Chrome/", "Google is asked as a browser which takes WOFF2")

	font := get(h, "/.well-known/honey/fonts/s/inter/v18/UcC73FwrK3iLTeHuS_nVMrMxCp50SjIa1ZL7.woff2")
	require.Equal(t, http.StatusOK, font.Code)
	assert.Equal(t, "wOF2 font data", font.Body.String())
	assert.Equal(t, "font/woff2", font.Header().Get("Content-Type"))
	assert.Equal(t, "public, max-age=31536000, immutable", font.Header().Get("Cache-Control"))

	requests := g.requests.Load()
	get(h, "/.well-known/honey/fonts/css2?family=Inter:wght@400;700&display=swap")
	get(h, "/.well-known/honey/fonts/s/inter/v18/UcC73FwrK3iLTeHuS_nVMrMxCp50SjIa1ZL7.woff2")
	assert.Equal(t, requests, g.requests.Load(), "they're kept")

	for _, path := range []string{
		"/.well-known/honey/fonts/s/../../etc/passwd.woff2",
		"/.well-known/honey/fonts/other?x=1",
		"/.well-known/honey/fonts/css2",
	} {
		assert.Equal(t, http.StatusNotFound, get(h, path).Code, path)
	}
	assert.Equal(t, http.StatusBadGateway, get(h, "/.well-known/honey/fonts/s/missing/v1/font.woff2").Code)
	assert.Equal(t, "page", func() string {
		w := httptest.NewRecorder()
		o.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "page") })).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/about/", nil))
		return w.Body.String()
	}())
}

func TestSelfHostFontsLimitsUnexpectedFetches(t *testing.T) {
	g := newGoogleFonts(t)
	o := fontsOptimizer(t, g)
	h := o.Middleware(http.NotFoundHandler())
	for i := 0; i < unexpectedFetches; i++ {
		get(h, "/.well-known/honey/fonts/css2?family=Made+Up"+strings.Repeat("x", i))
	}
	w := get(h, "/.well-known/honey/fonts/css2?family=One+Too+Many")
	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.Contains(t, w.Body.String(), "too many")

	// Pages' own are always fetched
	o.Rewrite(response("/", fontsPage))
	assert.Equal(t, http.StatusOK, get(h, "/.well-known/honey/fonts/css2?family=Inter:wght@400;700&display=swap").Code)

	o.fonts.minute = o.fonts.minute.Add(-2 * time.Minute)
	assert.NotEqual(t, http.StatusBadGateway, get(h, "/.well-known/honey/fonts/css2?family=Next+Minute").Code, "a minute later")
}

func TestSelfHostFontsRefreshesStylesheets(t *testing.T) {
	g := newGoogleFonts(t)
	clock := &clock{t: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)}
	o := New(Options{SelfHostFonts: true, googleCSS: g.URL + "/css/", googleFonts: g.URL + "/fonts/", now: clock.now})
	h := o.Middleware(http.NotFoundHandler())
	path := "/.well-known/honey/fonts/css2?family=Inter"
	get(h, path)
	get(h, path)
	assert.Equal(t, int32(1), g.requests.Load())
	clock.t = clock.t.Add(25 * time.Hour)
	get(h, path)
	assert.Equal(t, int32(2), g.requests.Load(), "a day later, in case Google has updated it")
	g.Close()
	clock.t = clock.t.Add(25 * time.Hour)
	assert.Equal(t, http.StatusOK, get(h, path).Code, "and if Google can't be reached, the old one does")
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }
