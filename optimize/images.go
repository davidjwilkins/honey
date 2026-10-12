package optimize

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/davidjwilkins/honey/events"
)

// Preloading the main image: a page's Largest Contentful Paint - how soon
// its largest element (usually its featured or hero image) is shown - is
// held up if the browser finds the image late (e.g. it's a CSS background,
// or it comes after the page's scripts), or waits to load it (it's lazy
// loaded, or the browser doesn't know it's important).  Which image it is
// depends on the page and the size of the screen, and can't be told from
// the HTML alone, so visitors' browsers report it: until it's known for a
// page, the page is cached with a small script which reports it (once) to
// BeaconPath.  Honey checks it's one of the page's images, then refreshes
// the page, which is cached with a preload for it at the top of its
// <head> (and sent as an Early Hint, if it can be), and the image isn't
// lazy loaded.  Phones and desktops are learned separately, as their main
// images may differ.

// BeaconPath is where browsers report pages' main images
const BeaconPath = "/.well-known/honey/main-image"

const (
	// maxImagePages bounds how many pages' main images are remembered
	maxImagePages = 10000
	// maxCandidates is how many of a page's first images could be its
	// main image
	maxCandidates = 50
	// maxReport bounds a report's size
	maxReport = 4 << 10
	// mobileWidth is the viewport width below which a visitor is on a
	// phone
	mobileWidth = 768
)

// viewports are the kinds of screen main images are learned for
var viewports = [2]struct{ name, media string }{
	{"mobile", "(max-width: 767.98px)"},
	{"desktop", "(min-width: 768px)"},
}

// imageStore remembers pages' main images, by host and path
type imageStore struct {
	mu    sync.Mutex
	pages map[string]*imagePage
	max   int
}

type imagePage struct {
	// candidates are the page's images (as hashes of their normalized
	// URLs), which reports must be one of, while any is still to be learned
	candidates map[uint64]bool
	// known is whether each viewport's main image is known, and url what
	// it is ("" if it isn't an image)
	known [2]bool
	url   [2]string
}

func newImageStore(max int) *imageStore {
	return &imageStore{pages: map[string]*imagePage{}, max: max}
}

// get returns what's known of key's main images
func (s *imageStore) get(key string) (known [2]bool, urls [2]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.pages[key]; p != nil {
		return p.known, p.url
	}
	return
}

// expect records that key's page reports its main image for the
// viewports not yet known, which must be one of candidates
func (s *imageStore) expect(key string, candidates map[uint64]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pages[key]
	if p == nil {
		if len(s.pages) >= s.max {
			// Forget any one (the map's order is random)
			for k := range s.pages {
				delete(s.pages, k)
				break
			}
		}
		p = &imagePage{}
		s.pages[key] = p
	}
	p.candidates = candidates
}

// forget forgets key's main image for viewport v, e.g. as it's no longer
// on the page
func (s *imageStore) forget(key string, v int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.pages[key]; p != nil {
		p.known[v], p.url[v] = false, ""
	}
}

// learn records a report that key's main image for viewport v is u (a
// normalized URL, or "" if it isn't an image), and returns whether it was
// news - if the page is expecting it, and it's one of its images - and
// whether all its main images are now known
func (s *imageStore) learn(key string, v int, u string) (news, complete bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pages[key]
	if p == nil || p.known[v] || p.candidates == nil || (u != "" && !p.candidates[hash(u)]) {
		return false, false
	}
	p.known[v], p.url[v] = true, u
	if p.known[0] && p.known[1] {
		p.candidates = nil
		return true, true
	}
	return true, false
}

func hash(s string) uint64 {
	h := fnv.New64a()
	io.WriteString(h, s)
	return h.Sum64()
}

// normalize returns an image URL as it's compared: a path (and query) for
// one on host, or the whole URL (with a lower case host) for one elsewhere,
// without its fragment; or "" if it isn't an http(s) URL.  base is the
// page's URL, which relative URLs are relative to.
func normalize(raw, host string, base *url.URL) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "data:") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	u.Fragment = ""
	if u.Host == "" || strings.EqualFold(u.Host, host) {
		if !strings.HasPrefix(u.Path, "/") {
			return ""
		}
		return u.RequestURI()
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	u.Host = strings.ToLower(u.Host)
	return u.String()
}

// srcset returns the URLs in a srcset attribute
func srcset(s string) []string {
	var urls []string
	for _, candidate := range strings.Split(s, ",") {
		if fields := strings.Fields(candidate); len(fields) > 0 {
			urls = append(urls, fields[0])
		}
	}
	return urls
}

// backgroundURL finds url(...)s in the page's HTML, e.g. inline style
// attributes for cover images
var backgroundURL = regexp.MustCompile(`url\(\s*(?:&quot;|&#039;|["'])?([^"')&]+)`)

// imageURLs returns the URLs an image may show, normalized: its src,
// srcset, and lazy loading plugins' data-src and data-srcset
func imageURLs(img image, host string, base *url.URL) []string {
	var urls []string
	for _, name := range []string{"src", "data-src", "data-lazy-src"} {
		if u := normalize(img.attributes[name], host, base); u != "" {
			urls = append(urls, u)
		}
	}
	for _, name := range []string{"srcset", "data-srcset", "data-lazy-srcset"} {
		for _, raw := range srcset(img.attributes[name]) {
			if u := normalize(raw, host, base); u != "" {
				urls = append(urls, u)
			}
		}
	}
	return urls
}

// pageKey is the key a page's main images are remembered by
func pageKey(host, path string) string {
	return strings.ToLower(host) + path
}

// mainImage returns what to add to the top of r's page p's <head> and the
// end of its <body>, and the changes to make to its images, for its main
// image: a preload, if it's known, or a script to report it, if it isn't
func (o *Optimizer) mainImage(r *http.Request, p *page) (head, tail []byte, edits []edit) {
	if r == nil || r.URL == nil {
		return
	}
	host, base := r.Host, &url.URL{Path: r.URL.Path}
	key := pageKey(host, r.URL.Path)
	known, urls := o.images.get(key)

	// Where each known main image is on the page: the image showing it
	// (or -1 for a background)
	var found [2]int
	for v := range viewports {
		if !known[v] || urls[v] == "" {
			continue
		}
		found[v] = -2
		for i, img := range p.images {
			if contains(imageURLs(img, host, base), urls[v]) {
				found[v] = i
				break
			}
		}
		if found[v] == -2 && bytes.Contains(p.body, []byte(urls[v])) {
			found[v] = -1
		}
		if found[v] == -2 {
			// It's changed: learn it again
			o.images.forget(key, v)
			known[v], urls[v] = false, ""
		}
	}

	// The same image on every screen (perhaps different sizes of it,
	// from its srcset)
	same := known[0] && known[1] && urls[0] != "" && urls[1] != "" &&
		(urls[0] == urls[1] || (found[0] >= 0 && found[0] == found[1]))
	for v := range viewports {
		if !known[v] || urls[v] == "" || (same && v == 1) {
			continue
		}
		i := found[v]
		if same && i >= 0 && !lazyPlugin(p.images[i]) {
			// It's the main image on every screen, and the browser finds
			// it as soon as it reads it: it just mustn't wait (a preload
			// would only compete with the page's stylesheets)
			if e, ok := eager(p, p.images[i]); ok {
				edits = append(edits, e)
			}
			continue
		}
		if i >= 0 && !late(p.images[i]) {
			continue
		}
		// The browser would find it, or start loading it, late: a
		// background, or a lazy loaded image
		media := viewports[v].media
		if same {
			media = ""
		}
		head = append(head, preload(p, i, urls[v], media)...)
	}

	var need []string
	for v, vp := range viewports {
		if !known[v] {
			need = append(need, vp.name)
		}
	}
	if len(need) > 0 {
		candidates := map[uint64]bool{}
		for _, img := range p.images {
			if len(candidates) >= maxCandidates {
				break
			}
			for _, u := range imageURLs(img, host, base) {
				candidates[hash(u)] = true
			}
		}
		backgrounds := 0
		for _, m := range backgroundURL.FindAllSubmatch(p.body, -1) {
			if backgrounds >= maxCandidates {
				break
			}
			if u := normalize(html.UnescapeString(string(m[1])), host, base); u != "" {
				candidates[hash(u)] = true
				backgrounds++
			}
		}
		o.images.expect(key, candidates)
		tail = beacon(need)
	}
	return head, tail, edits
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// lazyPlugin returns whether a lazy loading plugin shows img (its real
// src is in a data- attribute)
func lazyPlugin(img image) bool {
	return img.attributes["data-src"] != "" || img.attributes["data-srcset"] != "" || img.attributes["data-lazy-src"] != ""
}

// late returns whether the browser would start loading img late
func late(img image) bool {
	return lazyPlugin(img) || strings.EqualFold(strings.TrimSpace(img.attributes["loading"]), "lazy")
}

// preload returns a <link rel="preload"> for the main image u, shown by
// the page's image i (or a background, if i is negative), on screens
// matching media (or all, if it's "")
func preload(p *page, i int, u, media string) []byte {
	var b strings.Builder
	b.WriteString(`<link rel="preload" as="image"`)
	if i >= 0 {
		img := p.images[i].attributes
		src := img["src"]
		set, sizes := img["srcset"], img["sizes"]
		if contains(srcset(img["data-srcset"]), u) || (img["data-src"] != "" && img["srcset"] == "") {
			// Lazy loaded by a plugin, whose placeholder isn't the image
			src, set, sizes = img["data-src"], img["data-srcset"], img["data-sizes"]
		}
		if src == "" {
			src = u
		}
		if set != "" {
			// Without an href, which browsers fetch as well as the size
			// they pick from imagesrcset (Chrome does)
			b.WriteString(` imagesrcset="` + html.EscapeString(set) + `"`)
			if sizes != "" && sizes != "auto" {
				b.WriteString(` imagesizes="` + html.EscapeString(sizes) + `"`)
			}
		} else {
			b.WriteString(` href="` + html.EscapeString(src) + `"`)
		}
	} else {
		b.WriteString(` href="` + html.EscapeString(u) + `"`)
	}
	b.WriteString(` fetchpriority="high"`)
	if media != "" {
		b.WriteString(` media="` + media + `"`)
	}
	b.WriteString(` data-honey-main-image>`)
	return []byte(b.String())
}

// eager returns the edit which makes img load straight away, and with high
// priority, if it doesn't already
func eager(p *page, img image) (edit, bool) {
	lazy := strings.EqualFold(strings.TrimSpace(img.attributes["loading"]), "lazy")
	_, priority := img.attributes["fetchpriority"]
	if !lazy && priority {
		return edit{}, false
	}
	attrs := p.body[img.attrs[0]:img.attrs[1]]
	if lazy {
		attrs = withoutAttributes(attrs, "loading")
	}
	if !priority {
		attrs = append([]byte(` fetchpriority="high"`), attrs...)
	}
	text := append([]byte("<img"), attrs...)
	text = append(text, p.body[img.attrs[1]:img.end]...)
	return edit{start: img.start, end: img.end, text: text}, true
}

// beaconScript reports the page's main image - the element of its last
// Largest Contentful Paint - once the page has loaded (or when it's hidden,
// if that's sooner), if NEED includes the visitor's kind of screen
const beaconScript = `(function(){var n=NEED,v=innerWidth<WIDTH?"mobile":"desktop",l,sent=false;if(n.indexOf(v)<0||!window.PerformanceObserver||!navigator.sendBeacon)return;
try{new PerformanceObserver(function(x){var e=x.getEntries();l=e[e.length-1]}).observe({type:"largest-contentful-paint",buffered:true})}catch(e){return}
function send(){if(sent||!l)return;sent=true;navigator.sendBeacon("PATH",JSON.stringify({p:location.pathname,v:v,u:l.url||""}))}
addEventListener("visibilitychange",function(){if(document.visibilityState==="hidden")send()});addEventListener("load",function(){setTimeout(send,2500)})})();`

func beacon(need []string) []byte {
	list, _ := json.Marshal(need)
	s := strings.NewReplacer("NEED", string(list), "WIDTH", fmt.Sprint(mobileWidth), "PATH", BeaconPath).Replace(beaconScript)
	return []byte(`<script id="honey-main-image">` + s + `</script>`)
}

// Middleware handles reports of pages' main images (see BeaconPath) and
// requests for self-hosted fonts (see FontsPath), and passes everything else
// on to next
func (o *Optimizer) Middleware(next http.Handler) http.Handler {
	if o == nil || (o.images == nil && o.fonts == nil) {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case o.images != nil && r.URL.Path == BeaconPath:
			o.report(w, r)
		case o.fonts != nil && strings.HasPrefix(r.URL.Path, FontsPath):
			o.fonts.serve(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// report records a page's main image, as a browser reports it
func (o *Optimizer) report(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var report struct {
		Path     string `json:"p"`
		Viewport string `json:"v"`
		URL      string `json:"u"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxReport+1))
	if len(body) > maxReport || json.Unmarshal(body, &report) != nil ||
		!strings.HasPrefix(report.Path, "/") || len(report.Path) > 2048 {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	v := -1
	for i, vp := range viewports {
		if vp.name == report.Viewport {
			v = i
		}
	}
	if v < 0 {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	// Whether or not it's news: there's nothing for the browser to do
	w.WriteHeader(http.StatusNoContent)
	// As the page was cached
	host := r.Host
	if o.opts.BackendHost != "" {
		host = o.opts.BackendHost
	}
	u := ""
	if report.URL != "" {
		if u = normalize(report.URL, host, nil); u == "" {
			return
		}
	}
	news, complete := o.images.learn(pageKey(host, report.Path), v, u)
	if !news {
		return
	}
	if u != "" {
		o.opts.Events.Add(events.Info, "main-image", time.Minute, func(e *events.Event) {
			text := fmt.Sprintf("learned the main image of %s on %s screens: %s", report.Path, report.Viewport, u)
			if e.Count > 1 {
				text = fmt.Sprintf("learned the main images of %d pages, most recently %s on %s screens: %s", e.Count, report.Path, report.Viewport, u)
			}
			e.Text = text
		})
	}
	// Cache it again with the image preloaded, or without the script
	// which reports it
	if o.opts.Refresh != nil && (u != "" || complete) {
		o.opts.Refresh(host, report.Path)
	}
}
