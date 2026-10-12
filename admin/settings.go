package admin

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/davidjwilkins/honey/config"
)

// The kinds of settings fields
const (
	kindText     = "text"    // a string
	kindNumber   = "number"  // a number
	kindList     = "list"    // a list, empty if blank
	kindOptList  = "optlist" // a list, its default if blank, or empty if "none"
	kindOptBool  = "optbool" // its default, yes or no
	kindBool     = "bool"    // yes or no
	kindSelect   = "select"  // one of Options
	kindSecret   = "secret"  // never shown; kept unless changed
	kindRoutes   = "routes"  // the [[route]] tables, as TOML
	secretRemove = ".remove" // added to a secret's key, to remove it
)

// field is a setting on the settings page
type field struct {
	Key     string // its name in the config file, e.g. "cache.defaultTTL"
	Kind    string
	Help    string
	Default string // what applies if it isn't set, for the placeholder
	Options []string
	get     func(f *config.File) string
	set     func(f *config.File, value string) error
}

type section struct {
	Title  string
	Fields []*field
}

// sections are the settings page's fields, in the config file's order
var sections = []section{
	{"General", []*field{
		text("listen", "The address visitors' requests come to", ":8080",
			func(f *config.File) *string { return &f.Listen }),
		optList("trustedProxies", "Proxies in front of Honey whose X-Forwarded-For and X-Forwarded-Proto are believed", "loopback and private addresses",
			func(f *config.File) **[]string { return &f.TrustedProxies }),
		optList("hosts", "The only hosts served: others get a 421. *.example.com allows subdomains", "any host",
			func(f *config.File) **[]string { return &f.Hosts }),
	}},
	{"Backend", []*field{
		text("backend.uri", "The web server running WordPress", "",
			func(f *config.File) *string { return &f.Backend.URI }),
		text("backend.host", "The Host header sent to it", "the one the visitor sent",
			func(f *config.File) *string { return &f.Backend.Host }),
		text("backend.timeout", "How long to wait for it to start responding", "30s",
			func(f *config.File) *string { return &f.Backend.Timeout }),
		text("backend.stallTimeout", "How long it may go silent partway through a response", "30s",
			func(f *config.File) *string { return &f.Backend.StallTimeout }),
	}},
	{"Cache", []*field{
		text("cache.maxSize", "Memory for cached responses", "256MB",
			func(f *config.File) *string { return &f.Cache.MaxSize }),
		text("cache.maxObjectSize", "Larger responses are streamed, not cached", "10MB",
			func(f *config.File) *string { return &f.Cache.MaxObjectSize }),
		text("cache.defaultTTL", "How long pages are fresh, if WordPress doesn't say", "5m",
			func(f *config.File) *string { return &f.Cache.DefaultTTL }),
		text("cache.staleWhileRevalidate", "Then how long they're served while they're refreshed", "0",
			func(f *config.File) *string { return &f.Cache.StaleWhileReval }),
		text("cache.staleIfError", "And how long they're served if WordPress fails", "0",
			func(f *config.File) *string { return &f.Cache.StaleIfError }),
		text("cache.browserCacheControl", "The Cache-Control browsers get, e.g. no-cache", "the cached response's own",
			func(f *config.File) *string { return &f.Cache.BrowserCC }),
		optList("cache.queryParams", "The only query parameters which matter: others are removed", "all of them",
			func(f *config.File) **[]string { return &f.Cache.QueryParams }),
		optBool("cache.staticFiles", "Cache images, css, js, fonts and documents", "yes",
			func(f *config.File) **bool { return &f.Cache.StaticFiles }),
		optBool("cache.earlyHints", "Send browsers 103 Early Hints with the page's stylesheets and scripts while WordPress renders it (the proxy in front of Honey must pass them on)", "no",
			func(f *config.File) **bool { return &f.Cache.EarlyHints }),
		optBool("cache.brotli", "Brotli compress responses for browsers which accept it", "yes",
			func(f *config.File) **bool { return &f.Cache.Brotli }),
		optBool("cache.gzip", "Gzip compress them for browsers which don't", "yes",
			func(f *config.File) **bool { return &f.Cache.Gzip }),
		text("cache.compressMinSize", "Smaller responses aren't compressed", "1KB",
			func(f *config.File) *string { return &f.Cache.CompressMinSize }),
		text("cache.persist", "Keep the cache in this file across restarts", "not kept",
			func(f *config.File) *string { return &f.Cache.Persist }),
		text("cache.persistInterval", "How often to save it", "5m",
			func(f *config.File) *string { return &f.Cache.PersistInterval }),
	}},
	{"Cookies", []*field{
		optList("cache.bypassCookies", "Visitors with these cookies (name prefixes) aren't cached: logged in, commenters, shoppers", "WordPress and WooCommerce's",
			func(f *config.File) **[]string { return &f.Cache.BypassCookies }),
		list("cache.esiCookies", "Bypass cookies whose visitors get cached pages anyway, if the pages use ESI for the personal parts: commenters",
			func(f *config.File) *[]string { return &f.Cache.ESICookies }),
		list("cache.allowedCookies", "Cookies passed to WordPress, with a cached copy for each value",
			func(f *config.File) *[]string { return &f.Cache.AllowedCookies }),
		list("cache.stripCookies", "Cookies (name prefixes) removed from responses, so that they're cached",
			func(f *config.File) *[]string { return &f.Cache.StripCookies }),
	}},
	{"Routes", []*field{
		{Key: "route", Kind: kindRoutes, Help: "Rules for some requests, as [[route]] tables: the first matching route's settings apply",
			get: func(f *config.File) string {
				if len(f.Routes) == 0 {
					return ""
				}
				routes, _ := (&config.File{Routes: f.Routes}).TOML()
				return string(routes)
			},
			set: func(f *config.File, value string) error {
				routes, err := config.Decode(value)
				if err != nil {
					return err
				}
				f.Routes = routes.Routes
				routes.Routes = nil
				if rest, _ := routes.TOML(); len(rest) > 0 {
					return fmt.Errorf("only [[route]] tables go here")
				}
				return nil
			}},
	}},
	{"Warming", []*field{
		text("warm.sitemap", "Warm the cache from this sitemap, e.g. https://www.example.com/wp-sitemap.xml", "off",
			func(f *config.File) *string { return &f.Warm.Sitemap }),
		{Key: "warm.rate", Kind: kindNumber, Help: "Pages a second", Default: "2",
			get: func(f *config.File) string { return formatNumber(f.Warm.Rate) },
			set: func(f *config.File, v string) (err error) { f.Warm.Rate, err = parseNumber(v); return }},
		{Key: "warm.maxPages", Kind: kindNumber, Help: "The most pages to warm", Default: "10000",
			get: func(f *config.File) string { return formatNumber(float64(f.Warm.MaxPages)) },
			set: func(f *config.File, v string) error {
				n, err := parseNumber(v)
				if n != float64(int(n)) {
					return fmt.Errorf("a whole number")
				}
				f.Warm.MaxPages = int(n)
				return err
			}},
		text("warm.interval", "Warm it again this often", "only at startup and after purging everything",
			func(f *config.File) *string { return &f.Warm.Interval }),
	}},
	{"Purging", []*field{
		list("control.allowIPs", "Addresses (or networks) allowed to purge",
			func(f *config.File) *[]string { return &f.Control.AllowIPs }),
		{Key: "control.secret", Kind: kindSecret, Help: "A secret which allows purging from anywhere: it's never shown",
			get: func(f *config.File) string { return "" },
			set: func(f *config.File, v string) error { f.Control.Secret = v; return nil }},
		text("control.secretHeader", "The header the secret is sent in", "X-Honey-Secret",
			func(f *config.File) *string { return &f.Control.SecretHeader }),
	}},
	{"Admin and logs", []*field{
		text("admin.listen", "The address of this page", "off",
			func(f *config.File) *string { return &f.Admin.Listen }),
		text("metrics.listen", "An address for just the metrics", "off",
			func(f *config.File) *string { return &f.Metrics.Listen }),
		{Key: "log.access", Kind: kindBool, Help: "Log each request",
			get: func(f *config.File) string { return yesNo(f.Log.Access) },
			set: func(f *config.File, v string) error { f.Log.Access = v == "yes"; return nil }},
		{Key: "log.format", Kind: kindSelect, Help: "The access log's format", Options: []string{"", "text", "json"}, Default: "text",
			get: func(f *config.File) string { return f.Log.Format },
			set: func(f *config.File, v string) error { f.Log.Format = v; return nil }},
	}},
}

func text(key, help, def string, at func(*config.File) *string) *field {
	return &field{Key: key, Kind: kindText, Help: help, Default: def,
		get: func(f *config.File) string { return *at(f) },
		set: func(f *config.File, v string) error { *at(f) = v; return nil }}
}

func list(key, help string, at func(*config.File) *[]string) *field {
	return &field{Key: key, Kind: kindList, Help: help,
		get: func(f *config.File) string { return strings.Join(*at(f), "\n") },
		set: func(f *config.File, v string) error { *at(f) = splitList(v); return nil }}
}

func optList(key, help, def string, at func(*config.File) **[]string) *field {
	return &field{Key: key, Kind: kindOptList, Help: help, Default: def,
		get: func(f *config.File) string {
			switch l := *at(f); {
			case l == nil:
				return ""
			case len(*l) == 0:
				return "none"
			default:
				return strings.Join(*l, "\n")
			}
		},
		set: func(f *config.File, v string) error {
			items := splitList(v)
			switch {
			case len(items) == 0:
				*at(f) = nil
			case len(items) == 1 && items[0] == "none":
				*at(f) = &[]string{}
			default:
				*at(f) = &items
			}
			return nil
		}}
}

func optBool(key, help, def string, at func(*config.File) **bool) *field {
	return &field{Key: key, Kind: kindOptBool, Help: help, Default: def, Options: []string{"", "yes", "no"},
		get: func(f *config.File) string {
			if b := *at(f); b != nil {
				return yesNo(*b)
			}
			return ""
		},
		set: func(f *config.File, v string) error {
			switch v {
			case "":
				*at(f) = nil
			case "yes", "no":
				b := v == "yes"
				*at(f) = &b
			default:
				return fmt.Errorf("yes or no")
			}
			return nil
		}}
}

// splitList splits a list typed one to a line, or separated by commas
func splitList(v string) []string {
	var items []string
	for _, item := range strings.FieldsFunc(v, func(r rune) bool { return r == '\n' || r == ',' }) {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func formatNumber(n float64) string {
	if n == 0 {
		return ""
	}
	return strconv.FormatFloat(n, 'f', -1, 64)
}

func parseNumber(v string) (float64, error) {
	if v = strings.TrimSpace(v); v == "" {
		return 0, nil
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("a number")
	}
	return n, nil
}

// settingsView is what the settings page shows
type settingsView struct {
	chrome
	Sections []sectionView
	Preview  preview
	// HasSecret is whether there is a secret, which the form keeps
	HasSecret bool
}

type sectionView struct {
	Title  string
	Fields []fieldView
}

type fieldView struct {
	*field
	Value   string
	Problem string
	Changed bool
}

// preview is the config file the form describes, compared with the
// current one
type preview struct {
	Lines   []diffLine
	Changes int
	Valid   bool
	Problem string
}

// edit returns the settings the form describes, starting from the current
// ones, and what's wrong with any of its fields
func (a *admin) edit(form url.Values) (*config.File, map[string]string) {
	f := a.opts.Settings.Clone()
	problems := map[string]string{}
	for _, s := range sections {
		for _, fd := range s.Fields {
			values, sent := form[fd.Key]
			if fd.Kind == kindSecret {
				if form.Get(fd.Key+secretRemove) != "" {
					f.Control.Secret = ""
				} else if sent && strings.TrimSpace(values[0]) != "" {
					fd.set(f, strings.TrimSpace(values[0]))
				}
				continue
			}
			if !sent {
				continue
			}
			value := values[0]
			if fd.Kind != kindRoutes {
				value = strings.TrimSpace(value)
			}
			if err := fd.set(f, value); err != nil {
				problems[fd.Key] = err.Error()
			}
		}
	}
	return f, problems
}

// view returns the settings page for the settings f (or the current ones),
// with the form's values if there is one
func (a *admin) settingsView(f *config.File, form url.Values, problems map[string]string) settingsView {
	if f == nil {
		f = a.opts.Settings
	}
	v := settingsView{chrome: a.chrome("settings"), HasSecret: a.opts.Settings.Control.Secret != ""}
	for _, s := range sections {
		sv := sectionView{Title: s.Title}
		for _, fd := range s.Fields {
			value := fd.get(f)
			if sent, ok := form[fd.Key]; ok && fd.Kind != kindSecret {
				value = sent[0]
			}
			sv.Fields = append(sv.Fields, fieldView{
				field:   fd,
				Value:   value,
				Problem: problems[fd.Key],
				Changed: fd.Kind != kindSecret && fd.get(f) != fd.get(a.opts.Settings),
			})
		}
		v.Sections = append(v.Sections, sv)
	}
	v.Preview = a.preview(f, problems)
	return v
}

// preview compares the config file for f with the current one.  The
// secret is never shown.
func (a *admin) preview(f *config.File, problems map[string]string) preview {
	current, next := hideSecret(a.opts.Settings, a.opts.Settings), hideSecret(f, a.opts.Settings)
	before, _ := current.TOML()
	after, err := next.TOML()
	var p preview
	if err != nil {
		p.Problem = err.Error()
		return p
	}
	p.Lines = diffLines(string(before), string(after))
	for _, l := range p.Lines {
		if l.Op != " " {
			p.Changes++
		}
	}
	for key, problem := range problems {
		p.Problem = fmt.Sprintf("%s: %s", key, problem)
		return p
	}
	if _, err := config.FromFile(f); err != nil {
		p.Problem = err.Error()
		return p
	}
	p.Valid = true
	return p
}

// hideSecret returns f with its secret replaced, saying whether it is the
// same as current's
func hideSecret(f, current *config.File) *config.File {
	if f.Control.Secret == "" {
		return f
	}
	f = f.Clone()
	if f.Control.Secret == current.Control.Secret {
		f.Control.Secret = "(hidden)"
	} else {
		f.Control.Secret = "(changed, hidden)"
	}
	return f
}

// settings shows the settings, or, for a post of the form without
// JavaScript, the settings it describes and how they differ
func (a *admin) settings(w http.ResponseWriter, r *http.Request) {
	if a.opts.Settings == nil {
		http.Error(w, "The settings aren't available.", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		a.render(w, "settings", a.settingsView(nil, nil, nil))
	case http.MethodPost:
		r.ParseForm()
		f, problems := a.edit(r.PostForm)
		a.render(w, "settings", a.settingsView(f, r.PostForm, problems))
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// settingsPreview serves the preview of the settings the posted form
// describes, which the page's script shows as they're edited
func (a *admin) settingsPreview(w http.ResponseWriter, r *http.Request) {
	if a.opts.Settings == nil || r.Method != http.MethodPost {
		http.Error(w, "Post the settings form.", http.StatusMethodNotAllowed)
		return
	}
	r.ParseForm()
	f, problems := a.edit(r.PostForm)
	a.render(w, "preview", a.preview(f, problems))
}

// settingsExport downloads the settings the posted form describes, as a
// config file - with the secret, unlike the page.  Honey's own config
// file is never changed.
func (a *admin) settingsExport(w http.ResponseWriter, r *http.Request) {
	if a.opts.Settings == nil || r.Method != http.MethodPost {
		http.Error(w, "Post the settings form.", http.StatusMethodNotAllowed)
		return
	}
	r.ParseForm()
	f, problems := a.edit(r.PostForm)
	v := a.settingsView(f, r.PostForm, problems)
	if !v.Preview.Valid {
		w.WriteHeader(http.StatusBadRequest)
		a.render(w, "settings", v)
		return
	}
	data, err := f.TOML()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/toml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="honey.toml"`)
	fmt.Fprintf(w, "# Honey's settings, exported from its admin page on %s.\n", time.Now().Format("2 Jan 2006 15:04"))
	fmt.Fprintf(w, "# Replace honey.toml with this file and restart Honey to use them.\n\n")
	w.Write(data)
}

// diffLine is a line of a diff: Op is " " if it is in both, "-" if it was
// removed and "+" if it was added
type diffLine struct {
	Op, Text string
}

// diffLines compares two texts line by line
func diffLines(before, after string) []diffLine {
	a := strings.Split(strings.TrimSuffix(before, "\n"), "\n")
	b := strings.Split(strings.TrimSuffix(after, "\n"), "\n")
	if before == "" {
		a = nil
	}
	if after == "" {
		b = nil
	}
	// lcs[i][j] is the length of the longest common subsequence of a[i:]
	// and b[j:]
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var lines []diffLine
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			lines = append(lines, diffLine{" ", a[i]})
			i++
			j++
		case i < len(a) && (j == len(b) || lcs[i+1][j] >= lcs[i][j+1]):
			lines = append(lines, diffLine{"-", a[i]})
			i++
		default:
			lines = append(lines, diffLine{"+", b[j]})
			j++
		}
	}
	return lines
}
