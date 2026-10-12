package optimize

import (
	"html"
	"net/url"
	"regexp"
	"strings"
)

// Report is what the optimizer did to a cached page, and what it knows of
// it, for the admin page
type Report struct {
	// MainImages are what's known of its main image, on phones and on
	// desktops (if preloadMainImage is on)
	MainImages []MainImageReport
	// DelayedScripts are the scripts which run when the visitor first
	// interacts with it: their src, or "inline"
	DelayedScripts []string
	// SelfHostedFonts are the Google Fonts stylesheets it gets from the
	// site itself
	SelfHostedFonts []string
	// UsedCSS are its stylesheets whose used CSS it has
	UsedCSS []UsedCSSReport
	// Images are its images, and whether they're converted (if
	// convertImages is on)
	Images []ImageReport
}

// MainImageReport is what's known of a page's main image on Screens
// (phones or desktops)
type MainImageReport struct {
	Screens string
	// State is "learned", "not an image" (e.g. it's a heading), "asked"
	// (the page asks browsers) or "unknown"
	State string
	URL   string
}

// UsedCSSReport is a stylesheet whose used CSS a page has: Used bytes of
// it, of Whole (0 if it isn't known)
type UsedCSSReport struct {
	Href        string
	Used, Whole int
}

// ImageReport is an image on a page, and what it's been converted to
type ImageReport struct {
	URL string
	// Original is its size (0 if it isn't known)
	Original  int
	Converted []ConvertedReport
	// State is "converted", "converting", "not converted" (Why says why),
	// or "not seen" (no browser which takes AVIF or WebP has asked for it
	// yet)
	State, Why string
}

// ConvertedReport is a converted copy of an image
type ConvertedReport struct {
	Format string
	Bytes  int
}

// maxReportedImages bounds the images a report lists
const maxReportedImages = 40

var usedCSSStyle = regexp.MustCompile(`<style ` + UsedCSSAttribute + `="([^"]*)">((?s:.*?))</style>`)

// Report describes what o did to the cached page at path on host, whose
// HTML is body, or returns nil if o doesn't change pages
func (o *Optimizer) Report(host, path string, body []byte) *Report {
	if o == nil {
		return nil
	}
	report := &Report{}
	p := scan(body)
	// The host pages were cached under (see report)
	cachedHost := host
	if o.opts.BackendHost != "" {
		cachedHost = o.opts.BackendHost
	}

	if o.images != nil {
		known, urls := o.images.get(pageKey(cachedHost, path))
		asking := strings.Contains(string(body), `<script id="honey-main-image">`)
		for v, vp := range viewports {
			r := MainImageReport{Screens: map[string]string{"mobile": "phones", "desktop": "desktops"}[vp.name]}
			switch {
			case known[v] && urls[v] != "":
				r.State, r.URL = "learned", urls[v]
			case known[v]:
				r.State = "not an image"
			case asking:
				r.State = "asked"
			default:
				r.State = "unknown"
			}
			report.MainImages = append(report.MainImages, r)
		}
	}

	for _, s := range p.scripts {
		if s.attributeMap["type"] != DelayedType {
			continue
		}
		if src := s.attributeMap["data-honey-src"]; src != "" {
			report.DelayedScripts = append(report.DelayedScripts, src)
		} else if id := s.attributeMap["id"]; id != "" {
			report.DelayedScripts = append(report.DelayedScripts, "inline (#"+id+")")
		} else {
			report.DelayedScripts = append(report.DelayedScripts, "inline")
		}
	}

	for _, l := range p.links {
		if href := l.attributes["href"]; strings.HasPrefix(href, FontsPath) {
			report.SelfHostedFonts = append(report.SelfHostedFonts, href)
		}
	}

	for _, m := range usedCSSStyle.FindAllSubmatch(body, -1) {
		href := html.UnescapeString(string(m[1]))
		r := UsedCSSReport{Href: href, Used: len(m[2])}
		if o.css != nil {
			if u, err := url.Parse(href); err == nil {
				u = (&url.URL{Path: path}).ResolveReference(u)
				r.Whole = o.css.length(pageKey(cachedHost, u.RequestURI()))
			}
		}
		report.UsedCSS = append(report.UsedCSS, r)
	}

	if o.convert != nil {
		seen := map[string]bool{}
		base := &url.URL{Path: path}
		for _, img := range p.images {
			for _, u := range imageURLs(img, host, base) {
				if seen[u] || !strings.HasPrefix(u, "/") || imageKind(u) == "" || len(report.Images) >= maxReportedImages {
					continue
				}
				seen[u] = true
				report.Images = append(report.Images, o.convert.report(host, u))
			}
		}
	}
	return report
}

// length returns the size of the stylesheet kept as key, or 0
func (s *cssStore) length(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sheet := s.sheets[key]; sheet != nil {
		return sheet.length
	}
	return 0
}

// report describes the image at uri (a path, and perhaps a query) on host
func (c *imageConverter) report(host, uri string) ImageReport {
	r := ImageReport{URL: uri}
	if strings.Contains(uri, "?") {
		r.State, r.Why = "not converted", "it has a query string"
		return r
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.images[strings.ToLower(host)+uri]
	if e == nil {
		r.State = "not seen"
		return r
	}
	r.Original = e.original
	for _, f := range imageFormats {
		if img := e.formats[f.name]; img != nil {
			r.Converted = append(r.Converted, ConvertedReport{Format: f.name, Bytes: len(img.body)})
		}
	}
	var why []string
	for _, f := range imageFormats {
		if reason := e.none[f.name]; reason != "" && !contains(why, reason) {
			why = append(why, reason)
		}
	}
	switch {
	case len(r.Converted) > 0:
		r.State = "converted"
	case len(e.pending) > 0 || e.checking:
		r.State = "converting"
	case len(why) > 0:
		r.State, r.Why = "not converted", strings.Join(why, "; ")
	default:
		r.State = "not seen"
	}
	return r
}
