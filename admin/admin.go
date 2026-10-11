// Package admin serves Honey's admin page: how the cache is doing, and a
// form to purge it.  It has no login, so it must only be served on a
// private address.
package admin

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/davidjwilkins/honey/metrics"
)

//go:embed page.html admin.css admin.js
var files embed.FS

var page = template.Must(template.New("page.html").Funcs(template.FuncMap{
	"bytes":    formatBytes,
	"duration": formatDuration,
	"percent":  formatPercent,
	"count":    formatCount,
}).ParseFS(files, "page.html"))

// Options configures the admin page.
type Options struct {
	// Version and Backend are shown at the top of the page
	Version string
	Backend string
	// Started is when Honey started
	Started time.Time
	// Metrics are the counts shown, and served at /metrics
	Metrics *metrics.Recorder
	// Purge removes the cached responses for a URL or path - or, if prefix
	// is set, every URL starting with it - and returns how many it removed
	// (see fetch.Purger).
	Purge func(rawURL string, prefix bool) (int, error)
}

// Handler returns the admin page's handler.  As well as the page, it
// serves the metrics at /metrics.
func Handler(opts Options) http.Handler {
	a := &admin{opts: opts, results: map[string]outcome{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.page)
	mux.HandleFunc("/stats", a.stats)
	mux.HandleFunc("/purge", a.purge)
	mux.Handle("/admin.css", static("admin.css", "text/css; charset=utf-8"))
	mux.Handle("/admin.js", static("admin.js", "text/javascript; charset=utf-8"))
	pages := browserOnly(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Prometheus may scrape by hostname, and the metrics are only
		// numbers, so they don't need the page's protections
		if r.URL.Path == "/metrics" && opts.Metrics != nil {
			opts.Metrics.ServeHTTP(w, r)
			return
		}
		pages.ServeHTTP(w, r)
	})
}

type admin struct {
	opts Options

	// results holds the outcome of each purge until the page it redirects
	// to shows it (rather than putting it in the URL, where any link could
	// make the page show anything)
	mu      sync.Mutex
	results map[string]outcome
}

// outcome is the outcome of a purge
type outcome struct {
	Purged  *purged
	Problem string
}

// maxResults bounds the outcomes waiting to be shown
const maxResults = 100

// browserOnly protects the page from other websites the admin visits:
//   - It is only served to requests for an IP address or localhost, so
//     that a site can't use DNS rebinding (pointing its own hostname at
//     the admin address) to read it or post to it.
//   - Posts must come from the page itself (by their Origin), so that
//     other sites can't submit its forms.
//   - It can't be framed, and only runs its own script.
func browserOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !privateHost(r.Host) {
			http.Error(w, "The admin page is only served for an IP address or localhost (e.g. http://127.0.0.1:9090/), not a hostname.", http.StatusMisdirectedRequest)
			return
		}
		if r.Method == http.MethodPost && !sameOrigin(r) {
			http.Error(w, "Forms can only be submitted from the admin page.", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; connect-src 'self'; "+
			"img-src 'self' data:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		// not no-referrer, which makes browsers send Origin: null with the
		// page's own form posts
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func privateHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return strings.EqualFold(host, "localhost") || net.ParseIP(host) != nil
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Browsers send Origin with posts, but older ones may only send this
		return r.Header.Get("Sec-Fetch-Site") == "same-origin"
	}
	return origin == "http://"+r.Host || origin == "https://"+r.Host
}

func static(name, contentType string) http.Handler {
	data, err := files.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Write(data)
	})
}

// view is what the page shows
type view struct {
	Version string
	Backend string
	Uptime  time.Duration

	Entries  int
	Bytes    int64
	MaxBytes int64
	Full     float64 // how full the cache is, from 0 to 1

	Requests    uint64
	HitRatio    float64 // of all requests, served from the cache
	Results     []result
	Backend5xx  uint64 // backend requests which failed, or errored
	BackendReqs uint64
	AvgBackend  time.Duration
	InFlight    int64

	// The result of a purge, if there was one
	outcome
}

type result struct {
	Name, Help string
	Count      uint64
	Share      float64
}

type purged struct {
	Target string
	Prefix bool
	Count  int
}

func (a *admin) view() view {
	v := view{Version: a.opts.Version, Backend: a.opts.Backend, Uptime: time.Since(a.opts.Started)}
	if a.opts.Metrics == nil {
		return v
	}
	s := a.opts.Metrics.Snapshot()
	v.Entries, v.Bytes, v.MaxBytes = s.Entries, s.Bytes, s.MaxBytes
	if v.MaxBytes > 0 {
		v.Full = float64(v.Bytes) / float64(v.MaxBytes)
	}
	for _, count := range s.Requests {
		v.Requests += count
	}
	for _, r := range []result{
		{Name: "hit", Help: "Served from the cache"},
		{Name: "multiplexed", Help: "Waited for another request's backend response"},
		{Name: "stale", Help: "Served stale while refreshing, or because the backend failed"},
		{Name: "miss", Help: "Fetched from the backend, and cached"},
		{Name: "bypass", Help: "Not cacheable (e.g. logged in, or a route with cache = false)"},
		{Name: "none", Help: "Purges and errors"},
	} {
		r.Count = s.Requests[r.Name]
		if v.Requests > 0 {
			r.Share = float64(r.Count) / float64(v.Requests)
		}
		v.Results = append(v.Results, r)
	}
	if v.Requests > 0 {
		v.HitRatio = float64(s.Requests["hit"]+s.Requests["multiplexed"]+s.Requests["stale"]) / float64(v.Requests)
	}
	for class, count := range s.Backend {
		v.BackendReqs += count
		if class == "5xx" || class == "error" {
			v.Backend5xx += count
		}
	}
	if v.BackendReqs > 0 {
		v.AvgBackend = s.BackendLatency / time.Duration(v.BackendReqs)
	}
	v.InFlight = s.BackendInFlight
	return v
}

func (a *admin) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	v := a.view()
	if id := r.URL.Query().Get("result"); id != "" {
		a.mu.Lock()
		v.outcome = a.results[id]
		delete(a.results, id)
		a.mu.Unlock()
	}
	a.render(w, "page", v)
}

// stats serves the stats section of the page, which the page's script
// refreshes itself with
func (a *admin) stats(w http.ResponseWriter, r *http.Request) {
	a.render(w, "stats", a.view())
}

func (a *admin) render(w http.ResponseWriter, name string, v view) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.ExecuteTemplate(w, name, v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (a *admin) purge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	target := strings.TrimSpace(r.PostFormValue("url"))
	prefix := r.PostFormValue("prefix") != ""
	var result outcome
	switch {
	case a.opts.Purge == nil:
		result.Problem = "This cache can't be purged."
	case target == "":
		result.Problem = "Enter a URL or path to purge."
	default:
		count, err := a.opts.Purge(target, prefix)
		if err != nil {
			result.Problem = fmt.Sprintf("Couldn't purge: %v", err)
			break
		}
		result.Purged = &purged{Target: target, Prefix: prefix, Count: count}
	}
	id := make([]byte, 16)
	rand.Read(id)
	key := hex.EncodeToString(id)
	a.mu.Lock()
	if len(a.results) >= maxResults {
		// e.g. purges by a script which doesn't follow the redirects
		for k := range a.results {
			delete(a.results, k)
		}
	}
	a.results[key] = result
	a.mu.Unlock()
	// Redirect, so that reloading the page doesn't purge again
	http.Redirect(w, r, "/?"+url.Values{"result": {key}}.Encode(), http.StatusSeeOther)
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func formatDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	case d >= time.Minute:
		return fmt.Sprintf("%dm %ds", int(d/time.Minute), int(d%time.Minute/time.Second))
	case d >= time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dms", d.Milliseconds())
}

func formatPercent(f float64) string {
	return strconv.FormatFloat(f*100, 'f', 1, 64) + "%"
}

// formatCount formats a count with thousands separators
func formatCount(count any) string {
	var s string
	switch n := count.(type) {
	case int:
		s = strconv.Itoa(n)
	case int64:
		s = strconv.FormatInt(n, 10)
	case uint64:
		s = strconv.FormatUint(n, 10)
	default:
		s = fmt.Sprint(n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
