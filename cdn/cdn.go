// Package cdn passes Honey's purges on to a CDN in front of it (Cloudflare
// or Bunny), so that a CDN can cache pages near visitors around the world
// without them going out of date there: when a page is purged from Honey,
// it's purged from the CDN too.
//
// Purges come in bursts (WordPress purges a post, its archives, the home
// page and the feeds together), so they're collected for a moment, and
// sent together, with what's already covered left out.
package cdn

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/davidjwilkins/honey/events"
)

// The CDNs purges can be passed on to
const (
	Cloudflare = "cloudflare"
	Bunny      = "bunny"
)

const (
	// DefaultDelay is how long purges are collected for before they're sent
	DefaultDelay = 2 * time.Second
	// cloudflareBatch is how many URLs or prefixes Cloudflare is sent at
	// once
	cloudflareBatch = 30
	// maxBunnyURLs is how many URLs are purged from Bunny one by one; any
	// more and the whole site is purged instead
	maxBunnyURLs = 100
)

// The CDNs' APIs (variables, for tests)
var (
	CloudflareAPI = "https://api.cloudflare.com/client/v4"
	BunnyAPI      = "https://api.bunny.net"
)

// Options configures a Purger
type Options struct {
	// Provider is Cloudflare or Bunny
	Provider string
	// Zone is Cloudflare's zone ID
	Zone string
	// Token is a Cloudflare API token which can purge the zone's cache, or
	// a Bunny account's API key
	Token string
	// Hosts are the hosts the CDN serves the site on, whose URLs are
	// purged
	Hosts []string
	// Delay is how long purges are collected for (default DefaultDelay)
	Delay time.Duration
	// Events, if set, is where purges (and failures) are logged
	Events *events.Log
	// Client sends the requests (default: one with a 30 second timeout)
	Client *http.Client

	// api is the CDN's API (for tests)
	api string
}

// Purger passes purges on to a CDN
type Purger struct {
	opts Options
	send func(b *batch) (int, error)

	mu      sync.Mutex
	pending *batch
	timer   *time.Timer
	// sending is held while a batch is sent, so that batches go in order
	sending sync.Mutex
}

// batch is the purges collected to send together
type batch struct {
	// paths are exact paths (and query strings)
	paths map[string]bool
	// prefixes are paths everything starting with which is purged, and
	// the paths Honey had cached under each (if it said)
	prefixes map[string][]string
}

func newBatch() *batch {
	return &batch{paths: map[string]bool{}, prefixes: map[string][]string{}}
}

// New returns a Purger for opts
func New(opts Options) (*Purger, error) {
	if opts.Token == "" {
		return nil, errors.New("a token is needed")
	}
	if len(opts.Hosts) == 0 {
		return nil, errors.New("the hosts the CDN serves are needed")
	}
	if opts.Delay <= 0 {
		opts.Delay = DefaultDelay
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 30 * time.Second}
	}
	p := &Purger{opts: opts}
	switch opts.Provider {
	case Cloudflare:
		if opts.Zone == "" {
			return nil, errors.New("Cloudflare's zone ID is needed")
		}
		if p.opts.api == "" {
			p.opts.api = CloudflareAPI
		}
		p.send = p.cloudflare
	case Bunny:
		if p.opts.api == "" {
			p.opts.api = BunnyAPI
		}
		p.send = p.bunny
	default:
		return nil, fmt.Errorf("unknown CDN %q: %q or %q", opts.Provider, Cloudflare, Bunny)
	}
	return p, nil
}

// Name is the CDN's name, for people
func (p *Purger) Name() string {
	if p.opts.Provider == Bunny {
		return "Bunny"
	}
	return "Cloudflare"
}

// Purge purges path (with any query string) from the CDN - or, with
// prefix, everything whose path starts with it - shortly.  cached are the
// paths Honey had cached under a prefix, which are purged one by one from
// a CDN which can't purge by any prefix (Cloudflare only purges whole
// path segments).
func (p *Purger) Purge(path string, prefix bool, cached []string) {
	if p == nil || !strings.HasPrefix(path, "/") {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending == nil {
		p.pending = newBatch()
		p.timer = time.AfterFunc(p.opts.Delay, p.Flush)
	}
	if prefix {
		if i := strings.IndexByte(path, '?'); i >= 0 {
			path = path[:i]
		}
		p.pending.prefixes[path] = append(p.pending.prefixes[path], cached...)
	} else {
		p.pending.paths[path] = true
	}
}

// Flush sends the purges collected so far, now
func (p *Purger) Flush() {
	if p == nil {
		return
	}
	p.mu.Lock()
	b := p.pending
	p.pending = nil
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	p.mu.Unlock()
	if b == nil {
		return
	}
	p.sending.Lock()
	defer p.sending.Unlock()
	b.simplify()
	n, err := p.send(b)
	if err != nil {
		log.Printf("purging from %s: %v", p.Name(), err)
		p.opts.Events.Add(events.Error, "cdn purge failed", 10*time.Minute, func(e *events.Event) {
			e.Text = fmt.Sprintf("couldn't purge from %s: %v", p.Name(), err)
		})
		return
	}
	p.opts.Events.Add(events.Purge, "cdn purge", time.Minute, func(e *events.Event) {
		total, _ := e.Data().(int)
		total += n
		e.SetData(total)
		e.Text = fmt.Sprintf("purged %d %s from %s", total, plural(total, "URL or prefix", "URLs and prefixes"), p.Name())
	})
}

// everything returns whether b purges every path
func (b *batch) everything() bool {
	_, all := b.prefixes["/"]
	return all
}

// simplify leaves out what's covered by a prefix
func (b *batch) simplify() {
	if b.everything() {
		b.paths, b.prefixes = map[string]bool{}, map[string][]string{"/": nil}
		return
	}
	prefixes := b.sortedPrefixes()
	for _, prefix := range prefixes {
		for _, other := range prefixes {
			if other != prefix && strings.HasPrefix(prefix, other) {
				delete(b.prefixes, prefix)
				break
			}
		}
	}
	for path := range b.paths {
		for prefix := range b.prefixes {
			if strings.HasPrefix(path, prefix) {
				delete(b.paths, path)
				break
			}
		}
	}
}

func (b *batch) sortedPrefixes() []string {
	prefixes := make([]string, 0, len(b.prefixes))
	for prefix := range b.prefixes {
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)
	return prefixes
}

func (b *batch) sortedPaths() []string {
	paths := make([]string, 0, len(b.paths))
	for path := range b.paths {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// cloudflare purges b from Cloudflare (see
// https://developers.cloudflare.com/api/resources/cache/methods/purge/),
// returning how many URLs, prefixes and hosts it purged
func (p *Purger) cloudflare(b *batch) (int, error) {
	if b.everything() {
		return len(p.opts.Hosts), p.cloudflareRequest(map[string]any{"hosts": p.opts.Hosts})
	}
	// Cloudflare purges prefixes which are whole path segments ("/blog/"
	// purges /blog/ and /blog/2026/, but "/blog" doesn't purge /blogroll/),
	// so other prefixes are purged as the paths Honey had cached under them
	var prefixes []string
	files := map[string]bool{}
	for _, path := range b.sortedPaths() {
		files[path] = true
	}
	for _, prefix := range b.sortedPrefixes() {
		if strings.HasSuffix(prefix, "/") {
			for _, host := range p.opts.Hosts {
				prefixes = append(prefixes, host+strings.TrimSuffix(prefix, "/"))
			}
			continue
		}
		files[prefix] = true
		for _, path := range b.prefixes[prefix] {
			files[path] = true
		}
	}
	var urls []string
	for path := range files {
		for _, host := range p.opts.Hosts {
			urls = append(urls, "https://"+host+path)
		}
	}
	sort.Strings(urls)
	for _, list := range []struct {
		key   string
		items []string
	}{{"prefixes", prefixes}, {"files", urls}} {
		for start := 0; start < len(list.items); start += cloudflareBatch {
			end := min(start+cloudflareBatch, len(list.items))
			if err := p.cloudflareRequest(map[string]any{list.key: list.items[start:end]}); err != nil {
				return 0, err
			}
		}
	}
	return len(prefixes) + len(urls), nil
}

func (p *Purger) cloudflareRequest(body map[string]any) error {
	data, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, p.opts.api+"/zones/"+url.PathEscape(p.opts.Zone)+"/purge_cache", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.opts.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.do(req, data, true)
	if err != nil {
		return err
	}
	var result struct {
		Success bool `json:"success"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("unexpected response: %s", truncate(resp))
	}
	if !result.Success {
		var messages []string
		for _, e := range result.Errors {
			messages = append(messages, fmt.Sprintf("%s (%d)", e.Message, e.Code))
		}
		return fmt.Errorf("%s", strings.Join(messages, "; "))
	}
	return nil
}

// bunny purges b from Bunny (see
// https://docs.bunny.net/reference/purgepublic_indexpost), whose purges
// may end with a "*" to purge everything starting with them, returning how
// many URLs it purged
func (p *Purger) bunny(b *batch) (int, error) {
	var urls []string
	for _, prefix := range b.sortedPrefixes() {
		for _, host := range p.opts.Hosts {
			urls = append(urls, "https://"+host+prefix+"*")
		}
	}
	for _, path := range b.sortedPaths() {
		for _, host := range p.opts.Hosts {
			urls = append(urls, "https://"+host+path)
		}
	}
	if len(urls) > maxBunnyURLs {
		urls = nil
		for _, host := range p.opts.Hosts {
			urls = append(urls, "https://"+host+"/*")
		}
	}
	for _, u := range urls {
		req, err := http.NewRequest(http.MethodPost, p.opts.api+"/purge?"+url.Values{"url": {u}, "async": {"true"}}.Encode(), nil)
		if err != nil {
			return 0, err
		}
		req.Header.Set("AccessKey", p.opts.Token)
		if _, err := p.do(req, nil, false); err != nil {
			return 0, fmt.Errorf("%s: %w", u, err)
		}
	}
	return len(urls), nil
}

// do sends req (whose body is body), trying again once if the CDN is busy
// or failing, and returns the response's body if it succeeds - or, if the
// CDN explains its refusals in the body, if it refuses
func (p *Purger) do(req *http.Request, body []byte, explains bool) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		if body != nil {
			req.Body = io.NopCloser(bytes.NewReader(body))
		}
		resp, err := p.opts.Client.Do(req)
		var data []byte
		if err == nil {
			data, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			switch {
			case resp.StatusCode >= 200 && resp.StatusCode < 300:
				return data, nil
			case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
				err = fmt.Errorf("%s: %s", resp.Status, truncate(data))
			case explains:
				return data, nil
			default:
				return nil, fmt.Errorf("%s: %s", resp.Status, truncate(data))
			}
		}
		if attempt == 1 {
			return nil, err
		}
		time.Sleep(time.Second)
	}
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// Protect stops a CDN keeping responses it shouldn't:
//
//   - the stale pages Honey serves while it refreshes them (or while the
//     backend is down) are sent with "Cache-Control: no-cache", rather than
//     the cached page's own: otherwise a CDN asking for a page Honey has
//     just been told to purge (softly) would get the old page, and keep it
//   - responses Honey doesn't cache (pages WordPress renders for logged in
//     users, commenters and shoppers, pages which start a session, and
//     refusals) are marked private, as WordPress doesn't always say they're
//     personal
//
// Pages served from (or put in) Honey's cache are left as they are.
func Protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&staleWriter{ResponseWriter: w}, r)
	})
}

type staleWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *staleWriter) WriteHeader(status int) {
	if !w.wrote && status >= 200 {
		w.wrote = true
		h := w.Header()
		switch h.Get("X-Honey-Cache") {
		case "HIT", "MISS", "MISS (MULTIPLEXED)":
		case "STALE":
			h.Set("Cache-Control", "no-cache")
			h.Del("Expires")
		default:
			cc := h.Get("Cache-Control")
			lower := strings.ToLower(cc)
			if !strings.Contains(lower, "private") && !strings.Contains(lower, "no-store") {
				if cc == "" {
					h.Set("Cache-Control", "private")
				} else {
					h.Set("Cache-Control", "private, "+cc)
				}
			}
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *staleWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *staleWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack lets the proxy take over the connection, e.g. for websockets
func (w *staleWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("cdn: the ResponseWriter can't be hijacked")
	}
	return h.Hijack()
}

// Unwrap lets http.ResponseController reach the underlying ResponseWriter
func (w *staleWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
