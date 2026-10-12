// Package admin serves Honey's admin page: how the cache is doing, and a
// form to purge it.  It has no login, so it must only be served on a
// private address.
package admin

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/davidjwilkins/honey/cache"
	"github.com/davidjwilkins/honey/config"
	"github.com/davidjwilkins/honey/events"
	"github.com/davidjwilkins/honey/fetch"
	"github.com/davidjwilkins/honey/metrics"
)

//go:embed page.html settings.html browse.html admin.css admin.js
var files embed.FS

var page = template.Must(template.New("page.html").Funcs(template.FuncMap{
	"bytes":    formatBytes,
	"duration": formatDuration,
	"percent":  formatPercent,
	"count":    formatCount,
	"svg":      svgPercent,
	"freshFor": freshFor,
	"int64":    func(n int) int64 { return int64(n) },
}).ParseFS(files, "page.html", "settings.html", "browse.html"))

// Options configures the admin page.
type Options struct {
	// Version, Backend and Hosts (the hosts served, if not every host) are
	// shown at the top of the page
	Version string
	Backend string
	Hosts   []string
	// Started is when Honey started
	Started time.Time
	// Metrics are the counts shown, and served at /metrics
	Metrics *metrics.Recorder
	// Events are the recent events shown, if set.  Purges from the page
	// are logged to it.
	Events *events.Log
	// Purge removes the cached responses for a URL or path - or, if prefix
	// is set, every URL starting with it - and returns how many it removed.
	// With soft, it expires them instead (see fetch.Purger).
	Purge func(rawURL string, prefix, soft bool) (int, error)
	// Count returns how many cached responses Purge would remove, if set
	// (see fetch.Counter), so that the page can say before purging.
	Count func(rawURL string, prefix bool) (int, error)
	// Explain says how a request would be handled (see fetch.Explain), for
	// the page which tests URLs, if set.
	Explain func(r *http.Request) fetch.Explanation
	// Origin is the scheme and host (e.g. https://www.example.com) of
	// paths tested without them.
	Origin string
	// Entries lists the cached responses, most recently used first, and
	// Entry returns one by its key, for the cache browser, if set.
	Entries func() []cache.Entry
	Entry   func(key string) (cache.Entry, cache.Response, bool)
	// Fragments lists the shared fragments, and PurgeFragments purges them
	// by name (see fetch.FragmentPurger), if set.
	Fragments      func() []cache.FragmentInfo
	PurgeFragments func(name string) (fragments, pages int, err error)
	// Settings are Honey's settings, as written in its config file, for
	// the settings page, if set.  The page never changes them: it lets
	// them be edited and exported as a new config file.
	Settings *config.File
}

// Handler returns the admin page's handler.  As well as the page, it
// serves the metrics at /metrics.
func Handler(opts Options) http.Handler {
	a := &admin{opts: opts, results: map[string]outcome{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.page)
	mux.HandleFunc("/stats", a.stats)
	mux.HandleFunc("/purge", a.purge)
	mux.HandleFunc("/count", a.count)
	mux.HandleFunc("/test", a.test)
	mux.HandleFunc("/cache", a.browse)
	mux.HandleFunc("/cache/entry", a.entry)
	mux.HandleFunc("/settings", a.settings)
	mux.HandleFunc("/settings/preview", a.settingsPreview)
	mux.HandleFunc("/settings/export", a.settingsExport)
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

// chrome is what every page's header shows
type chrome struct {
	Version string
	Backend string
	Hosts   string
	Uptime  time.Duration
	// Section is the page's: dashboard, cache, test or settings
	Section string
}

func (a *admin) chrome(section string) chrome {
	return chrome{
		Version: a.opts.Version,
		Backend: a.opts.Backend,
		Hosts:   strings.Join(a.opts.Hosts, ", "),
		Uptime:  time.Since(a.opts.Started),
		Section: section,
	}
}

// view is what the dashboard shows
type view struct {
	chrome

	Entries  int
	Bytes    int64
	MaxBytes int64
	Full     float64 // how full the cache is, from 0 to 1

	Requests uint64
	HitRatio float64 // of all requests, served from the cache
	Trend    trend   // of the hit ratio over the last hour
	Results  []result

	BackendReqs   uint64
	P50, P95, P99 string // how long the backend takes to respond
	Histogram     []bar
	// HistogramWidth is the width of the histogram's box: barWidth for
	// each bar
	HistogramWidth int
	InFlight       int64
	Backend5xx     uint64 // backend requests which failed, or errored
	LastError      string // the last of them, and when
	RecentError    bool   // whether it was in the last hour

	Events []event

	// The result of a purge, if there was one
	outcome
}

type result struct {
	Name, Class, Help string
	Count             uint64
	Share             float64
	Offset            float64 // the shares of the results before it
}

// trend is a sparkline of the hit ratio, drawn in a trendWidth x
// trendHeight box: one polyline per run of minutes with requests
type trend struct {
	Lines    []string
	Min, Max float64
	Enough   bool // whether there are at least two minutes to draw
	Hour     bool // whether it covers the whole hour, not just since Honey started
}

const (
	trendWidth  = 240
	trendHeight = 48
)

// bar is a bucket of the backend latency histogram
type bar struct {
	Label  string // under the bar
	Range  string // what the bar counts, e.g. "up to 25ms"
	Count  uint64
	Height float64 // as a share of the tallest bar, from 0 to 1
	Top    float64 // 1 - Height
	X      int     // where it starts, in a box barWidth wide for each bar
	Slow   bool
}

// barWidth is the width of a histogram bar, and the gap after it
const barWidth = 10

type event struct {
	Time  string
	Kind  string
	Text  string
	Count int
}

type purged struct {
	Target string
	Prefix bool
	Soft   bool
	Count  int
	// Fragment is whether Target is a shared fragment's name, and Pages
	// how many pages are being refreshed for it
	Fragment bool
	Pages    int
}

// results are the rows of the requests table, by their honey_requests_total
// label, named after their X-Honey-Cache header
var results = []result{
	{Name: "HIT", Class: "hit", Help: "from the cache"},
	{Name: "MULTIPLEXED", Class: "multiplexed", Help: "waited for another visitor's fetch"},
	{Name: "STALE", Class: "stale", Help: "old copy, while it's refreshed or the backend fails"},
	{Name: "MISS", Class: "miss", Help: "fetched from the backend, and cached"},
	{Name: "NO-CACHE", Class: "bypass", Help: "logged in, admin, personal or uncacheable"},
	{Name: "BLOCKED", Class: "blocked", Help: "turned away to protect WordPress"},
	{Name: "OTHER", Class: "none", Help: "purges, other hosts and errors"},
}

func (a *admin) view() view {
	v := view{chrome: a.chrome("dashboard")}
	for _, e := range a.opts.Events.Recent(12) {
		v.Events = append(v.Events, event{Time: formatTime(e.Time), Kind: string(e.Kind), Text: e.Text, Count: e.Count})
	}
	if a.opts.Metrics == nil {
		return v
	}
	s := a.opts.Metrics.Snapshot()
	v.Entries, v.Bytes, v.MaxBytes = s.Entries, s.Bytes, s.MaxBytes
	if v.MaxBytes > 0 {
		v.Full = math.Min(float64(v.Bytes)/float64(v.MaxBytes), 1)
	}
	for _, count := range s.Requests {
		v.Requests += count
	}
	var offset float64
	for _, r := range results {
		r.Count = s.Requests[r.Class]
		if v.Requests > 0 {
			r.Share = float64(r.Count) / float64(v.Requests)
		}
		r.Offset = offset
		offset += r.Share
		v.Results = append(v.Results, r)
	}
	if v.Requests > 0 {
		v.HitRatio = float64(s.Requests["hit"]+s.Requests["multiplexed"]+s.Requests["stale"]) / float64(v.Requests)
	}
	v.Trend = hitTrend(sinceStart(s.History, a.opts.Started))

	for class, count := range s.Backend {
		v.BackendReqs += count
		if class == "5xx" || class == "error" {
			v.Backend5xx += count
		}
	}
	if v.BackendReqs > 0 {
		v.P50, v.P95, v.P99 = quantile(s, 0.5), quantile(s, 0.95), quantile(s, 0.99)
	}
	v.Histogram = histogram(s.BackendBuckets)
	v.HistogramWidth = len(v.Histogram) * barWidth
	v.InFlight = s.BackendInFlight
	if e := s.LastBackendError; e != nil {
		what := e.Err
		if e.Status != 0 {
			what = fmt.Sprintf("%d", e.Status)
		}
		v.LastError = fmt.Sprintf("latest: %s for %s, %s ago", what, e.Path, formatDuration(time.Since(e.Time).Round(time.Second)))
		v.RecentError = time.Since(e.Time) < time.Hour
	}
	return v
}

// sinceStart returns the minutes of history from the one Honey started in
func sinceStart(history []metrics.Minute, started time.Time) []metrics.Minute {
	first := started.Truncate(time.Minute)
	for i, m := range history {
		if !m.Start.Before(first) {
			return history[i:]
		}
	}
	return nil
}

// hitTrend draws the share of each minute's requests served from the
// cache.  The scale starts a little below the lowest share, as it is
// usually high, and the changes are what matter.
func hitTrend(history []metrics.Minute) trend {
	t := trend{Min: 1, Max: 1}
	points := 0
	for _, m := range history {
		if m.Requests > 0 {
			t.Min = math.Min(t.Min, float64(m.FromCache)/float64(m.Requests))
			points++
		}
	}
	t.Enough = points >= 2
	t.Hour = len(history) >= 60
	if !t.Enough {
		return t
	}
	low := math.Max(0, math.Min(t.Min-0.02, t.Max-0.05))
	step := float64(trendWidth) / float64(len(history)-1)
	var line []string
	for i, m := range history {
		if m.Requests == 0 {
			if len(line) > 0 {
				t.Lines = append(t.Lines, strings.Join(line, " "))
				line = nil
			}
			continue
		}
		share := float64(m.FromCache) / float64(m.Requests)
		y := 4 + (t.Max-share)/(t.Max-low)*float64(trendHeight-8)
		line = append(line, fmt.Sprintf("%.1f,%.1f", float64(i)*step, y))
	}
	if len(line) > 0 {
		t.Lines = append(t.Lines, strings.Join(line, " "))
	}
	return t
}

// quantile formats the q quantile of the backend's response times
func quantile(s metrics.Snapshot, q float64) string {
	d, over := s.BackendQuantile(q)
	if over {
		return "over " + formatDuration(d)
	}
	return formatDuration(d)
}

// histogram returns a bar for each of the backend latency buckets
func histogram(buckets []uint64) []bar {
	bounds := metrics.BackendBuckets()
	var tallest uint64
	for _, count := range buckets {
		if count > tallest {
			tallest = count
		}
	}
	bars := make([]bar, len(buckets))
	for i, count := range buckets {
		b := bar{Count: count}
		if i < len(bounds) {
			b.Label = shortDuration(bounds[i])
			b.Range = "up to " + b.Label
			b.Slow = bounds[i] > time.Second
		} else {
			b.Label = shortDuration(bounds[len(bounds)-1]) + "+"
			b.Range = "over " + shortDuration(bounds[len(bounds)-1])
			b.Slow = true
		}
		if tallest > 0 {
			b.Height = float64(count) / float64(tallest)
			if count > 0 {
				// so that a few slow requests still show
				b.Height = math.Max(b.Height, 0.02)
			}
		}
		b.Top = 1 - b.Height
		b.X = i*barWidth + 1
		bars[i] = b
	}
	return bars
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
	v.outcome = a.result(r)
	a.render(w, "page", v)
}

// stats serves the stats section of the page, which the page's script
// refreshes itself with
func (a *admin) stats(w http.ResponseWriter, r *http.Request) {
	a.render(w, "stats", a.view())
}

func (a *admin) render(w http.ResponseWriter, name string, v any) {
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
	soft := r.PostFormValue("soft") != ""
	fragment := strings.TrimSpace(r.PostFormValue("fragment"))
	var result outcome
	switch {
	case fragment != "" && a.opts.PurgeFragments == nil:
		result.Problem = "This cache doesn't keep shared fragments."
	case fragment != "":
		fragments, pages, err := a.opts.PurgeFragments(fragment)
		if err != nil {
			result.Problem = fmt.Sprintf("Couldn't purge: %v", err)
			break
		}
		result.Purged = &purged{Target: fragment, Fragment: true, Count: fragments, Pages: pages}
		a.opts.Events.Add(events.Purge, "", 0, events.Text(fmt.Sprintf("purged the shared fragment %s · refreshing %d %s · from the admin page",
			fragment, pages, map[bool]string{true: "page", false: "pages"}[pages == 1])))
	case a.opts.Purge == nil:
		result.Problem = "This cache can't be purged."
	case target == "":
		result.Problem = "Enter a URL or path to purge."
	default:
		count, err := a.opts.Purge(target, prefix, soft)
		if err != nil {
			result.Problem = fmt.Sprintf("Couldn't purge: %v", err)
			break
		}
		result.Purged = &purged{Target: target, Prefix: prefix, Soft: soft, Count: count}
		a.logPurge(*result.Purged)
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
	// Redirect, so that reloading the page doesn't purge again: back to
	// the page the purge came from, if it says
	back := returnTo(r.PostFormValue("return"))
	query := back.Query()
	query.Set("result", key)
	back.RawQuery = query.Encode()
	http.Redirect(w, r, back.String(), http.StatusSeeOther)
}

// returnTo returns the admin page to go back to after a purge: the one
// named (one of the pages which can show its result, never another site),
// or else the dashboard.
func returnTo(page string) *url.URL {
	u, err := url.Parse(page)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" ||
		(u.Path != "/cache" && u.Path != "/cache/entry") {
		return &url.URL{Path: "/"}
	}
	u.Fragment = ""
	return u
}

// result returns (once) the outcome of the purge named in r's result
// parameter, if there is one
func (a *admin) result(r *http.Request) outcome {
	id := r.URL.Query().Get("result")
	if id == "" {
		return outcome{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	result := a.results[id]
	delete(a.results, id)
	return result
}

// logPurge logs a purge from the page
func (a *admin) logPurge(p purged) {
	mode := "purged"
	if p.Soft {
		mode = "soft purged"
	}
	target := p.Target
	if p.Prefix {
		target += " and everything under it"
	}
	responses := "responses"
	if p.Count == 1 {
		responses = "response"
	}
	a.opts.Events.Add(events.Purge, "", 0, events.Text(fmt.Sprintf("%s %s · %d %s · from the admin page", mode, target, p.Count, responses)))
}

// count says how many cached responses a purge would remove, as JSON:
// {"count": 38}, or {"error": "..."}
func (a *admin) count(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var body struct {
		Count *int   `json:"count,omitempty"`
		Error string `json:"error,omitempty"`
	}
	target := strings.TrimSpace(r.URL.Query().Get("url"))
	switch {
	case a.opts.Count == nil:
		body.Error = "this cache can't count its responses"
	case target == "":
		body.Error = "enter a URL or path"
	default:
		count, err := a.opts.Count(target, r.URL.Query().Get("prefix") != "")
		if err != nil {
			body.Error = err.Error()
		} else {
			body.Count = &count
		}
	}
	json.NewEncoder(w).Encode(body)
}

// formatTime formats when an event happened: the time of day if it was
// today, or the date too if it wasn't
func formatTime(t time.Time) string {
	now := time.Now()
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04:05")
	}
	return t.Format("2 Jan 15:04")
}

// shortDuration formats a round duration, e.g. 25ms or 2.5s
func shortDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + "s"
}

// svgPercent formats a share (from 0 to 1) as a percentage, for SVG
// lengths
func svgPercent(share float64) string {
	return strconv.FormatFloat(math.Max(0, math.Min(share, 1))*100, 'f', 2, 64) + "%"
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
