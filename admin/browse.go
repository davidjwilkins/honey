package admin

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/davidjwilkins/honey/cache"
	"github.com/davidjwilkins/honey/utilities"
)

// pageSize is how many cached responses the cache browser lists at once
const pageSize = 50

// browseView is what the cache browser shows
type browseView struct {
	chrome
	outcome
	// The filters: a path, which entries start with if it starts with
	// "/" and otherwise contain; the kind of response; its state; and the
	// order
	Path, Kind, State, Sort string
	Kinds, States           []option
	Sorts                   []option
	// Entries are this page's, of Matches which match the filters, of
	// Total in the cache
	Entries        []entryRow
	Matches, Total int
	Bytes          int64 // of the matches
	// From and To number this page's entries (from 1); Prev and Next are
	// links to the pages before and after, if there are any
	From, To   int
	Prev, Next string
	// ManyHosts is whether the entries are for more than one host, so
	// that each one's host is shown
	ManyHosts bool
	// Prefix is whether Path is a path prefix, which can be purged
	Prefix bool
	// Return is this page, to come back to after a purge
	Return string
	// Fragments are the shared fragments
	Fragments []fragmentRow
}

type fragmentRow struct {
	cache.FragmentInfo
	Changed string // how long ago its content last changed
}

type option struct {
	Value, Label string
}

var (
	kinds  = []option{{"", "Everything"}, {"pages", "Pages"}, {"static", "Static files"}, {"errors", "Redirects and errors"}}
	states = []option{{"", "Any state"}, {"fresh", "Fresh"}, {"stale", "Stale"}, {"expired", "Soft purged"}}
	sorts  = []option{{"", "Recently used"}, {"hits", "Most served"}, {"size", "Largest"}, {"newest", "Newest"}, {"oldest", "Oldest"}, {"path", "Path"}}
)

// entryRow is a row of the cache browser
type entryRow struct {
	cache.Entry
	Link  string // to its details
	State string // fresh, stale, expired or old
	When  string // e.g. "fresh for 3m 12s"
	Age   time.Duration
}

func row(e cache.Entry, now time.Time) entryRow {
	r := entryRow{Entry: e, Link: "/cache/entry?" + url.Values{"key": {e.Key}}.Encode(), Age: now.Sub(e.Fetched)}
	freshUntil := e.Fetched.Add(e.Lifetime)
	switch {
	case e.Fresh(now):
		left := freshUntil.Sub(now).Round(time.Second)
		r.State, r.When = "fresh", "fresh for "+formatDuration(left)
		if left < time.Second {
			r.When = "fresh for under 1s"
		}
	case e.Expired && e.Usable(now):
		r.State, r.When = "expired", "soft purged: refreshed on its next request"
	case e.Expired:
		r.State, r.When = "expired", "soft purged"
	case e.Usable(now):
		r.State, r.When = "stale", "stale: refreshed on its next request"
	default:
		r.State, r.When = "old", "too old to serve, kept in case the backend fails"
	}
	return r
}

// browse lists the cached responses
func (a *admin) browse(w http.ResponseWriter, r *http.Request) {
	if a.opts.Entries == nil {
		http.Error(w, "This cache can't be browsed.", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	v := browseView{
		chrome:  a.chrome("cache"),
		outcome: a.result(r),
		Path:    strings.TrimSpace(q.Get("path")),
		Kind:    pick(q.Get("kind"), kinds),
		State:   pick(q.Get("state"), states),
		Sort:    pick(q.Get("sort"), sorts),
		Kinds:   kinds,
		States:  states,
		Sorts:   sorts,
	}
	v.Prefix = strings.HasPrefix(v.Path, "/")
	now := time.Now()
	all := a.opts.Entries()
	v.Total = len(all)
	var matches []entryRow
	hosts := map[string]bool{}
	for _, e := range all {
		row := row(e, now)
		if !v.matches(row) {
			continue
		}
		matches = append(matches, row)
		hosts[e.Host] = true
		v.Bytes += e.Size
	}
	v.Matches = len(matches)
	v.ManyHosts = len(hosts) > 1
	sortRows(matches, v.Sort)

	page, _ := strconv.Atoi(q.Get("page"))
	if pages := (len(matches) + pageSize - 1) / pageSize; page < 1 || page > pages {
		page = 1
	}
	start := (page - 1) * pageSize
	end := min(start+pageSize, len(matches))
	v.Entries = matches[start:end]
	if len(matches) > 0 {
		v.From, v.To = start+1, end
	}
	link := func(page int) string {
		q := url.Values{}
		for key, value := range map[string]string{"path": v.Path, "kind": v.Kind, "state": v.State, "sort": v.Sort} {
			if value != "" {
				q.Set(key, value)
			}
		}
		if page > 1 {
			q.Set("page", strconv.Itoa(page))
		}
		if len(q) == 0 {
			return "/cache"
		}
		return "/cache?" + q.Encode()
	}
	if start > 0 {
		v.Prev = link(page - 1)
	}
	if end < len(matches) {
		v.Next = link(page + 1)
	}
	v.Return = link(page)
	if a.opts.Fragments != nil {
		for _, f := range a.opts.Fragments() {
			v.Fragments = append(v.Fragments, fragmentRow{FragmentInfo: f, Changed: formatDuration(now.Sub(f.Updated).Round(time.Second)) + " ago"})
		}
	}
	a.render(w, "cache", v)
}

// pick returns value if it is one of the options, or else the first
func pick(value string, options []option) string {
	for _, o := range options {
		if o.Value == value {
			return value
		}
	}
	return options[0].Value
}

// matches returns whether r passes v's filters
func (v browseView) matches(r entryRow) bool {
	switch {
	case v.Prefix && !strings.HasPrefix(r.Path, v.Path):
		return false
	case v.Path != "" && !v.Prefix && !strings.Contains(r.Path, v.Path) && !strings.Contains(r.Host, v.Path):
		return false
	}
	path, _, _ := strings.Cut(r.Path, "?")
	switch v.Kind {
	case "pages":
		if utilities.IsStaticFile(path) || r.StatusCode != http.StatusOK {
			return false
		}
	case "static":
		if !utilities.IsStaticFile(path) {
			return false
		}
	case "errors":
		if r.StatusCode == http.StatusOK {
			return false
		}
	}
	switch v.State {
	case "fresh":
		return r.State == "fresh"
	case "stale":
		return r.State == "stale" || r.State == "old"
	case "expired":
		return r.State == "expired"
	}
	return true
}

func sortRows(rows []entryRow, by string) {
	less := map[string]func(a, b entryRow) bool{
		"hits":   func(a, b entryRow) bool { return a.Hits > b.Hits },
		"size":   func(a, b entryRow) bool { return a.Size > b.Size },
		"newest": func(a, b entryRow) bool { return a.Fetched.After(b.Fetched) },
		"oldest": func(a, b entryRow) bool { return a.Fetched.Before(b.Fetched) },
		"path":   func(a, b entryRow) bool { return a.Path < b.Path },
	}[by]
	if less != nil {
		// Stable, so that ties stay in the order they were most recently used
		sort.SliceStable(rows, func(i, j int) bool { return less(rows[i], rows[j]) })
	}
}

// entryView is what the page for a cached response shows
type entryView struct {
	chrome
	outcome
	entryRow
	Found bool
	// Header is the headers it is served with, and Varied the request
	// headers it was fetched with which it Varies on
	Header, Varied []headerLine
	// Body is the start of its body, if it is text, and Truncated whether
	// there's more
	Body      string
	Truncated bool
	KeptUntil string
	// Test is a link to test its URL, and Return this page
	Test, Return string
}

type headerLine struct {
	Name, Value string
}

// maxBodyShown is how much of a cached body is shown
const maxBodyShown = 8 << 10

// entry shows a cached response
func (a *admin) entry(w http.ResponseWriter, r *http.Request) {
	if a.opts.Entry == nil {
		http.Error(w, "This cache can't be browsed.", http.StatusNotFound)
		return
	}
	key := r.URL.Query().Get("key")
	v := entryView{chrome: a.chrome("cache"), outcome: a.result(r)}
	e, resp, found := a.opts.Entry(key)
	if !found {
		// e.g. it was purged, or evicted
		w.WriteHeader(http.StatusNotFound)
		a.render(w, "entry", v)
		return
	}
	now := time.Now()
	v.Found = true
	v.entryRow = row(e, now)
	v.Header = headerLines(resp.Header())
	v.Varied = headerLines(cache.RequestHeader(resp))
	if !e.RemoveAt.IsZero() {
		v.KeptUntil = "until " + formatDuration(e.RemoveAt.Sub(now).Round(time.Second)) + " from now, unless it's evicted to make room first"
	} else {
		v.KeptUntil = "until it's evicted to make room"
	}
	if isText(e.ContentType) {
		body := resp.Body()
		if len(body) > maxBodyShown {
			body, v.Truncated = body[:maxBodyShown], true
		}
		if utf8.Valid(body) || v.Truncated {
			v.Body = strings.ToValidUTF8(string(body), "")
		}
	}
	v.Test = "/test?" + url.Values{"url": {e.URL}, "method": {e.Method}}.Encode()
	v.Return = "/cache/entry?" + url.Values{"key": {key}}.Encode()
	a.render(w, "entry", v)
}

func headerLines(h http.Header) []headerLine {
	var lines []headerLine
	for name, values := range h {
		for _, value := range values {
			lines = append(lines, headerLine{name, value})
		}
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].Name < lines[j].Name })
	return lines
}

// isText returns whether a Content-Type is text which can be shown
func isText(contentType string) bool {
	mediaType, _, _ := strings.Cut(strings.ToLower(contentType), ";")
	mediaType = strings.TrimSpace(mediaType)
	return strings.HasPrefix(mediaType, "text/") ||
		strings.HasSuffix(mediaType, "json") || strings.HasSuffix(mediaType, "xml") ||
		mediaType == "application/javascript" || mediaType == "image/svg+xml"
}
