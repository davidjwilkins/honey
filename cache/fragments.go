package cache

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Fragments are parts of pages which are shared between many pages - such
// as a list of recent posts in a sidebar - and which the backend marks as
// such, so that when one changes, every cached page showing it can be
// updated without the backend rendering each of them again.
//
// The backend marks a fragment like this (the WordPress purge plugin does
// so for its Latest Posts block and Recent Posts widget):
//
//	<!--honey:fragment name="posts-block-1a2b3c" token="..."-->
//	...the fragment...
//	<!--/honey:fragment-->
//
// The token is the one Honey sent in the FragmentTokenHeader of the
// backend request, which changes with every request, so that a fragment
// marker can't be planted in a page's content (e.g. a post) to change
// every page.  It is removed from the cached page.
//
// When a page is cached, its fragments' content becomes the fragments'
// current content (the backend has just rendered it, so it is the newest).
// Pages cached earlier with older content are rebuilt with the new content
// the next time they're served: once per change, not every time.  Purging
// a fragment (see PurgeFragments) refreshes a page which shows it, to get
// its new content.

// FragmentTokenHeader is the request header which carries the token the
// backend marks fragments with
const FragmentTokenHeader = "X-Honey-Fragments"

// Limits on fragments, so that a backend's pages can't use up memory
// outside the cache's limit
const (
	maxFragmentsPerPage = 50
	maxFragmentBytes    = 64 << 10
	maxFragments        = 1000
)

// fragmentName is what a fragment may be called
var fragmentName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,99}$`)

// fragmentStart matches a fragment's opening marker
var fragmentStart = regexp.MustCompile(`<!--honey:fragment name="([^"]*)" token="([^"]*)"-->`)

const fragmentEnd = "<!--/honey:fragment-->"

// NewFragmentToken returns a token for a backend request sent at start.
// It says when the request was sent, so that fragments rendered for it
// can be told apart from ones rendered before a purge.
func NewFragmentToken(start time.Time) string {
	b := make([]byte, 16)
	rand.Read(b)
	return strconv.FormatInt(start.UnixNano(), 10) + "-" + hex.EncodeToString(b)
}

// tokenTime returns when the request a token was made for was sent
func tokenTime(token string) (time.Time, bool) {
	sent, _, found := strings.Cut(token, "-")
	nanos, err := strconv.ParseInt(sent, 10, 64)
	if !found || err != nil {
		return time.Time{}, false
	}
	return time.Unix(0, nanos), true
}

// segment is part of a page with fragments: text, or a fragment
type segment struct {
	text []byte
	// name is the fragment's name, if it is one, and text its content
	// when the page was cached
	name string
}

// parseFragments splits body into text and fragments marked with token.
// It returns nil if body has none, or if they're marked wrongly (e.g. one
// inside another, or without its end), in which case the page is cached
// as it is.
func parseFragments(body []byte, token string) []segment {
	if token == "" || !bytes.Contains(body, []byte("<!--honey:fragment ")) {
		return nil
	}
	var segments []segment
	fragments := 0
	rest := body
	for {
		loc := fragmentStart.FindSubmatchIndex(rest)
		if loc == nil {
			if bytes.Contains(rest, []byte(fragmentEnd)) {
				return nil
			}
			segments = append(segments, segment{text: rest})
			break
		}
		name, sent := string(rest[loc[2]:loc[3]]), string(rest[loc[4]:loc[5]])
		before := rest[:loc[0]]
		if bytes.Contains(before, []byte(fragmentEnd)) || sent != token || !fragmentName.MatchString(name) {
			return nil
		}
		content := rest[loc[1]:]
		end := bytes.Index(content, []byte(fragmentEnd))
		if end < 0 || bytes.Contains(content[:end], []byte("<!--honey:fragment ")) || end > maxFragmentBytes {
			return nil
		}
		if fragments++; fragments > maxFragmentsPerPage {
			return nil
		}
		segments = append(segments, segment{text: before}, segment{name: name, text: content[:end]})
		rest = content[end+len(fragmentEnd):]
	}
	if fragments == 0 {
		return nil
	}
	return segments
}

// assemble returns the page made of segments, with each fragment's content
// from content (or its own, if content doesn't have it), and its marker
// without the token - and its segments, which refer to the page (so that
// the one they were made from needn't be kept).
func assemble(segments []segment, content func(name string) []byte) ([]byte, []segment) {
	var b bytes.Buffer
	type span struct{ start, end int }
	spans := make([]span, len(segments))
	for i, s := range segments {
		if s.name == "" {
			spans[i].start = b.Len()
			b.Write(s.text)
			spans[i].end = b.Len()
			continue
		}
		text := s.text
		if c := content(s.name); c != nil {
			text = c
		}
		b.WriteString(`<!--honey:fragment name="` + s.name + `"-->`)
		spans[i].start = b.Len()
		b.Write(text)
		spans[i].end = b.Len()
		b.WriteString(fragmentEnd)
	}
	body := b.Bytes()
	assembled := make([]segment, len(segments))
	for i, s := range segments {
		assembled[i] = segment{name: s.name, text: body[spans[i].start:spans[i].end:spans[i].end]}
	}
	return body, assembled
}

// fragment is a fragment's current content, for one host
type fragment struct {
	content []byte
	// version is changed whenever content is
	version uint64
	// purged is when it was last purged: content from pages rendered
	// before then is out of date
	purged time.Time
	// source is the URL of the last page which showed it, to refresh when
	// it is purged
	source string
	// updated is when its content last changed
	updated time.Time
}

// fragments holds the fragments' current content, by host and name
type fragments struct {
	mu      sync.RWMutex
	byKey   map[string]*fragment
	version uint64
}

func newFragments() *fragments {
	return &fragments{byKey: map[string]*fragment{}}
}

func fragmentKey(host, name string) string {
	return host + " " + name
}

// seen records the content of a fragment in a page from host, requested
// from the backend at date, at source.  It returns the fragment's
// version afterwards, and whether the content was kept: it isn't if it was
// rendered before the fragment was last purged (so it's out of date), or
// if there are too many fragments.
func (f *fragments) seen(host, name string, content []byte, date time.Time, source string) (uint64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fragmentKey(host, name)
	fr, found := f.byKey[key]
	if !found {
		if len(f.byKey) >= maxFragments {
			return 0, false
		}
		fr = &fragment{}
		f.byKey[key] = fr
	}
	if date.Before(fr.purged) {
		return fr.version, false
	}
	fr.source = source
	if fr.version == 0 || !bytes.Equal(fr.content, content) {
		f.version++
		fr.version = f.version
		fr.content = append([]byte(nil), content...)
		fr.updated = time.Now()
	}
	return fr.version, true
}

// current returns the current content and version of a fragment from host
func (f *fragments) current(host, name string) ([]byte, uint64) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if fr, found := f.byKey[fragmentKey(host, name)]; found {
		return fr.content, fr.version
	}
	return nil, 0
}

// purge marks the fragments called name - or, if it ends with "*", whose
// names start with what precedes it - as purged, for every host, and
// returns how many there were and the pages which showed them.
func (f *fragments) purge(name string, now time.Time) (int, []string) {
	prefix, isPrefix := strings.CutSuffix(name, "*")
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	var sources []string
	seen := map[string]bool{}
	for key, fr := range f.byKey {
		_, n, _ := strings.Cut(key, " ")
		if n != name && !(isPrefix && strings.HasPrefix(n, prefix)) {
			continue
		}
		count++
		fr.purged = now
		if fr.source != "" && !seen[fr.source] {
			seen[fr.source] = true
			sources = append(sources, fr.source)
		}
	}
	sort.Strings(sources)
	return count, sources
}

// FragmentInfo describes a fragment, e.g. for the admin page
type FragmentInfo struct {
	Host, Name string
	Bytes      int
	Version    uint64
	Updated    time.Time
	Purged     time.Time
	Source     string
}

// list describes the fragments
func (f *fragments) list() []FragmentInfo {
	f.mu.RLock()
	defer f.mu.RUnlock()
	infos := make([]FragmentInfo, 0, len(f.byKey))
	for key, fr := range f.byKey {
		host, name, _ := strings.Cut(key, " ")
		infos = append(infos, FragmentInfo{Host: host, Name: name, Bytes: len(fr.content), Version: fr.version,
			Updated: fr.updated, Purged: fr.purged, Source: fr.source})
	}
	sort.Slice(infos, func(i, j int) bool {
		if infos[i].Name != infos[j].Name {
			return infos[i].Name < infos[j].Name
		}
		return infos[i].Host < infos[j].Host
	})
	return infos
}

// isHTML returns whether header is an HTML response's
func isHTML(header http.Header) bool {
	mediaType, _, _ := strings.Cut(header.Get("Content-Type"), ";")
	return strings.EqualFold(strings.TrimSpace(mediaType), "text/html")
}

// pageOrigin returns the host and URL of the page a response is for, from
// its base key (e.g. "GET :: https://www.example.com/blog/")
func pageOrigin(baseKey string) (host, rawURL string) {
	_, rawURL, _ = strings.Cut(baseKey, " :: ")
	if u, err := url.Parse(rawURL); err == nil {
		return u.Host, rawURL
	}
	return "", rawURL
}

// recordFragments records the content of r's fragments, which the backend
// has just rendered, as their current content, and which version of each
// r has.
func (c *defaultCacher) recordFragments(r *responseImpl) {
	host, source := pageOrigin(r.baseKey)
	// When the backend was asked for it: content rendered before a
	// fragment was purged is out of date
	date, ok := tokenTime(r.requestHeaders.Get(FragmentTokenHeader))
	if !ok {
		date = r.now
	}
	r.versions = map[string]uint64{}
	for _, s := range r.segments {
		if s.name == "" {
			continue
		}
		version, kept := c.fragments.seen(host, s.name, s.text, date, source)
		if !kept {
			// Older than the fragment's current content: rebuild it
			version = 0
		}
		r.versions[s.name] = version
	}
}

// withCurrentFragments returns r, which is cached under key, rebuilt with
// its fragments' current content if any has changed - and caches the
// rebuilt response in its place, so that it is only rebuilt once.
func (c *defaultCacher) withCurrentFragments(key string, r *responseImpl) Response {
	host, _ := pageOrigin(r.baseKey)
	current := func() bool {
		for name, version := range r.versions {
			if _, now := c.fragments.current(host, name); now != 0 && now != version {
				return false
			}
		}
		return true
	}
	if current() {
		return r
	}
	r.rebuildMu.Lock()
	defer r.rebuildMu.Unlock()
	if r.rebuilt != nil {
		return r.rebuilt
	}
	rebuilt := &responseImpl{
		response:       r.response,
		cookies:        r.cookies,
		headers:        r.headers.Clone(),
		requestHeaders: r.requestHeaders,
		now:            r.now,
		initialAge:     r.initialAge,
		baseKey:        r.baseKey,
		status:         r.status,
		statusCode:     r.statusCode,
		versions:       map[string]uint64{},
	}
	rebuilt.body, rebuilt.segments = assemble(r.segments, func(name string) []byte {
		content, version := c.fragments.current(host, name)
		if version == 0 {
			version = r.versions[name]
		}
		rebuilt.versions[name] = version
		return content
	})
	rebuilt.hits.Store(r.hits.Load())
	rebuilt.expired.Store(r.expired.Load())
	if rebuilt.headers.Get("Content-Length") != "" {
		rebuilt.headers.Set("Content-Length", strconv.Itoa(len(rebuilt.body)))
	}
	rebuilt.headers.Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
	c.encode(rebuilt, r.statusCode, rebuilt.headers, rebuilt.headers.Get("Etag") != "")
	r.rebuilt = rebuilt
	c.store.replace(key, r, rebuilt, responseSize(key, rebuilt))
	return rebuilt
}

// PurgeFragments marks the fragments called name - or, if it ends with
// "*", whose names start with what precedes it - as out of date, for
// every host.  It returns how many there were, and the URLs of pages which
// showed them: refreshing one of those gets a fragment's new content, for
// every page.
func (c *defaultCacher) PurgeFragments(name string) (int, []string) {
	return c.fragments.purge(name, time.Now())
}

// Fragments describes the shared fragments the cache knows of.
func (c *defaultCacher) Fragments() []FragmentInfo {
	return c.fragments.list()
}
