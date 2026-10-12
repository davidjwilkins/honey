package cache

import (
	"html"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

// Early Hints (RFC 8297) let a server tell the browser which stylesheets,
// fonts and scripts a page needs - with a "103 Early Hints" response of
// Link headers - before the page itself is ready, so that the browser can
// fetch them while it waits.  The cache works out each cached page's from
// its <head>, for Honey to send while a request for it waits for the
// backend (see fetch).

const (
	// maxHints is how many Link headers a page's hints have at most
	maxHints = 10
	// maxHintBytes bounds their total length
	maxHintBytes = 4 << 10
	// maxHeadBytes is how far into a page its </head> is looked for (which
	// may hold up to 150KB of a page's used CSS: see optimize.usedcss.go)
	maxHeadBytes = 256 << 10
	// hintPages is how many of a host's recently cached pages its common
	// hints are worked out from, and minHintPages how many it needs
	hintPages    = 8
	minHintPages = 3
	// maxHintHosts bounds how many hosts' recent pages are kept
	maxHintHosts = 100
)

var (
	headEnd  = regexp.MustCompile(`(?i)</head\s*>`)
	headTag  = regexp.MustCompile(`(?is)<!--.*?-->|<(link|script)\s([^>]*)>`)
	tagAttrs = regexp.MustCompile(`([a-zA-Z_:][-a-zA-Z0-9_:.]*)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'=<>` + "`" + `]+)))?`)
)

// EarlyHints returns the Link header values to send as Early Hints for the
// cached page resp: the stylesheets, scripts and preloads in its <head>.
// It returns nil if resp isn't a cached HTML page, or has none.
func EarlyHints(resp Response) []string {
	r, ok := resp.(*responseImpl)
	if !ok || r.statusCode != http.StatusOK || !isHTML(r.headers) {
		return nil
	}
	r.hintsOnce.Do(func() { r.hints = parseHints(r.body) })
	return r.hints
}

// parseHints returns the Early Hints for a page's HTML (see EarlyHints)
func parseHints(body []byte) []string {
	if len(body) > maxHeadBytes {
		body = body[:maxHeadBytes]
	}
	end := headEnd.FindIndex(body)
	if end == nil {
		// Without the whole <head>, it can't be told what's in it
		return nil
	}
	var hints []string
	seen := map[string]bool{}
	size := 0
	for _, m := range headTag.FindAllSubmatch(body[:end[0]], -1) {
		if len(m[1]) == 0 {
			continue // a comment
		}
		hint := hintFor(strings.ToLower(string(m[1])), attributes(m[2]))
		if hint == "" || seen[hint] {
			continue
		}
		if len(hints) == maxHints || size+len(hint) > maxHintBytes {
			break
		}
		seen[hint] = true
		size += len(hint)
		hints = append(hints, hint)
	}
	return hints
}

func attributes(tag []byte) map[string]string {
	attrs := map[string]string{}
	for _, a := range tagAttrs.FindAllSubmatch(tag, -1) {
		name := strings.ToLower(string(a[1]))
		if _, dup := attrs[name]; !dup {
			attrs[name] = html.UnescapeString(string(a[2]) + string(a[3]) + string(a[4]))
		}
	}
	return attrs
}

// hintFor returns the Link header value to hint a <link> or <script> tag
// with attrs, or "" if it isn't one worth hinting
func hintFor(tag string, attrs map[string]string) string {
	var target, params string
	if tag == "script" {
		// Only scripts which hold up the page: classic ones, without
		// async or defer
		_, async := attrs["async"]
		_, deferred := attrs["defer"]
		if async || deferred || (attrs["type"] != "" && !strings.Contains(attrs["type"], "javascript")) {
			return ""
		}
		target, params = attrs["src"], "; rel=preload; as=script"
	} else {
		rels := strings.Fields(strings.ToLower(attrs["rel"]))
		has := func(rel string) bool {
			for _, r := range rels {
				if r == rel {
					return true
				}
			}
			return false
		}
		target = attrs["href"]
		switch {
		case has("stylesheet") && !has("alternate"):
			// Not stylesheets for print, or loaded later by the page
			if media := strings.ToLower(strings.TrimSpace(attrs["media"])); media != "" && media != "all" && media != "screen" {
				return ""
			}
			params = "; rel=preload; as=style"
		case has("preload"):
			as := strings.ToLower(attrs["as"])
			if !validToken(as) || attrs["imagesrcset"] != "" || attrs["media"] != "" {
				return ""
			}
			params = "; rel=preload; as=" + as
			if t := attrs["type"]; t != "" && validToken(strings.ReplaceAll(t, "/", "")) {
				params += "; type=" + t
			}
		case has("modulepreload"):
			params = "; rel=modulepreload"
		case has("preconnect"):
			params = "; rel=preconnect"
		default:
			return ""
		}
	}
	if !hintTarget(target) {
		return ""
	}
	if c, ok := attrs["crossorigin"]; ok {
		if strings.EqualFold(c, "use-credentials") {
			params += "; crossorigin=use-credentials"
		} else {
			params += "; crossorigin"
		}
	}
	return "<" + target + ">" + params
}

// hintTarget returns whether target is a URL which can be hinted: an
// absolute path, or an http(s) URL, with nothing which would break the
// header
func hintTarget(target string) bool {
	if !strings.HasPrefix(target, "/") && !strings.HasPrefix(target, "https://") && !strings.HasPrefix(target, "http://") {
		return false
	}
	return !strings.ContainsAny(target, "<>\"' \t\r\n\\") && !strings.ContainsFunc(target, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

func validToken(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '+' || r == '.') {
			return false
		}
	}
	return true
}

// hostHints remembers the hints of each host's recently cached pages, so
// that pages which aren't cached (e.g. a shop's checkout) can be sent the
// ones common to them all - usually the theme's.
type hostHints struct {
	mu    sync.Mutex
	hosts map[string][]pageHints
}

type pageHints struct {
	page  string
	hints []string
}

// seen records a cached page's hints
func (h *hostHints) seen(host, page string, hints []string) {
	if host == "" || hints == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hosts == nil {
		h.hosts = map[string][]pageHints{}
	}
	pages, known := h.hosts[host]
	if !known && len(h.hosts) >= maxHintHosts {
		return
	}
	for i, p := range pages {
		if p.page == page {
			pages = append(pages[:i:i], pages[i+1:]...)
			break
		}
	}
	pages = append(pages, pageHints{page, hints})
	if len(pages) > hintPages {
		pages = pages[len(pages)-hintPages:]
	}
	h.hosts[host] = pages
}

// common returns the hints which every one of host's recently cached pages
// has, in the order the latest has them
func (h *hostHints) common(host string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	pages := h.hosts[host]
	if len(pages) < minHintPages {
		return nil
	}
	var common []string
	for _, hint := range pages[len(pages)-1].hints {
		everywhere := true
		for _, p := range pages[:len(pages)-1] {
			everywhere = everywhere && contains(p.hints, hint)
		}
		if everywhere {
			common = append(common, hint)
		}
	}
	return common
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// CommonEarlyHints returns the Early Hints which all of host's recently
// cached pages have, for pages which aren't cached
func (c *defaultCacher) CommonEarlyHints(host string) []string {
	return c.hostHints.common(strings.ToLower(host))
}

// learnHints records a newly cached page's hints, for CommonEarlyHints
func (c *defaultCacher) learnHints(resp *responseImpl) {
	host, page := pageOrigin(resp.baseKey)
	c.hostHints.seen(strings.ToLower(host), page, EarlyHints(resp))
}
