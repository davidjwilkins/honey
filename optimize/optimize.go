// Package optimize changes pages so that browsers load them faster, once,
// as they're cached (see cache.Options.RewritePage):
//
//   - Delaying third-party scripts: analytics, chat, ad and other scripts
//     which aren't needed to show the page are disabled, and run - in
//     order - when the visitor first interacts with it (see scripts.go).
//   - Preloading the main image: visitors' browsers report each page's
//     largest image (its Largest Contentful Paint), and the page is then
//     cached with a preload for it at the top of its <head>, and without
//     lazy loading it (see lcp.go).
package optimize

import (
	"bytes"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/davidjwilkins/honey/events"
)

// Options configures what pages are changed for
type Options struct {
	// DelayScripts delays the scripts which match DefaultDelayed or
	// DelayMatching, and not KeepMatching, until the visitor interacts
	// with the page
	DelayScripts bool
	// DelayMatching are more scripts to delay: text found in their src, id
	// or (for inline scripts) code
	DelayMatching []string
	// KeepMatching are scripts never to delay, the same way
	KeepMatching []string
	// DelayTimeout, if set, runs delayed scripts after this long even if
	// the visitor hasn't interacted with the page
	DelayTimeout time.Duration
	// PreloadMainImage learns each page's main image from visitors'
	// browsers, and preloads it
	PreloadMainImage bool
	// BackendHost, if set, is the Host pages are cached under, rather than
	// the one visitors send (see fetch.Options.BackendHost)
	BackendHost string
	// Refresh soft purges a page (by host and path), once its main image
	// has been learned, so that it's cached again with it preloaded
	Refresh func(host, path string)
	// Events, if set, is where learned main images are logged
	Events *events.Log

	// now is the time (for tests)
	now func() time.Time
}

// Optimizer changes pages as Options say
type Optimizer struct {
	opts   Options
	delay  []string
	keep   []string
	images *imageStore
}

// New returns an Optimizer, or nil if opts turn nothing on
func New(opts Options) *Optimizer {
	if !opts.DelayScripts && !opts.PreloadMainImage {
		return nil
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	o := &Optimizer{opts: opts}
	for _, s := range append(append([]string(nil), DefaultDelayed...), opts.DelayMatching...) {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			o.delay = append(o.delay, s)
		}
	}
	for _, s := range opts.KeepMatching {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			o.keep = append(o.keep, s)
		}
	}
	if opts.PreloadMainImage {
		o.images = newImageStore(maxImagePages)
	}
	return o
}

var (
	headStart   = regexp.MustCompile(`(?i)<head(?:\s[^>]*)?>`)
	metaCharset = regexp.MustCompile(`(?i)<meta\s[^>]*charset[^>]*>`)
	headEnd     = regexp.MustCompile(`(?i)</head\s*>`)
	bodyEnd     = regexp.MustCompile(`(?i)</body\s*>`)
	// tokens are the parts of a page which matter: comments, and
	// elements whose content isn't HTML (which are skipped), scripts, and
	// images
	tokens = regexp.MustCompile(`(?is)<!--.*?-->|<script\b([^>]*)>(.*?)</script\s*>|<(?:noscript|template|textarea|style)\b.*?</(?:noscript|template|textarea|style)\s*>|<img\b([^>]*)>`)
)

// Rewrite returns the page body - a response to r.Request - changed as o's
// options say, or body itself if it isn't a whole HTML page which can be
// changed.
func (o *Optimizer) Rewrite(r *http.Response, body []byte) []byte {
	if o == nil || !rewritable(r, body) {
		return body
	}
	page := scan(body)
	var edits []edit
	if o.opts.DelayScripts {
		edits = append(edits, o.delayScripts(page)...)
	}
	var head, tail []byte
	if o.images != nil {
		var imageEdits []edit
		head, tail, imageEdits = o.mainImage(r.Request, page)
		edits = append(edits, imageEdits...)
	}
	delayed := false
	for _, e := range edits {
		delayed = delayed || e.delayed
	}
	if delayed {
		tail = append(tail, delayLoader(o.opts.DelayTimeout)...)
	}
	if len(edits) == 0 && len(head) == 0 && len(tail) == 0 {
		return body
	}
	// Where the preloads go, at the top of the <head>
	at := headStart.FindIndex(body)
	if m := metaCharset.FindIndex(body[at[1]:headEnd.FindIndex(body)[0]]); m != nil {
		at = []int{at[1] + m[0], at[1] + m[1]}
	}
	if len(head) > 0 {
		edits = append(edits, edit{start: at[1], end: at[1], text: head})
	}
	if len(tail) > 0 {
		ends := bodyEnd.FindAllIndex(body, -1)
		end := ends[len(ends)-1][0]
		edits = append(edits, edit{start: end, end: end, text: tail})
	}
	return apply(body, edits)
}

// rewritable returns whether r, with body, is a whole HTML page which can
// be changed: a successful, uncompressed response to a GET for a page (not
// an include), with a <head> and </body>, and without a Content Security
// Policy which the scripts Honey adds might break
func rewritable(r *http.Response, body []byte) bool {
	if r.StatusCode != http.StatusOK || r.Request == nil || r.Request.Method != http.MethodGet || r.Header.Get("Content-Encoding") != "" {
		return false
	}
	if ct := strings.ToLower(r.Header.Get("Content-Type")); !strings.HasPrefix(ct, "text/html") {
		return false
	}
	if r.Request.URL != nil && r.Request.URL.Query().Has("honey-esi") {
		return false
	}
	if r.Header.Get("Content-Security-Policy") != "" {
		return false
	}
	head := headStart.FindIndex(body)
	end := headEnd.FindIndex(body)
	return head != nil && end != nil && head[1] <= end[0] && bodyEnd.Match(body)
}

// page is what's found in a page's HTML: its scripts and images, outside
// comments and the like
type page struct {
	body    []byte
	scripts []script
	images  []image
}

type script struct {
	// start and end are where the element is in the page, and attrs and
	// content where its attributes and content are
	start, end   int
	attrs        [2]int
	content      [2]int
	attributeMap map[string]string
}

type image struct {
	start, end int
	attrs      [2]int
	attributes map[string]string
}

// scan finds the scripts and images in a page
func scan(body []byte) *page {
	p := &page{body: body}
	for _, m := range tokens.FindAllSubmatchIndex(body, -1) {
		switch {
		case m[2] >= 0:
			p.scripts = append(p.scripts, script{
				start: m[0], end: m[1], attrs: [2]int{m[2], m[3]}, content: [2]int{m[4], m[5]},
				attributeMap: attributes(body[m[2]:m[3]]),
			})
		case m[6] >= 0:
			p.images = append(p.images, image{start: m[0], end: m[1], attrs: [2]int{m[6], m[7]},
				attributes: attributes(body[m[6]:m[7]])})
		}
	}
	return p
}

// edit replaces body[start:end] with text
type edit struct {
	start, end int
	text       []byte
	// delayed is whether it delays a script
	delayed bool
}

// apply makes edits to body, which don't overlap, and returns the result
func apply(body []byte, edits []edit) []byte {
	sortEdits(edits)
	var b bytes.Buffer
	b.Grow(len(body) + 4096)
	last := 0
	for _, e := range edits {
		b.Write(body[last:e.start])
		b.Write(e.text)
		last = e.end
	}
	b.Write(body[last:])
	return b.Bytes()
}

func sortEdits(edits []edit) {
	// Insertion sort: there are few, and mostly in order.  An insertion
	// goes before a replacement starting at the same place.
	before := func(a, b edit) bool { return a.start < b.start || (a.start == b.start && a.end < b.end) }
	for i := 1; i < len(edits); i++ {
		for j := i; j > 0 && before(edits[j], edits[j-1]); j-- {
			edits[j], edits[j-1] = edits[j-1], edits[j]
		}
	}
}
