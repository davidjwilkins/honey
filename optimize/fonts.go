package optimize

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/davidjwilkins/honey/utilities"
)

// Self-hosting Google Fonts: a page using Google Fonts has the browser
// connect to two more sites (fonts.googleapis.com for the stylesheet, then
// fonts.gstatic.com for the fonts) before it can show its text in them,
// and tells Google about every visitor.  So pages' links to Google Fonts'
// stylesheets (and @imports of them) are changed to FontsPath on the site
// itself, and their preconnects to Google are left out.  Honey fetches the
// stylesheet from Google the first time it's asked for it - as a modern
// browser, so that it gets WOFF2 fonts - changes its fonts' URLs to
// FontsPath too, and adds font-display: swap (so that text is shown in
// another font while the font loads, unless the page asked otherwise).
// The fonts are fetched the first time they're asked for.  Both are kept,
// in memory, and the fonts sent to browsers to keep for a year (their URLs
// change with them).  Stylesheets are kept compressed too (with brotli and
// gzip, as Google sends them), as they hold the page up.

// FontsPath is where self-hosted fonts and their stylesheets are served
const FontsPath = "/.well-known/honey/fonts/"

const (
	// maxFontBytes bounds the fonts and stylesheets kept
	maxFontBytes = 64 << 20
	// maxFontFile and maxFontCSS bound a font's and a stylesheet's size
	maxFontFile = 4 << 20
	maxFontCSS  = 512 << 10
	// cssLifetime is how long a stylesheet is kept before it's fetched
	// again (in case Google has updated its fonts)
	cssLifetime = 24 * time.Hour
	// unexpectedFetches is how many stylesheets and fonts Honey hasn't seen
	// a page use (e.g. since it restarted) it fetches a minute, so that it
	// can't be made to fetch any number from Google
	unexpectedFetches = 60
	// fontsUserAgent is the browser Google is told is asking: one which
	// takes WOFF2 fonts
	fontsUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"
)

var (
	// googleFontsURL matches a Google Fonts stylesheet's URL, and its path
	// (css or css2) and query string
	googleFontsURL = regexp.MustCompile(`(?i)^(?:https?:)?//fonts\.googleapis\.com/(css2?)\?(.+)$`)
	// googleFontsImport matches Google Fonts stylesheets' URLs in CSS
	googleFontsImport = regexp.MustCompile(`(?i)(?:https?:)?//fonts\.googleapis\.com/(css2?)\?`)
	// googleFontFile matches a font's URL in Google's stylesheets
	googleFontFile = regexp.MustCompile(`https://fonts\.gstatic\.com/(s/[A-Za-z0-9_.\-/]+)`)
	// fontFilePath is a font's path, after FontsPath
	fontFilePath = regexp.MustCompile(`^s/[a-z0-9_-]+/v[0-9]+/[A-Za-z0-9_.-]+\.(woff2|woff|ttf|otf)$`)
	// fontFace finds @font-face rules' openings
	fontFace = regexp.MustCompile(`@font-face\s*\{`)
	// localImport finds self-hosted stylesheets' URLs in CSS
	localImport = regexp.MustCompile(regexp.QuoteMeta(FontsPath) + `css2?\?[^'")\s;]+`)
)

// selfHostFonts returns the edits which change p's Google Fonts links,
// @imports and preconnects
func (o *Optimizer) selfHostFonts(p *page) []edit {
	var edits []edit
	for _, l := range p.links {
		rels := " " + strings.ToLower(l.attributes["rel"]) + " "
		local := fontsURL(l.attributes["href"])
		if local == "" || !(strings.Contains(rels, " stylesheet ") || strings.Contains(rels, " preload ")) {
			continue
		}
		o.fonts.expect(local)
		rest := withoutAttributes(p.body[l.attrs[0]:l.attrs[1]], "href")
		text := []byte(`<link href="` + html.EscapeString(local) + `"`)
		if len(rest) > 0 && rest[0] != ' ' {
			text = append(text, ' ')
		}
		text = append(text, rest...)
		text = append(text, '>')
		edits = append(edits, edit{start: l.start, end: l.end, text: text})
	}
	for _, s := range p.styles {
		content := p.body[s.content[0]:s.content[1]]
		if !googleFontsImport.Match(content) {
			continue
		}
		changed := googleFontsImport.ReplaceAllFunc(content, func(m []byte) []byte {
			path := "css2"
			if !bytes.HasSuffix(bytes.ToLower(m), []byte("css2?")) {
				path = "css"
			}
			return []byte(FontsPath + path + "?")
		})
		var b bytes.Buffer
		b.Write(p.body[s.start:s.content[0]])
		b.Write(changed)
		b.Write(p.body[s.content[1]:s.end])
		edits = append(edits, edit{start: s.start, end: s.end, text: b.Bytes()})
		for _, m := range localImport.FindAll(changed, -1) {
			o.fonts.expect(html.UnescapeString(string(m)))
		}
	}
	if len(edits) == 0 {
		return nil
	}
	// The browser no longer connects to Google for the fonts
	for _, l := range p.links {
		rels := " " + strings.ToLower(l.attributes["rel"]) + " "
		host := strings.ToLower(l.attributes["href"])
		if (strings.Contains(rels, " preconnect ") || strings.Contains(rels, " dns-prefetch ")) &&
			(strings.Contains(host, "fonts.googleapis.com") || strings.Contains(host, "fonts.gstatic.com")) {
			edits = append(edits, edit{start: l.start, end: l.end})
		}
	}
	return edits
}

// fontStore keeps self-hosted fonts and stylesheets, fetching them from
// Google when they're first asked for
type fontStore struct {
	opts   *Options
	client *http.Client

	mu       sync.Mutex
	files    map[string]*fontFile
	bytes    int
	expected map[string]bool
	inFlight map[string]*fontFetch
	// fetches counts unexpected fetches this minute, since minute
	fetches int
	minute  time.Time
}

type fontFile struct {
	body        []byte
	contentType string
	fetched     time.Time
	// br and gzip are a stylesheet's body, compressed
	br, gzip []byte
}

// size is how much memory f takes
func (f *fontFile) size() int {
	return len(f.body) + len(f.br) + len(f.gzip)
}

// encoded returns f's body as a browser accepting acceptEncoding is sent
// it, and its Content-Encoding ("" if it isn't compressed)
func (f *fontFile) encoded(acceptEncoding string) ([]byte, string) {
	var available []string
	if f.br != nil {
		available = append(available, "br")
	}
	if f.gzip != nil {
		available = append(available, "gzip")
	}
	switch utilities.PreferredEncoding(acceptEncoding, available...) {
	case "br":
		return f.br, "br"
	case "gzip":
		return f.gzip, "gzip"
	}
	return f.body, ""
}

type fontFetch struct {
	done chan struct{}
	file *fontFile
	err  error
}

func newFontStore(opts *Options) *fontStore {
	if opts.googleCSS == "" {
		opts.googleCSS = "https://fonts.googleapis.com/"
	}
	if opts.googleFonts == "" {
		opts.googleFonts = "https://fonts.gstatic.com/"
	}
	client := opts.client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &fontStore{opts: opts, client: client, files: map[string]*fontFile{},
		expected: map[string]bool{}, inFlight: map[string]*fontFetch{}}
}

// expect records that a page uses the stylesheet or font at path (and
// query), so that it's fetched whenever it's asked for
func (s *fontStore) expect(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.expected) < 10000 {
		s.expected[path] = true
	}
}

// serve responds with a self-hosted stylesheet or font
func (s *fontStore) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, FontsPath)
	var key, upstream string
	css := false
	switch {
	case (rest == "css" || rest == "css2") && r.URL.RawQuery != "" && len(r.URL.RawQuery) <= 2048:
		css = true
		key = FontsPath + rest + "?" + r.URL.RawQuery
		upstream = s.opts.googleCSS + rest + "?" + r.URL.RawQuery
	case fontFilePath.MatchString(rest):
		key = r.URL.Path
		upstream = s.opts.googleFonts + rest
	default:
		http.NotFound(w, r)
		return
	}
	file, err := s.get(r.Context(), key, upstream, css)
	if err != nil {
		http.Error(w, "Bad Gateway: "+err.Error(), http.StatusBadGateway)
		return
	}
	h := w.Header()
	h.Set("Content-Type", file.contentType)
	h.Set("X-Content-Type-Options", "nosniff")
	body := file.body
	if css {
		h.Set("Cache-Control", "public, max-age=86400")
		h.Add("Vary", "Accept-Encoding")
		var encoding string
		if body, encoding = file.encoded(r.Header.Get("Accept-Encoding")); encoding != "" {
			h.Set("Content-Encoding", encoding)
		}
	} else {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
		h.Set("Access-Control-Allow-Origin", "*")
	}
	http.ServeContent(w, r, "", file.fetched, bytes.NewReader(body))
}

// get returns the file key, fetching it from upstream if it isn't kept (or,
// for a stylesheet, is out of date)
func (s *fontStore) get(ctx context.Context, key, upstream string, css bool) (*fontFile, error) {
	now := s.opts.now()
	s.mu.Lock()
	kept := s.files[key]
	if kept != nil && (!css || now.Sub(kept.fetched) < cssLifetime) {
		s.mu.Unlock()
		return kept, nil
	}
	f := s.inFlight[key]
	if f == nil {
		if kept == nil && !s.expected[key] {
			if now.Sub(s.minute) >= time.Minute {
				s.minute, s.fetches = now, 0
			}
			if s.fetches >= unexpectedFetches {
				s.mu.Unlock()
				return nil, fmt.Errorf("too many fonts asked for")
			}
			s.fetches++
		}
		f = &fontFetch{done: make(chan struct{})}
		s.inFlight[key] = f
		go s.fetch(key, upstream, css, f)
	}
	s.mu.Unlock()
	select {
	case <-f.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if f.err != nil {
		if kept != nil {
			// An old stylesheet is better than none
			return kept, nil
		}
		return nil, f.err
	}
	return f.file, nil
}

// fetch fetches the file key from upstream, and keeps it
func (s *fontStore) fetch(key, upstream string, css bool, f *fontFetch) {
	defer close(f.done)
	defer func() {
		s.mu.Lock()
		delete(s.inFlight, key)
		s.mu.Unlock()
	}()
	req, err := http.NewRequest(http.MethodGet, upstream, nil)
	if err != nil {
		f.err = err
		return
	}
	req.Header.Set("User-Agent", fontsUserAgent)
	resp, err := s.client.Do(req)
	if err != nil {
		f.err = fmt.Errorf("fetching it from Google: %w", err)
		return
	}
	defer resp.Body.Close()
	limit := int64(maxFontFile)
	if css {
		limit = maxFontCSS
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	switch {
	case err != nil:
		f.err = fmt.Errorf("fetching it from Google: %w", err)
		return
	case resp.StatusCode != http.StatusOK:
		f.err = fmt.Errorf("Google said %s", resp.Status)
		return
	case int64(len(body)) > limit:
		f.err = fmt.Errorf("it's too big")
		return
	}
	file := &fontFile{body: body, contentType: resp.Header.Get("Content-Type"), fetched: s.opts.now()}
	if css {
		file.body, file.contentType = s.localCSS(body), "text/css; charset=utf-8"
		file.br, file.gzip = compressCSS(file.body)
	} else if !strings.HasPrefix(file.contentType, "font/") {
		file.contentType = "font/" + key[strings.LastIndexByte(key, '.')+1:]
	}
	f.file = file
	s.keep(key, file)
}

// localCSS returns Google's stylesheet with its fonts served from FontsPath,
// and font-display: swap unless it says otherwise
func (s *fontStore) localCSS(css []byte) []byte {
	css = googleFontFile.ReplaceAllFunc(css, func(m []byte) []byte {
		local := FontsPath + string(m[len("https://fonts.gstatic.com/"):])
		s.expect(local)
		return []byte(local)
	})
	// A font-display of Google's (from the URL's display=) comes later,
	// and wins
	return fontFace.ReplaceAll(css, []byte("@font-face {\n  font-display: swap;"))
}

// keep keeps file, as key, forgetting others if they'd be too big
func (s *fontStore) keep(key string, file *fontFile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.files[key]; old != nil {
		s.bytes -= old.size()
	}
	for k, f := range s.files {
		if s.bytes+file.size() <= maxFontBytes {
			break
		}
		delete(s.files, k)
		s.bytes -= f.size()
	}
	s.files[key] = file
	s.bytes += file.size()
}

// compressCSS returns css compressed with brotli and gzip
func compressCSS(css []byte) (br, gz []byte) {
	var b bytes.Buffer
	w := brotli.NewWriterLevel(&b, brotli.BestCompression)
	w.Write(css)
	w.Close()
	br = bytes.Clone(b.Bytes())
	b.Reset()
	g, _ := gzip.NewWriterLevel(&b, gzip.BestCompression)
	g.Write(css)
	g.Close()
	return br, bytes.Clone(b.Bytes())
}

// fontsURL returns a Google Fonts stylesheet URL's self-hosted path, or ""
func fontsURL(raw string) string {
	m := googleFontsURL.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return ""
	}
	// (Not parsed: Google's css2 queries have semicolons, e.g.
	// family=Inter:wght@400;700)
	if strings.ContainsAny(m[2], "<>\"' \t\r\n") {
		return ""
	}
	return FontsPath + strings.ToLower(m[1]) + "?" + m[2]
}
