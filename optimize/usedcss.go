package optimize

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/andybalholm/cascadia"
	"github.com/davidjwilkins/honey/events"
	xhtml "golang.org/x/net/html"
)

// Removing unused CSS: a browser can't show a page until it has loaded
// every stylesheet in its <head>, though a theme's or page builder's
// stylesheets are mostly rules for elements which aren't on the page.  So
// Honey fetches the site's stylesheets itself (the first time a page links
// to each, and the page is refreshed once they're fetched) and, as each
// page is cached, puts the rules it uses - those whose selectors match an
// element of its HTML - in a <style> in place of each <link>, and has the
// browser load the whole stylesheet once the page is shown: so it's shown
// sooner, and anything the HTML alone doesn't show (rules for elements
// scripts add, say) is styled moments later.
//
// Rules for what a visitor does - :hover, :focus and the like - and pseudo
// elements are kept if what they're for is on the page; @font-face rules
// are all kept (fonts are only loaded if they're used), and @keyframes if a
// kept rule names them.  Stylesheets from other sites, for print, which
// @import others or are too big are left alone, as are those of pages with
// too much used CSS to put in them.

// UsedCSSAttribute marks the <style>s which hold a stylesheet's used CSS
const UsedCSSAttribute = "data-honey-used-css"

const (
	// maxStylesheet bounds the size of a stylesheet which is used
	maxStylesheet = 1 << 20
	// maxUsedCSS bounds how much used CSS is put in a page
	maxUsedCSS = 150 << 10
	// maxStylesheets and maxStylesheetBytes bound how many stylesheets are
	// kept, and their size
	maxStylesheets     = 2000
	maxStylesheetBytes = 64 << 20
	// maxWaiting bounds how many pages wait for stylesheets to be fetched
	maxWaiting = 10000
	// stylesheetLifetime is how long a stylesheet is used before it's
	// fetched again, in case it's changed, and stylesheetRetry how long
	// before one which couldn't be used is tried again
	stylesheetLifetime = time.Hour
	stylesheetRetry    = 10 * time.Minute
	// stylesheetFetches is how many stylesheets are fetched at once
	stylesheetFetches = 4
)

// DefaultKeepCSS are rules always kept (by text in their selectors), as
// they're for classes scripts commonly add as the page loads: before it's
// shown, or while the whole stylesheets load
var DefaultKeepCSS = []string{
	".js", ".wf-active", ".wf-loading", ".fonts-loaded", ".lazyloaded",
	".swiper-", ".slick-", ".owl-", "-initialized", ".is-loaded", ".e--ua-",
}

// cssStore keeps the site's stylesheets, as rules to pick from
type cssStore struct {
	opts *Options
	keep []string
	sem  chan struct{}

	mu      sync.Mutex
	sheets  map[string]*stylesheet
	bytes   int
	waiting map[string]*waitingPage
}

type stylesheet struct {
	// fetching is whether it's being fetched
	fetching bool
	fetched  time.Time
	// rules are its rules, or nil if it can't be used (why says why), and
	// size the stylesheet's size
	rules []usedRule
	why   string
	size  int
}

// waitingPage is a page waiting for stylesheets to be fetched, to be
// refreshed once they are
type waitingPage struct {
	host, path string
	sheets     map[string]bool
	// useful is whether any of them can be used
	useful bool
}

// usedRule is a rule of a stylesheet, ready to be checked against pages
type usedRule struct {
	// For a style rule, its selectors, and its text
	selectors []usedSelector
	text      string
	// For an at-rule with rules in its block, its text up to the block,
	// and the rules; or (always) the text of one which is always kept;
	// or the name of @keyframes (which are kept if a kept rule names
	// them)
	open      string
	children  []usedRule
	always    bool
	keyframes string
}

// usedSelector is a selector, without what it says about what the visitor
// does (:hover and the like) or pseudo elements
type usedSelector struct {
	// classes and ids are those an element on the page must have for it
	// to match
	classes, ids []string
	// sel matches it, or is nil if it's assumed to match (it's kept, or
	// can't be understood)
	sel cascadia.Sel
}

func newCSSStore(opts *Options) *cssStore {
	s := &cssStore{opts: opts, sem: make(chan struct{}, stylesheetFetches),
		sheets: map[string]*stylesheet{}, waiting: map[string]*waitingPage{}}
	for _, k := range append(append([]string(nil), DefaultKeepCSS...), opts.KeepCSSMatching...) {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			s.keep = append(s.keep, k)
		}
	}
	return s
}

// sheetLink is a link to one of the site's stylesheets on a page
type sheetLink struct {
	link link
	// uri is the stylesheet's path and query, and key what it's kept as
	uri, key string
}

// usedCSS returns the edits which put the CSS r's page p uses in place of
// its stylesheets, those which have been fetched (and fetches the rest)
func (o *Optimizer) usedCSS(r *http.Request, p *page) []edit {
	if r == nil || r.URL == nil {
		return nil
	}
	host, base := r.Host, &url.URL{Path: r.URL.Path}
	var links []sheetLink
	for _, l := range p.links {
		if uri := o.stylesheet(l, host, base); uri != "" {
			links = append(links, sheetLink{link: l, uri: uri, key: pageKey(host, uri)})
		}
	}
	if len(links) == 0 {
		return nil
	}
	sheets := o.css.get(host, r.URL.Path, links)
	var doc *usedPage
	var edits []edit
	total := 0
	for i, l := range links {
		if sheets[i] == nil {
			continue
		}
		if doc == nil {
			if doc = parsePage(p.body); doc == nil {
				return nil
			}
		}
		used := usedRules(sheets[i], doc)
		if total += len(used); total > maxUsedCSS {
			// The rest stay as they are
			break
		}
		edits = append(edits, edit{start: l.link.start, end: l.link.end, text: usedStyle(p, l.link, used)})
	}
	return edits
}

// stylesheet returns the path (and query) of the site's stylesheet which l
// links to, on a page on host at base, if it's one whose used CSS can be put
// in the page
func (o *Optimizer) stylesheet(l link, host string, base *url.URL) string {
	a := l.attributes
	rel := " " + strings.ToLower(strings.Join(strings.Fields(a["rel"]), " ")) + " "
	if !strings.Contains(rel, " stylesheet ") || strings.Contains(rel, " alternate ") {
		return ""
	}
	if _, ok := a["disabled"]; ok {
		return ""
	}
	if _, ok := a["onload"]; ok {
		// Already loaded some other way
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(a["media"])) {
	case "", "all", "screen":
	default:
		return ""
	}
	href := strings.TrimSpace(a["href"])
	if href == "" || strings.HasPrefix(href, FontsPath) {
		return ""
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	u = base.ResolveReference(u)
	if u.Host != "" && !o.siteHost(u.Host, host) {
		return ""
	}
	if u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	if !strings.HasPrefix(u.Path, "/") || len(u.RequestURI()) > 2048 {
		return ""
	}
	return u.RequestURI()
}

// siteHost returns whether h (from a URL on a page on host) is the site's
func (o *Optimizer) siteHost(h, host string) bool {
	if strings.EqualFold(h, host) {
		return true
	}
	name := h
	if n, _, err := net.SplitHostPort(h); err == nil {
		name = n
	}
	for _, s := range o.opts.Hosts {
		if strings.EqualFold(h, s) || strings.EqualFold(name, s) {
			return true
		}
	}
	return false
}

// usedStyle returns what replaces the link l: the used CSS, then the link,
// changed to load the whole stylesheet once the page is shown (and as it
// was, for browsers without scripts)
func usedStyle(p *page, l link, used string) []byte {
	var b bytes.Buffer
	b.WriteString(`<style ` + UsedCSSAttribute + `="` + html.EscapeString(l.attributes["href"]) + `">`)
	b.WriteString(styleEnd.ReplaceAllString(used, `<\/$1`))
	b.WriteString(`</style>`)
	media := strings.TrimSpace(l.attributes["media"])
	if media == "" {
		media = "all"
	}
	attrs := bytes.TrimRight(withoutAttributes(p.body[l.attrs[0]:l.attrs[1]], "media"), " \t\r\n/")
	b.WriteString("<link")
	b.Write(attrs)
	b.WriteString(` media="print" onload="this.onload=null;this.media='` + html.EscapeString(media) + `'">`)
	b.WriteString("<noscript>")
	b.Write(p.body[l.start:l.end])
	b.WriteString("</noscript>")
	return b.Bytes()
}

// get returns the rules of the stylesheets links are to, or nil for those
// which aren't fetched yet or can't be used; and fetches those which need
// it, with the page at path on host refreshed once they are.  (Rules,
// once made, don't change.)
func (s *cssStore) get(host, path string, links []sheetLink) [][]usedRule {
	now := s.opts.now()
	sheets := make([][]usedRule, len(links))
	page := pageKey(host, path)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, l := range links {
		sheet := s.sheets[l.key]
		switch {
		case sheet == nil:
			if len(s.sheets) >= maxStylesheets {
				s.evict(0)
			}
			sheet = &stylesheet{}
			s.sheets[l.key] = sheet
		case sheet.fetching:
		case sheet.rules != nil:
			sheets[i] = sheet.rules
			if now.Sub(sheet.fetched) >= stylesheetLifetime {
				// It's used while it's fetched again
				sheet.fetching = true
				go s.fetch(host, l.uri, l.key)
			}
			continue
		case now.Sub(sheet.fetched) < stylesheetRetry:
			// It can't be used
			continue
		}
		// The page waits for it
		w := s.waiting[page]
		if w == nil {
			if len(s.waiting) >= maxWaiting {
				continue
			}
			w = &waitingPage{host: host, path: path, sheets: map[string]bool{}}
			s.waiting[page] = w
		}
		w.sheets[l.key] = true
		if !sheet.fetching {
			sheet.fetching = true
			go s.fetch(host, l.uri, l.key)
		}
	}
	return sheets
}

// fetch fetches the stylesheet at uri on host, kept as key
func (s *cssStore) fetch(host, uri, key string) {
	s.sem <- struct{}{}
	rules, why, size := s.load(host, uri)
	<-s.sem

	s.mu.Lock()
	sheet := s.sheets[key]
	if sheet == nil {
		sheet = &stylesheet{}
		s.sheets[key] = sheet
	}
	sheet.fetching = false
	sheet.fetched = s.opts.now()
	if rules != nil || sheet.rules == nil {
		// (An old stylesheet is better than none)
		s.bytes += size - sheet.size
		sheet.rules, sheet.why, sheet.size = rules, why, size
		if s.bytes > maxStylesheetBytes {
			s.evict(maxStylesheetBytes)
		}
	}
	var refresh []*waitingPage
	for k, w := range s.waiting {
		if !w.sheets[key] {
			continue
		}
		delete(w.sheets, key)
		w.useful = w.useful || sheet.rules != nil
		if len(w.sheets) == 0 {
			delete(s.waiting, k)
			if w.useful {
				refresh = append(refresh, w)
			}
		}
	}
	s.mu.Unlock()

	if rules == nil {
		s.opts.Events.Add(events.Info, "unused-css "+uri, time.Hour, events.Text(fmt.Sprintf("stylesheet %s isn't changed into each page's used CSS: %s", uri, why)))
	}
	for _, w := range refresh {
		if s.opts.Refresh != nil {
			s.opts.Refresh(w.host, w.path)
		}
	}
}

// evict forgets stylesheets (not being fetched) until they're within size
// bytes, or one if size is 0
func (s *cssStore) evict(size int) {
	for k, sheet := range s.sheets {
		if sheet.fetching {
			continue
		}
		delete(s.sheets, k)
		s.bytes -= sheet.size
		if size == 0 || s.bytes <= size {
			return
		}
	}
}

// load fetches the stylesheet at uri on host, and returns its rules (and
// roughly how much memory they take), or nil and why it can't be used
func (s *cssStore) load(host, uri string) ([]usedRule, string, int) {
	if s.opts.Fetch == nil {
		return nil, "there's no backend to fetch it from", 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return nil, err.Error(), 0
	}
	req.Host = host
	req.Header.Set("Accept", "text/css,*/*;q=0.1")
	resp, err := s.opts.Fetch(req)
	if err != nil {
		return nil, "fetching it: " + err.Error(), 0
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxStylesheet+1))
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	switch {
	case err != nil:
		return nil, "fetching it: " + err.Error(), 0
	case resp.StatusCode != http.StatusOK:
		return nil, "the backend said " + resp.Status, 0
	case resp.Header.Get("Content-Encoding") != "" && !strings.EqualFold(resp.Header.Get("Content-Encoding"), "identity"):
		return nil, "it's compressed", 0
	case !strings.Contains(contentType, "css"):
		return nil, "it isn't CSS", 0
	case len(body) > maxStylesheet:
		return nil, "it's too big", 0
	case !utf8.Valid(body) || (strings.Contains(contentType, "charset=") && !strings.Contains(contentType, "charset=utf-8")):
		// (Pages are almost always UTF-8, and it would be garbled in one)
		return nil, "it isn't UTF-8", 0
	}
	nodes := parseCSS(absoluteURLs(string(body), uri))
	rules := make([]usedRule, 0, len(nodes))
	for _, n := range nodes {
		if n.atRule == "import" {
			return nil, "it @imports another", 0
		}
		if r, ok := s.rule(n); ok {
			rules = append(rules, r)
		}
	}
	// Its text, twice over, and its selectors
	return rules, "", 3 * len(body)
}

// rule prepares a stylesheet's rule to be checked against pages, unless
// it's one which is never needed
func (s *cssStore) rule(n cssNode) (usedRule, bool) {
	var b strings.Builder
	switch {
	case n.atRule == "" && n.selectors != "":
		r := usedRule{}
		for _, sel := range splitSelectors(n.selectors) {
			r.selectors = append(r.selectors, s.selector(sel))
		}
		serialize(&b, []cssNode{n})
		r.text = b.String()
		return r, true
	case n.atRule == "":
		return usedRule{}, false
	case n.atRule == "charset":
		return usedRule{}, false
	case n.atRule == "media" && strings.EqualFold(n.prelude, "print"):
		return usedRule{}, false
	case n.block && nestedAtRules[n.atRule]:
		r := usedRule{open: "@" + n.atRule}
		if n.prelude != "" {
			r.open += " " + n.prelude
		}
		r.open += "{"
		for _, child := range n.children {
			if c, ok := s.rule(child); ok {
				r.children = append(r.children, c)
			}
		}
		return r, len(r.children) > 0
	}
	serialize(&b, []cssNode{n})
	r := usedRule{text: b.String(), always: true}
	if strings.HasSuffix(n.atRule, "keyframes") {
		r.always, r.keyframes = false, strings.Trim(n.prelude, `"' `)
	}
	return r, true
}

// selector prepares a selector to be checked against pages
func (s *cssStore) selector(sel string) usedSelector {
	lower := strings.ToLower(sel)
	for _, k := range s.keep {
		if strings.Contains(lower, k) {
			return usedSelector{}
		}
	}
	sel = withoutStates(sel)
	u := usedSelector{}
	u.classes, u.ids = requiredNames(sel)
	if compiled, err := cascadia.Parse(sel); err == nil {
		u.sel = compiled
	}
	return u
}

// usedPage is a page's HTML, as its stylesheets' rules are checked against
// it
type usedPage struct {
	doc          *xhtml.Node
	classes, ids map[string]bool
}

func parsePage(body []byte) *usedPage {
	doc, err := xhtml.Parse(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	p := &usedPage{doc: doc, classes: map[string]bool{}, ids: map[string]bool{}}
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode {
			for _, a := range n.Attr {
				switch a.Key {
				case "class":
					for _, c := range strings.Fields(a.Val) {
						p.classes[c] = true
					}
				case "id":
					p.ids[a.Val] = true
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return p
}

// matches returns whether sel matches an element of p
func (sel usedSelector) matches(p *usedPage) bool {
	for _, c := range sel.classes {
		if !p.classes[c] {
			return false
		}
	}
	for _, id := range sel.ids {
		if !p.ids[id] {
			return false
		}
	}
	return sel.sel == nil || cascadia.Query(p.doc, sel.sel) != nil
}

// usedRules returns the CSS of a stylesheet's rules which page p uses
func usedRules(rules []usedRule, p *usedPage) string {
	var kept []usedRule
	var named strings.Builder
	for _, r := range rules {
		if k, ok := keptRule(r, p, &named); ok {
			kept = append(kept, k)
		}
	}
	var b strings.Builder
	writeRules(&b, kept, named.String())
	return b.String()
}

// keptRule returns r as much of it as p uses, if it uses any, and adds the
// text of the style rules kept to named
func keptRule(r usedRule, p *usedPage, named *strings.Builder) (usedRule, bool) {
	switch {
	case r.always || r.keyframes != "":
		return r, true
	case r.open != "":
		k := usedRule{open: r.open}
		for _, c := range r.children {
			if kc, ok := keptRule(c, p, named); ok {
				k.children = append(k.children, kc)
			}
		}
		return k, len(k.children) > 0
	}
	for _, sel := range r.selectors {
		if sel.matches(p) {
			named.WriteString(r.text)
			return r, true
		}
	}
	return usedRule{}, false
}

// writeRules writes rules, without @keyframes which named doesn't name
func writeRules(b *strings.Builder, rules []usedRule, named string) {
	for _, r := range rules {
		switch {
		case r.keyframes != "":
			if containsWord(named, r.keyframes) {
				b.WriteString(r.text)
			}
		case r.open != "":
			b.WriteString(r.open)
			writeRules(b, r.children, named)
			b.WriteByte('}')
		default:
			b.WriteString(r.text)
		}
	}
}

// containsWord returns whether s has word in it, not as part of a longer
// name
func containsWord(s, word string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], word)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(word)
		if (start == 0 || !isNameByte(s[start-1])) && (end == len(s) || !isNameByte(s[end])) {
			return true
		}
		i = end
	}
}

// statePseudoClasses are pseudo classes for what the visitor does (or
// what a form's fields hold), which are left out of selectors: the rule
// is kept if what it's for is on the page
var statePseudoClasses = map[string]bool{
	"hover": true, "focus": true, "focus-visible": true, "focus-within": true, "active": true,
	"visited": true, "link": true, "any-link": true, "target": true, "target-within": true,
	"checked": true, "indeterminate": true, "default": true, "disabled": true, "enabled": true,
	"valid": true, "invalid": true, "user-valid": true, "user-invalid": true, "in-range": true,
	"out-of-range": true, "required": true, "optional": true, "read-only": true, "read-write": true,
	"placeholder-shown": true, "autofill": true, "blank": true,
	"open": true, "closed": true, "modal": true, "popover-open": true, "fullscreen": true,
	"picture-in-picture": true, "playing": true, "paused": true, "seeking": true, "buffering": true,
	"stalled": true, "muted": true, "volume-locked": true, "current": true, "past": true, "future": true,
	"defined": true, "state": true, "local-link": true,
	// Pseudo elements, written the old way
	"before": true, "after": true, "first-line": true, "first-letter": true,
}

// withoutStates returns a selector without pseudo elements, or pseudo
// classes for what the visitor does
func withoutStates(sel string) string {
	var b strings.Builder
	for i := 0; i < len(sel); {
		c := sel[i]
		switch {
		case c == '"' || c == '\'':
			end := skipString(sel, i)
			b.WriteString(sel[i:end])
			i = end
			continue
		case c == '\\' && i+1 < len(sel):
			b.WriteString(sel[i : i+2])
			i += 2
			continue
		case c == '[':
			end := i
			for end < len(sel) && sel[end] != ']' {
				if sel[end] == '"' || sel[end] == '\'' {
					end = skipString(sel, end)
					continue
				}
				end++
			}
			if end < len(sel) {
				end++
			}
			b.WriteString(sel[i:end])
			i = end
			continue
		case c != ':':
			b.WriteByte(c)
			i++
			continue
		}
		element := strings.HasPrefix(sel[i:], "::")
		start := i + 1
		if element {
			start++
		}
		end := start
		for end < len(sel) && isNameByte(sel[end]) {
			end++
		}
		name := strings.ToLower(sel[start:end])
		if end < len(sel) && sel[end] == '(' {
			end = skipParens(sel, end)
		}
		if !element && !statePseudoClasses[name] && !strings.HasPrefix(name, "-") {
			b.WriteString(sel[i:end])
			i = end
			continue
		}
		// Left out, leaving "*" if nothing else is left of its part of the
		// selector
		out := b.String()
		if last := strings.TrimRight(out, " \t\n"); last == "" || strings.ContainsAny(last[len(last)-1:], ">+~(,") || len(last) < len(out) {
			if end == len(sel) || strings.ContainsAny(sel[end:end+1], " \t\n>+~),") {
				b.WriteByte('*')
			}
		}
		i = end
	}
	return b.String()
}

// requiredNames returns the classes and ids an element must have for a
// selector to match it: those not in brackets or parentheses (as in
// :not(.x) or :is(.x, .y))
func requiredNames(sel string) (classes, ids []string) {
	depth := 0
	for i := 0; i < len(sel); i++ {
		switch c := sel[i]; c {
		case '"', '\'':
			i = skipString(sel, i) - 1
		case '\\':
			i++
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case '.', '#':
			if depth != 0 {
				continue
			}
			name, end, ok := ident(sel, i+1)
			if !ok {
				continue
			}
			if c == '.' {
				classes = append(classes, name)
			} else {
				ids = append(ids, name)
			}
			i = end - 1
		}
	}
	return classes, ids
}

// ident returns the identifier starting at s[i], unescaped, and where it
// ends; or false if it has an escape which isn't simple (a hex code)
func ident(s string, i int) (string, int, bool) {
	var b strings.Builder
	for i < len(s) {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			if isHex(s[i+1]) || s[i+1] == '\n' {
				return "", i, false
			}
			b.WriteByte(s[i+1])
			i += 2
		case isNameByte(c):
			b.WriteByte(c)
			i++
		default:
			return b.String(), i, b.Len() > 0
		}
	}
	return b.String(), i, b.Len() > 0
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// styleEnd finds what would end a <style> early
var styleEnd = regexp.MustCompile(`(?i)</(style)`)

// cssURL finds url()s in CSS
var cssURL = regexp.MustCompile(`(?i)\burl\(\s*(?:"([^"]*)"|'([^']*)'|([^)'"\s]*))\s*\)`)

// absoluteURLs returns css, from the stylesheet at uri, with its relative
// url()s made relative to the site instead, so that they work in a page
func absoluteURLs(css, uri string) string {
	base, err := url.Parse(uri)
	if err != nil {
		return css
	}
	return cssURL.ReplaceAllStringFunc(css, func(m string) string {
		sub := cssURL.FindStringSubmatch(m)
		raw := sub[1] + sub[2] + sub[3]
		if raw == "" || strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "#") || strings.HasPrefix(raw, "data:") || strings.Contains(raw, "://") {
			return m
		}
		if i := strings.IndexByte(raw, ':'); i >= 0 && !strings.ContainsAny(raw[:i], "/?#") {
			// Some other scheme
			return m
		}
		u, err := url.Parse(raw)
		if err != nil {
			return m
		}
		return `url("` + strings.ReplaceAll(base.ResolveReference(u).String(), `"`, `%22`) + `")`
	})
}
