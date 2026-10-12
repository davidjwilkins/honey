package admin

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/config"
	"github.com/davidjwilkins/honey/fetch"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const settingsFile = `
hosts = ["www.example.com"]

[backend]
uri = "http://127.0.0.1:8081"

[cache]
defaultTTL = "5m"
bypassCookies = []

[[route]]
match = "/wp-admin"
cache = false

[control]
secret = "the-secret-0123456789"
`

func settingsAdmin(t *testing.T) http.Handler {
	t.Helper()
	cfg, err := config.Parse(settingsFile)
	require.NoError(t, err)
	return Handler(Options{Started: time.Now(), Settings: cfg.File})
}

// settingsForm is the settings form as the page sends it, unchanged
func settingsForm(t *testing.T, h http.Handler) url.Values {
	t.Helper()
	cfg, err := config.Parse(settingsFile)
	require.NoError(t, err)
	a := &admin{opts: Options{Settings: cfg.File}}
	form := url.Values{}
	for _, s := range a.settingsView(nil, nil, nil).Sections {
		for _, f := range s.Fields {
			form.Set(f.Key, f.Value)
		}
	}
	return form
}

func post(h http.Handler, path string, form url.Values) *testResponse {
	w := do(h, http.MethodPost, path, "127.0.0.1:9090", form, "Origin", "http://127.0.0.1:9090")
	return &testResponse{w.Code, w.Header(), w.Body.String()}
}

type testResponse struct {
	Code   int
	Header http.Header
	Body   string
}

func TestSettingsPage(t *testing.T) {
	h := settingsAdmin(t)
	w := do(h, http.MethodGet, "/settings", "127.0.0.1:9090", nil)
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `name="cache.defaultTTL" type="text" value="5m"`)
	assert.Contains(t, body, `placeholder="default: 256MB"`, "blank settings show their defaults")
	assert.Contains(t, body, ">none</textarea>", "an empty list is shown as none")
	assert.Contains(t, body, "No changes")
	assert.Contains(t, body, "secret = &#34;(hidden)&#34;")
	assert.NotContains(t, body, "the-secret-0123456789", "the secret is never shown")
	assert.Contains(t, body, `aria-current="page">Settings`)
}

func TestSettingsPreview(t *testing.T) {
	h := settingsAdmin(t)
	form := settingsForm(t, h)
	form.Set("cache.defaultTTL", "10m")
	form.Set("hosts", "www.example.com\nexample.com")
	form.Set("cache.bypassCookies", "")
	resp := post(h, "/settings/preview", form)
	require.Equal(t, http.StatusOK, resp.Code)
	assert.Contains(t, resp.Body, `<span class="del">- defaultTTL = &#34;5m&#34;</span>`)
	assert.Contains(t, resp.Body, `<span class="add">&#43; defaultTTL = &#34;10m&#34;</span>`)
	assert.Contains(t, resp.Body, `<span class="add">&#43; hosts = [&#34;www.example.com&#34;, &#34;example.com&#34;]</span>`)
	assert.Contains(t, resp.Body, `<span class="del">- bypassCookies = []</span>`, "blank means the default")
	assert.Contains(t, resp.Body, `class="valid"`)
	assert.Contains(t, resp.Body, "5 changed lines")

	form = settingsForm(t, h)
	form.Set("cache.defaultTTL", "soon")
	resp = post(h, "/settings/preview", form)
	assert.Contains(t, resp.Body, "not valid")
	assert.Contains(t, resp.Body, "cache.defaultTTL must be a duration")

	form = settingsForm(t, h)
	form.Set("warm.rate", "fast")
	assert.Contains(t, post(h, "/settings/preview", form).Body, "warm.rate: a number")

	form = settingsForm(t, h)
	form.Set("route", "listen = \":1\"\n")
	assert.Contains(t, post(h, "/settings/preview", form).Body, "route: only [[route]] tables go here")

	assert.Equal(t, http.StatusMethodNotAllowed, do(h, http.MethodGet, "/settings/preview", "127.0.0.1:9090", nil).Code)
}

func TestSettingsWithoutJavaScript(t *testing.T) {
	h := settingsAdmin(t)
	form := settingsForm(t, h)
	form.Set("cache.defaultTTL", "10m")
	resp := post(h, "/settings", form)
	require.Equal(t, http.StatusOK, resp.Code)
	assert.Contains(t, resp.Body, `value="10m"`, "the form keeps what was typed")
	assert.Contains(t, resp.Body, `class="setting changed"`)
	assert.Contains(t, resp.Body, `&#43; defaultTTL = &#34;10m&#34;`)
}

func TestSettingsExport(t *testing.T) {
	h := settingsAdmin(t)
	form := settingsForm(t, h)
	form.Set("cache.defaultTTL", "10m")
	form.Set("cache.stripCookies", "pll_language, PHPSESSID")
	resp := post(h, "/settings/export", form)
	require.Equal(t, http.StatusOK, resp.Code, resp.Body)
	assert.Equal(t, `attachment; filename="honey.toml"`, resp.Header.Get("Content-Disposition"))
	assert.True(t, strings.HasPrefix(resp.Body, "# Honey's settings, exported from its admin page"))

	cfg, err := config.Parse(resp.Body)
	require.NoError(t, err, resp.Body)
	assert.Equal(t, 10*time.Minute, cfg.Cache.DefaultTTL)
	assert.Equal(t, []string{"pll_language", "PHPSESSID"}, cfg.Cache.StripCookies)
	assert.Equal(t, "the-secret-0123456789", cfg.Control.Secret, "the export keeps the secret")
	assert.Equal(t, []string{"www.example.com"}, cfg.Hosts)
	require.Len(t, cfg.Routes, 1)
	assert.False(t, cfg.Routes[0].Cache)
	assert.Empty(t, cfg.Cache.BypassCookies)

	form.Set("cache.defaultTTL", "soon")
	resp = post(h, "/settings/export", form)
	assert.Equal(t, http.StatusBadRequest, resp.Code, "invalid settings aren't exported")
	assert.Contains(t, resp.Body, "cache.defaultTTL must be a duration")

	w := do(h, http.MethodPost, "/settings/export", "127.0.0.1:9090", settingsForm(t, h), "Origin", "https://evil.example")
	assert.Equal(t, http.StatusForbidden, w.Code, "other sites can't export the secret")
}

func TestSettingsSecret(t *testing.T) {
	h := settingsAdmin(t)
	export := func(form url.Values) *config.Config {
		resp := post(h, "/settings/export", form)
		require.Equal(t, http.StatusOK, resp.Code, resp.Body)
		cfg, err := config.Parse(resp.Body)
		require.NoError(t, err)
		return cfg
	}
	form := settingsForm(t, h)
	form.Set("control.secret", "a-new-secret-0123456789")
	preview := post(h, "/settings/preview", form).Body
	assert.Contains(t, preview, "(changed, hidden)")
	assert.NotContains(t, preview, "a-new-secret")
	assert.Equal(t, "a-new-secret-0123456789", export(form).Control.Secret)

	form = settingsForm(t, h)
	form.Set("control.secret.remove", "1")
	assert.Empty(t, export(form).Control.Secret)

	// The CDN's token is a secret too
	form = settingsForm(t, h)
	form.Set("cdn.provider", "cloudflare")
	form.Set("cdn.zone", "zone1")
	form.Set("cdn.token", "cdn-token-0123456789")
	form.Set("cdn.hosts", "www.example.com")
	preview = post(h, "/settings/preview", form).Body
	assert.NotContains(t, preview, "cdn-token")
	cfg := export(form)
	require.NotNil(t, cfg.CDN)
	assert.Equal(t, "cdn-token-0123456789", cfg.CDN.Token)
	assert.Equal(t, "the-secret-0123456789", cfg.Control.Secret, "the other secret is kept")
}

func TestDiffLines(t *testing.T) {
	assert.Equal(t, []diffLine{{" ", "a"}, {"-", "b"}, {"+", "B"}, {" ", "c"}, {"+", "d"}},
		diffLines("a\nb\nc\n", "a\nB\nc\nd\n"))
	assert.Equal(t, []diffLine{{"+", "a"}}, diffLines("", "a\n"))
	assert.Empty(t, diffLines("", ""))
}

func TestTestPage(t *testing.T) {
	var got *http.Request
	h := Handler(Options{
		Started: time.Now(),
		Origin:  "https://www.example.com",
		Explain: func(r *http.Request) fetch.Explanation {
			got = r
			return fetch.Explanation{
				Cacheable: true,
				Verdict:   "Cacheable, in the cache now",
				Steps:     []fetch.Step{{Outcome: fetch.StepChanged, Title: "Cookies removed", Detail: "removed <theme>"}},
				Key:       "GET :: https://www.example.com/blog/",
				Cached:    &fetch.CachedResponse{StatusCode: 200, Bytes: 2048, Age: time.Minute, FreshFor: 4 * time.Minute, Usable: true},
			}
		},
	})
	w := do(h, http.MethodGet, "/test", "127.0.0.1:9090", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Nil(t, got, "nothing is tested until there's a URL")

	w = do(h, http.MethodGet, "/test?url=%2Fblog%2F%3Fs%3Dx&cookies=theme%3Ddark", "127.0.0.1:9090", nil)
	require.NotNil(t, got)
	assert.Equal(t, "www.example.com", got.Host, "a path is tested on the site's origin")
	assert.Equal(t, "/blog/?s=x", got.URL.RequestURI())
	assert.Equal(t, "https", got.Header.Get("X-Forwarded-Proto"))
	assert.Equal(t, "theme=dark", got.Header.Get("Cookie"))
	assert.Equal(t, http.MethodGet, got.Method)
	body := w.Body.String()
	assert.Contains(t, body, "Cacheable, in the cache now")
	assert.Contains(t, body, `class="step-changed"`)
	assert.Contains(t, body, "removed &lt;theme&gt;")
	assert.Contains(t, body, "GET :: https://www.example.com/blog/")
	assert.Contains(t, body, "200 · 2.0 KB · fetched 1m 0s ago · fresh for 4m 0s")
	assert.Contains(t, body, `aria-current="page">Test a URL`)

	w = do(h, http.MethodGet, "/test?url=nope", "127.0.0.1:9090", nil)
	assert.Contains(t, w.Body.String(), "isn&#39;t a full URL")
	w = do(h, http.MethodGet, "/test?url=%2F&method=DELETE", "127.0.0.1:9090", nil)
	assert.Contains(t, w.Body.String(), "test a GET, HEAD or POST request")
}
