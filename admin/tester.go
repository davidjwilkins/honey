package admin

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/davidjwilkins/honey/fetch"
)

// testView is what the page which tests URLs shows
type testView struct {
	chrome
	URL, Method, Cookies string
	Methods              []string
	// Explanation is how the URL would be handled, once one is tested
	Explanation *fetch.Explanation
	Problem     string
}

var testMethods = []string{http.MethodGet, http.MethodHead, http.MethodPost}

// test shows how a URL would be handled: whether it would be cached, and
// why.  Nothing is fetched.
func (a *admin) test(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	v := testView{
		chrome:  a.chrome("test"),
		URL:     strings.TrimSpace(q.Get("url")),
		Method:  strings.ToUpper(q.Get("method")),
		Cookies: strings.TrimSpace(q.Get("cookies")),
		Methods: testMethods,
	}
	if v.Method == "" {
		v.Method = http.MethodGet
	}
	switch {
	case a.opts.Explain == nil:
		v.Problem = "This cache can't explain how it handles requests."
	case v.URL == "":
	default:
		req, err := a.testRequest(v.Method, v.URL, v.Cookies)
		if err != nil {
			v.Problem = err.Error()
			break
		}
		e := a.opts.Explain(req)
		v.Explanation = &e
	}
	a.render(w, "test", v)
}

// testRequest makes the request a visitor would send for rawURL (or for
// a path, on Origin), as Honey receives it
func (a *admin) testRequest(method, rawURL, cookies string) (*http.Request, error) {
	if !contains(testMethods, method) {
		return nil, fmt.Errorf("test a GET, HEAD or POST request")
	}
	if strings.HasPrefix(rawURL, "/") && a.opts.Origin != "" {
		rawURL = strings.TrimSuffix(a.opts.Origin, "/") + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%q isn't a full URL, e.g. https://www.example.com/blog/", rawURL)
	}
	r := &http.Request{
		Method:     method,
		URL:        &url.URL{Path: u.Path, RawPath: u.RawPath, RawQuery: u.RawQuery},
		RequestURI: u.RequestURI(),
		Host:       u.Host,
		Header:     http.Header{},
		Proto:      "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
	}
	// As if from a trusted proxy which handled the scheme
	r.Header.Set("X-Forwarded-Proto", u.Scheme)
	if cookies != "" {
		r.Header.Set("Cookie", cookies)
	}
	return r, nil
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// freshFor describes how much longer a cached response is fresh
func freshFor(d time.Duration) string {
	if d > 0 {
		return "fresh for " + formatDuration(d.Round(time.Second))
	}
	return "stale for " + formatDuration((-d).Round(time.Second))
}
