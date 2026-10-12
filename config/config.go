// Package config loads Honey's TOML configuration file.
package config

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/davidjwilkins/honey/cache"
	"github.com/davidjwilkins/honey/clientip"
	"github.com/davidjwilkins/honey/fetch"
)

// DefaultListen is the address Honey listens on if none is configured.
const DefaultListen = ":8080"

// DefaultPersistInterval is how often the cache is saved, if
// cache.persist is set and cache.persistInterval isn't.
const DefaultPersistInterval = 5 * time.Minute

// DefaultBackendTimeout is how long Honey waits for the backend to start
// responding, or for it to send more of a response body, if no
// backend.timeout or backend.stallTimeout is configured.
const DefaultBackendTimeout = 30 * time.Second

// Config is a validated Honey configuration.
type Config struct {
	// Listen is the address to listen on, e.g. ":8080"
	Listen string
	// TrustedProxies are the proxies whose X-Forwarded-For and
	// X-Forwarded-Proto headers are believed (see clientip.Middleware).
	// Defaults to clientip.DefaultTrusted.
	TrustedProxies []*net.IPNet
	// Hosts, if not nil, are the only hosts requests are handled for (see
	// fetch.Options.Hosts)
	Hosts []string
	// MetricsListen is the address to serve metrics on, e.g.
	// "127.0.0.1:9090", or "" not to serve them
	MetricsListen string
	// AdminListen is the address to serve the admin page (and metrics) on,
	// e.g. "127.0.0.1:9090", or "" not to serve it
	AdminListen string
	// AccessLog is whether to log each request to stdout, in
	// AccessLogFormat ("text" or "json")
	AccessLog       bool
	AccessLogFormat string
	// Backend is the server which requests are proxied to
	Backend *url.URL
	// BackendHost, if set, is the Host header sent to the backend, instead
	// of the client's (see fetch.Options.BackendHost)
	BackendHost string
	// BackendTimeout is how long to wait for the backend to start
	// responding.  Zero means no limit.
	BackendTimeout time.Duration
	// BackendStallTimeout is how long the backend may send nothing while a
	// response body is being read.  Zero means no limit.
	BackendStallTimeout time.Duration
	Cache               Cache
	Routes              []Route
	Control             Control
	Warm                Warm
	// File is the settings as they were written, e.g. to export them
	File *File
}

// Warm configures warming the cache from a sitemap (see the warm
// package).  It is off if Sitemap is nil.
type Warm struct {
	Sitemap  *url.URL
	Rate     float64
	MaxPages int
	Interval time.Duration
}

// Control says which requests are trusted to purge the cache and to make
// it fetch fresh responses (see fetch.Control).
type Control struct {
	AllowIPs     []*net.IPNet
	Secret       string
	SecretHeader string
}

// MinSecretLength is the shortest control.secret allowed, so that it can't
// be guessed.
const MinSecretLength = 16

// Cache configures the cache.  Zero values mean the cache package's
// defaults.
type Cache struct {
	MaxBytes       int64
	MaxObjectBytes int64
	DefaultTTL     time.Duration
	// StaleIfError is how long responses may be served stale if the
	// backend errors, unless they say otherwise
	StaleIfError time.Duration
	// StaleWhileRevalidate is how long responses may be served stale while
	// they are refreshed in the background, unless they say otherwise
	StaleWhileRevalidate time.Duration
	// Persist is the file the cache is saved to (every PersistInterval, if
	// it isn't zero, and on shutdown) and loaded from at startup, or ""
	Persist         string
	PersistInterval time.Duration
	AllowedCookies  []string
	// BypassCookies are cookie name prefixes which mark visitors whose
	// pages may be personal, and so aren't cached (see
	// cache.Options.BypassCookies).  Defaults to
	// cache.WordPressBypassCookies.
	BypassCookies []string
	// ESICookies are bypass cookies (name prefixes) whose visitors may be
	// served cached pages which use Edge Side Includes for the parts
	// which differ between them (see cache.Options.ESICookies)
	ESICookies []string
	// StripCookies are cookie name prefixes which responses may set and
	// still be cached, without the cookie (see cache.Options.StripCookies)
	StripCookies []string
	// BrowserCacheControl, if set, is the Cache-Control header sent to
	// clients with successful cached responses (see
	// cache.Options.BrowserCacheControl)
	BrowserCacheControl string
	// QueryParams, if not nil, are the only query parameters kept on
	// cacheable requests (see fetch.Options)
	QueryParams []string
	// StaticFiles is whether static files (images, css, js...) are cached
	StaticFiles bool
	// EarlyHints is whether browsers are sent 103 Early Hints while their
	// requests wait for the backend (see fetch.Options)
	EarlyHints bool
	// Brotli and Gzip are whether compressible responses are compressed
	// with each, for clients which accept them
	Brotli           bool
	Gzip             bool
	CompressMinBytes int64
}

// Route is a rule for requests whose path starts with Match, or, if
// Regex is set, whose path and query string match it.  It either stops
// them being cached, or overrides cache settings for them.
type Route struct {
	Match string
	// Regex is the compiled Match, if the route is a regular expression
	Regex *regexp.Regexp
	Cache bool
	// DefaultTTL overrides cache.defaultTTL, if it isn't zero
	DefaultTTL time.Duration
	// StaleIfError overrides cache.staleIfError, if it isn't nil
	StaleIfError *time.Duration
	// StaleWhileRevalidate overrides cache.staleWhileRevalidate, if it
	// isn't nil
	StaleWhileRevalidate *time.Duration
	// QueryParams overrides cache.queryParams, if it isn't nil
	QueryParams *[]string
	// BypassCookies overrides cache.bypassCookies, if it isn't nil
	BypassCookies *[]string
	// BrowserCacheControl overrides cache.browserCacheControl, if it isn't
	// nil.  It is "" for "inherit": send the cached response's own.
	BrowserCacheControl *string
}

// File is the layout of the TOML file: the settings as they are written,
// before they are validated, and without defaults filled in.
type File struct {
	Listen         string    `toml:"listen,omitempty"`
	TrustedProxies *[]string `toml:"trustedProxies,omitempty"`
	Hosts          *[]string `toml:"hosts,omitempty"`
	Metrics        struct {
		Listen string `toml:"listen,omitempty"`
	} `toml:"metrics,omitempty"`
	Admin struct {
		Listen string `toml:"listen,omitempty"`
	} `toml:"admin,omitempty"`
	Log struct {
		Access bool   `toml:"access,omitempty"`
		Format string `toml:"format,omitempty"`
	} `toml:"log,omitempty"`
	Backend struct {
		URI          string `toml:"uri,omitempty"`
		Host         string `toml:"host,omitempty"`
		Timeout      string `toml:"timeout,omitempty"`
		StallTimeout string `toml:"stallTimeout,omitempty"`
	} `toml:"backend,omitempty"`
	Cache struct {
		MaxSize         string    `toml:"maxSize,omitempty"`
		MaxObjectSize   string    `toml:"maxObjectSize,omitempty"`
		DefaultTTL      string    `toml:"defaultTTL,omitempty"`
		StaleIfError    string    `toml:"staleIfError,omitempty"`
		StaleWhileReval string    `toml:"staleWhileRevalidate,omitempty"`
		Persist         string    `toml:"persist,omitempty"`
		PersistInterval string    `toml:"persistInterval,omitempty"`
		AllowedCookies  []string  `toml:"allowedCookies,omitempty"`
		BypassCookies   *[]string `toml:"bypassCookies,omitempty"`
		ESICookies      []string  `toml:"esiCookies,omitempty"`
		StripCookies    []string  `toml:"stripCookies,omitempty"`
		BrowserCC       string    `toml:"browserCacheControl,omitempty"`
		QueryParams     *[]string `toml:"queryParams,omitempty"`
		StaticFiles     *bool     `toml:"staticFiles,omitempty"`
		EarlyHints      *bool     `toml:"earlyHints,omitempty"`
		Brotli          *bool     `toml:"brotli,omitempty"`
		Gzip            *bool     `toml:"gzip,omitempty"`
		CompressMinSize string    `toml:"compressMinSize,omitempty"`
	} `toml:"cache,omitempty"`
	Warm struct {
		Sitemap  string  `toml:"sitemap,omitempty"`
		Rate     float64 `toml:"rate,omitempty,omitzero"`
		MaxPages int     `toml:"maxPages,omitempty,omitzero"`
		Interval string  `toml:"interval,omitempty"`
	} `toml:"warm,omitempty"`
	Control struct {
		AllowIPs     []string `toml:"allowIPs,omitempty"`
		Secret       string   `toml:"secret,omitempty"`
		SecretHeader string   `toml:"secretHeader,omitempty"`
	} `toml:"control,omitempty"`
	Routes []RouteFile `toml:"route,omitempty"`
}

// RouteFile is the layout of a [[route]] in the TOML file
type RouteFile struct {
	Match         string    `toml:"match"`
	Regex         bool      `toml:"regex,omitempty"`
	Cache         *bool     `toml:"cache,omitempty"`
	DefaultTTL    string    `toml:"defaultTTL,omitempty"`
	StaleIfError  string    `toml:"staleIfError,omitempty"`
	StaleWhileRev string    `toml:"staleWhileRevalidate,omitempty"`
	QueryParams   *[]string `toml:"queryParams,omitempty"`
	BypassCookies *[]string `toml:"bypassCookies,omitempty"`
	BrowserCC     *string   `toml:"browserCacheControl,omitempty"`
}

// Decode decodes the settings in a config file's contents, without
// validating them (see Parse).  It returns an error if the file isn't
// TOML, or has settings Honey doesn't support.
func Decode(data string) (*File, error) {
	var f File
	meta, err := toml.Decode(data, &f)
	if err != nil {
		return nil, err
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, key := range undecoded {
			keys[i] = key.String()
		}
		sort.Strings(keys)
		return nil, fmt.Errorf("unsupported settings: %s", strings.Join(keys, ", "))
	}
	return &f, nil
}

// TOML encodes the settings as a config file.  Settings which aren't set
// are left out, so that their defaults apply.
func (f *File) TOML() ([]byte, error) {
	var b bytes.Buffer
	enc := toml.NewEncoder(&b)
	enc.Indent = ""
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Clone returns a deep copy of f, which can be changed without changing f.
func (f *File) Clone() *File {
	// Every field is a value, a slice of strings or a pointer to one, or
	// a pointer to a bool or string, so this is enough
	c := *f
	c.TrustedProxies = cloneList(f.TrustedProxies)
	c.Hosts = cloneList(f.Hosts)
	c.Cache.AllowedCookies = append([]string(nil), f.Cache.AllowedCookies...)
	c.Cache.BypassCookies = cloneList(f.Cache.BypassCookies)
	c.Cache.ESICookies = append([]string(nil), f.Cache.ESICookies...)
	c.Cache.StripCookies = append([]string(nil), f.Cache.StripCookies...)
	c.Cache.QueryParams = cloneList(f.Cache.QueryParams)
	c.Cache.StaticFiles = cloneBool(f.Cache.StaticFiles)
	c.Cache.EarlyHints = cloneBool(f.Cache.EarlyHints)
	c.Cache.Brotli = cloneBool(f.Cache.Brotli)
	c.Cache.Gzip = cloneBool(f.Cache.Gzip)
	c.Control.AllowIPs = append([]string(nil), f.Control.AllowIPs...)
	c.Routes = nil
	for _, r := range f.Routes {
		r.Cache = cloneBool(r.Cache)
		r.QueryParams = cloneList(r.QueryParams)
		r.BypassCookies = cloneList(r.BypassCookies)
		if r.BrowserCC != nil {
			cc := *r.BrowserCC
			r.BrowserCC = &cc
		}
		c.Routes = append(c.Routes, r)
	}
	return &c
}

func cloneList(l *[]string) *[]string {
	if l == nil {
		return nil
	}
	c := append([]string{}, *l...)
	return &c
}

func cloneBool(b *bool) *bool {
	if b == nil {
		return nil
	}
	c := *b
	return &c
}

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse parses and validates a config file's contents.
func Parse(data string) (*Config, error) {
	f, err := Decode(data)
	if err != nil {
		return nil, err
	}
	return FromFile(f)
}

// FromFile validates the settings in f, and fills in their defaults.
func FromFile(f *File) (*Config, error) {
	var err error
	cfg := &Config{File: f.Clone(), Listen: f.Listen, MetricsListen: f.Metrics.Listen, AdminListen: f.Admin.Listen, AccessLog: f.Log.Access, AccessLogFormat: f.Log.Format}
	if cfg.AdminListen != "" && cfg.AdminListen == cfg.MetricsListen {
		return nil, fmt.Errorf("admin.listen and metrics.listen are the same: the admin page serves /metrics too, so only set admin.listen")
	}
	switch cfg.AccessLogFormat {
	case "":
		cfg.AccessLogFormat = "text"
	case "text", "json":
	default:
		return nil, fmt.Errorf("log.format must be \"text\" or \"json\"")
	}
	if cfg.Listen == "" {
		cfg.Listen = DefaultListen
	}
	cfg.TrustedProxies = clientip.DefaultTrusted
	if f.TrustedProxies != nil {
		cfg.TrustedProxies, err = fetch.ParseIPs(*f.TrustedProxies)
		if err != nil {
			return nil, fmt.Errorf("trustedProxies: %w", err)
		}
	}
	if f.Hosts != nil {
		cfg.Hosts, err = parseHosts(*f.Hosts)
		if err != nil {
			return nil, err
		}
	}

	if f.Backend.URI == "" {
		return nil, fmt.Errorf("backend.uri is required")
	}
	cfg.Backend, err = url.Parse(f.Backend.URI)
	if err != nil {
		return nil, fmt.Errorf("backend.uri: %w", err)
	}
	if (cfg.Backend.Scheme != "http" && cfg.Backend.Scheme != "https") || cfg.Backend.Host == "" {
		return nil, fmt.Errorf("backend.uri must be an http or https URL, e.g. https://www.example.com")
	}

	if f.Backend.Host != "" {
		if strings.ContainsAny(f.Backend.Host, "/ \t\r\n") {
			return nil, fmt.Errorf("backend.host must be a host name (and port), e.g. \"www.example.com\", not %q", f.Backend.Host)
		}
		cfg.BackendHost = f.Backend.Host
	}
	cfg.BackendTimeout = DefaultBackendTimeout
	if f.Backend.Timeout != "" {
		cfg.BackendTimeout, err = parseDuration(f.Backend.Timeout)
		if err != nil || cfg.BackendTimeout < 0 || (cfg.BackendTimeout > 0 && cfg.BackendTimeout < time.Second) {
			return nil, fmt.Errorf("backend.timeout must be a duration of at least 1s (or \"0\" for no limit), e.g. \"30s\"")
		}
	}
	cfg.BackendStallTimeout = DefaultBackendTimeout
	if f.Backend.StallTimeout != "" {
		cfg.BackendStallTimeout, err = parseDuration(f.Backend.StallTimeout)
		if err != nil || cfg.BackendStallTimeout < 0 || (cfg.BackendStallTimeout > 0 && cfg.BackendStallTimeout < time.Second) {
			return nil, fmt.Errorf("backend.stallTimeout must be a duration of at least 1s (or \"0\" for no limit), e.g. \"30s\"")
		}
	}
	cfg.Cache.Persist = f.Cache.Persist
	if f.Cache.PersistInterval != "" {
		if cfg.Cache.Persist == "" {
			return nil, fmt.Errorf("cache.persistInterval is set, but cache.persist isn't")
		}
		cfg.Cache.PersistInterval, err = parseDuration(f.Cache.PersistInterval)
		if err != nil || cfg.Cache.PersistInterval < 0 || (cfg.Cache.PersistInterval > 0 && cfg.Cache.PersistInterval < time.Second) {
			return nil, fmt.Errorf("cache.persistInterval must be a duration of at least 1s (or \"0\" to only save on shutdown), e.g. \"5m\"")
		}
	} else if cfg.Cache.Persist != "" {
		cfg.Cache.PersistInterval = DefaultPersistInterval
	}
	if f.Cache.StaleIfError != "" {
		cfg.Cache.StaleIfError, err = parseDuration(f.Cache.StaleIfError)
		if err != nil || cfg.Cache.StaleIfError < time.Second {
			return nil, fmt.Errorf("cache.staleIfError must be a duration of at least 1s, e.g. \"1h\"")
		}
	}
	if f.Cache.StaleWhileReval != "" {
		cfg.Cache.StaleWhileRevalidate, err = parseDuration(f.Cache.StaleWhileReval)
		if err != nil || cfg.Cache.StaleWhileRevalidate < time.Second {
			return nil, fmt.Errorf("cache.staleWhileRevalidate must be a duration of at least 1s, e.g. \"1d\"")
		}
	}
	if f.Cache.MaxSize != "" {
		cfg.Cache.MaxBytes, err = parseSize(f.Cache.MaxSize)
		if err != nil {
			return nil, fmt.Errorf("cache.maxSize: %w", err)
		}
	}
	if f.Cache.MaxObjectSize != "" {
		cfg.Cache.MaxObjectBytes, err = parseSize(f.Cache.MaxObjectSize)
		if err != nil {
			return nil, fmt.Errorf("cache.maxObjectSize: %w", err)
		}
	}
	if cfg.Cache.MaxBytes != 0 && cfg.Cache.MaxObjectBytes > cfg.Cache.MaxBytes {
		return nil, fmt.Errorf("cache.maxObjectSize can't be larger than cache.maxSize")
	}
	cfg.Cache.StaticFiles = f.Cache.StaticFiles == nil || *f.Cache.StaticFiles
	// Off unless asked for, as the proxy in front of Honey (if any) must
	// pass them on
	cfg.Cache.EarlyHints = f.Cache.EarlyHints != nil && *f.Cache.EarlyHints
	cfg.Cache.Brotli = f.Cache.Brotli == nil || *f.Cache.Brotli
	cfg.Cache.Gzip = f.Cache.Gzip == nil || *f.Cache.Gzip
	if f.Cache.CompressMinSize != "" {
		cfg.Cache.CompressMinBytes, err = parseSize(f.Cache.CompressMinSize)
		if err != nil {
			return nil, fmt.Errorf("cache.compressMinSize: %w", err)
		}
	}
	if f.Cache.DefaultTTL != "" {
		cfg.Cache.DefaultTTL, err = parseDuration(f.Cache.DefaultTTL)
		if err != nil || cfg.Cache.DefaultTTL < time.Second {
			return nil, fmt.Errorf("cache.defaultTTL must be a duration of at least 1s, e.g. \"5m\"")
		}
	}
	cfg.Cache.AllowedCookies = append([]string(nil), f.Cache.AllowedCookies...)
	cfg.Cache.BypassCookies = append([]string{}, cache.WordPressBypassCookies...)
	if f.Cache.BypassCookies != nil {
		cfg.Cache.BypassCookies, err = cookiePrefixes("cache.bypassCookies", *f.Cache.BypassCookies)
		if err != nil {
			return nil, err
		}
	}
	// "inherit" (the default) sends clients cached responses' own
	if f.Cache.BrowserCC != "" && f.Cache.BrowserCC != "inherit" {
		if err := checkCacheControl("cache.browserCacheControl", f.Cache.BrowserCC); err != nil {
			return nil, err
		}
		cfg.Cache.BrowserCacheControl = f.Cache.BrowserCC
	}
	for _, name := range cfg.Cache.AllowedCookies {
		if hasAnyPrefix(name, cfg.Cache.BypassCookies) {
			return nil, fmt.Errorf("cache.allowedCookies: %q is also a bypass cookie", name)
		}
	}
	cfg.Cache.ESICookies, err = cookiePrefixes("cache.esiCookies", f.Cache.ESICookies)
	if err != nil {
		return nil, err
	}
	for _, prefix := range cfg.Cache.ESICookies {
		overlaps := false
		for _, bypass := range cfg.Cache.BypassCookies {
			overlaps = overlaps || strings.HasPrefix(prefix, bypass) || strings.HasPrefix(bypass, prefix)
		}
		if !overlaps {
			return nil, fmt.Errorf("cache.esiCookies: %q isn't one of the bypass cookies", prefix)
		}
	}
	cfg.Cache.StripCookies, err = cookiePrefixes("cache.stripCookies", f.Cache.StripCookies)
	if err != nil {
		return nil, err
	}
	for _, prefix := range cfg.Cache.StripCookies {
		for _, bypass := range cfg.Cache.BypassCookies {
			if strings.HasPrefix(prefix, bypass) || strings.HasPrefix(bypass, prefix) {
				return nil, fmt.Errorf("cache.stripCookies: %q overlaps the bypass cookie %q, which marks personal pages", prefix, bypass)
			}
		}
	}
	if f.Cache.QueryParams != nil {
		cfg.Cache.QueryParams = append([]string{}, *f.Cache.QueryParams...)
	}

	if err := parseWarm(cfg, f.Warm.Sitemap, f.Warm.Rate, f.Warm.MaxPages, f.Warm.Interval); err != nil {
		return nil, err
	}

	cfg.Control.AllowIPs, err = fetch.ParseIPs(f.Control.AllowIPs)
	if err != nil {
		return nil, fmt.Errorf("control.allowIPs: %w", err)
	}
	if f.Control.Secret != "" && len(f.Control.Secret) < MinSecretLength {
		return nil, fmt.Errorf("control.secret must be at least %d characters", MinSecretLength)
	}
	if f.Control.SecretHeader != "" && f.Control.Secret == "" {
		return nil, fmt.Errorf("control.secretHeader is set, but control.secret isn't")
	}
	cfg.Control.Secret = f.Control.Secret
	cfg.Control.SecretHeader = f.Control.SecretHeader

	for i, r := range f.Routes {
		if r.Match == "" {
			return nil, fmt.Errorf("route %d: match is required", i+1)
		}
		route := Route{Match: r.Match, Cache: r.Cache == nil || *r.Cache}
		if r.Regex {
			route.Regex, err = regexp.Compile(r.Match)
			if err != nil {
				return nil, fmt.Errorf("route %d: %w", i+1, err)
			}
		}
		if r.DefaultTTL != "" {
			route.DefaultTTL, err = parseDuration(r.DefaultTTL)
			if err != nil || route.DefaultTTL < time.Second {
				return nil, fmt.Errorf("route %d (%q): defaultTTL must be a duration of at least 1s, e.g. \"1h\" or \"7d\"", i+1, r.Match)
			}
		}
		if r.StaleIfError != "" {
			staleIfError, err := parseDuration(r.StaleIfError)
			if err != nil || staleIfError < 0 {
				return nil, fmt.Errorf("route %d (%q): staleIfError must be a duration, e.g. \"1h\", or \"0\" to turn it off", i+1, r.Match)
			}
			route.StaleIfError = &staleIfError
		}
		if r.StaleWhileRev != "" {
			staleWhileRevalidate, err := parseDuration(r.StaleWhileRev)
			if err != nil || staleWhileRevalidate < 0 {
				return nil, fmt.Errorf("route %d (%q): staleWhileRevalidate must be a duration, e.g. \"1d\", or \"0\" to turn it off", i+1, r.Match)
			}
			route.StaleWhileRevalidate = &staleWhileRevalidate
		}
		if r.QueryParams != nil {
			params := append([]string{}, *r.QueryParams...)
			route.QueryParams = &params
		}
		if r.BypassCookies != nil {
			prefixes, err := cookiePrefixes(fmt.Sprintf("route %d (%q): bypassCookies", i+1, r.Match), *r.BypassCookies)
			if err != nil {
				return nil, err
			}
			route.BypassCookies = &prefixes
		}
		if r.BrowserCC != nil {
			cc := *r.BrowserCC
			if cc == "inherit" {
				cc = ""
			} else if err := checkCacheControl(fmt.Sprintf("route %d (%q): browserCacheControl", i+1, r.Match), cc); err != nil {
				return nil, err
			}
			route.BrowserCacheControl = &cc
		}
		overrides := route.DefaultTTL != 0 || route.StaleIfError != nil || route.StaleWhileRevalidate != nil || route.QueryParams != nil ||
			route.BypassCookies != nil || route.BrowserCacheControl != nil
		if !route.Cache && overrides {
			return nil, fmt.Errorf("route %d (%q): a route with cache = false can't have other settings", i+1, r.Match)
		}
		if route.Cache && !overrides {
			return nil, fmt.Errorf("route %d (%q): set cache = false, or a setting to override (defaultTTL, staleIfError, staleWhileRevalidate, queryParams, bypassCookies or browserCacheControl)", i+1, r.Match)
		}
		cfg.Routes = append(cfg.Routes, route)
	}
	return cfg, nil
}

func parseWarm(cfg *Config, sitemap string, rate float64, maxPages int, interval string) error {
	if sitemap == "" {
		if rate != 0 || maxPages != 0 || interval != "" {
			return fmt.Errorf("warm.sitemap is required to warm the cache")
		}
		return nil
	}
	u, err := url.Parse(sitemap)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("warm.sitemap must be the site's sitemap's full URL, e.g. \"https://www.example.com/wp-sitemap.xml\"")
	}
	if rate < 0 || rate > 100 {
		return fmt.Errorf("warm.rate must be a number of pages a second, up to 100")
	}
	if maxPages < 0 {
		return fmt.Errorf("warm.maxPages can't be negative")
	}
	if cfg.Hosts != nil && !fetch.HostAllowed(u.Host, cfg.Hosts) {
		return fmt.Errorf("warm.sitemap: %s isn't one of the hosts", u.Host)
	}
	cfg.Warm = Warm{Sitemap: u, Rate: rate, MaxPages: maxPages}
	if interval != "" {
		cfg.Warm.Interval, err = parseDuration(interval)
		if err != nil || cfg.Warm.Interval < 0 || (cfg.Warm.Interval > 0 && cfg.Warm.Interval < time.Minute) {
			return fmt.Errorf("warm.interval must be a duration of at least 1m (or \"0\" to only warm at startup and after purging everything), e.g. \"6h\"")
		}
	}
	return nil
}

// hostPattern is a host name, or "*." and a domain
var hostPattern = regexp.MustCompile(`^(\*\.)?[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$`)

// parseHosts validates the hosts setting
func parseHosts(hosts []string) ([]string, error) {
	if len(hosts) == 0 {
		return nil, fmt.Errorf("hosts: list the site's host names, e.g. [\"www.example.com\", \"example.com\"], or leave it out to allow any")
	}
	parsed := make([]string, 0, len(hosts))
	for _, host := range hosts {
		if !hostPattern.MatchString(host) {
			return nil, fmt.Errorf("hosts: %q isn't a host name (without a scheme or port), e.g. \"www.example.com\" or \"*.example.com\"", host)
		}
		parsed = append(parsed, strings.ToLower(host))
	}
	return parsed, nil
}

// cacheControlDirective is a Cache-Control directive, e.g. "max-age=60",
// "immutable" or no-cache="set-cookie"
var cacheControlDirective = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_|~-]+(=([A-Za-z0-9!#$%&'*+.^_|~-]+|"[^"\\]*"))?$`)

// checkCacheControl validates a Cache-Control header value
func checkCacheControl(setting, value string) error {
	for _, directive := range strings.Split(value, ",") {
		if !cacheControlDirective.MatchString(strings.TrimSpace(directive)) {
			return fmt.Errorf("%s: %q isn't a Cache-Control header, e.g. \"public, max-age=3600\"", setting, value)
		}
	}
	return nil
}

// cookiePrefixes validates a list of cookie name prefixes
func cookiePrefixes(setting string, prefixes []string) ([]string, error) {
	for _, prefix := range prefixes {
		// An empty prefix would match every cookie, and these characters
		// can't be in a cookie name
		if prefix == "" || strings.ContainsAny(prefix, "=; \t") {
			return nil, fmt.Errorf("%s: %q isn't a cookie name (or the start of one)", setting, prefix)
		}
	}
	return append([]string{}, prefixes...), nil
}

func hasAnyPrefix(name string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

var sizeUnits = map[string]int64{
	"":   1,
	"B":  1,
	"KB": 1 << 10,
	"MB": 1 << 20,
	"GB": 1 << 30,
}

// parseSize parses a size such as "256MB" (units are powers of 1024)
func parseSize(s string) (int64, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	i := strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' })
	if i < 0 {
		i = len(s)
	}
	number, unit := s[:i], strings.TrimSpace(s[i:])
	multiplier, ok := sizeUnits[unit]
	n, err := strconv.ParseInt(number, 10, 64)
	if !ok || err != nil || n <= 0 {
		return 0, fmt.Errorf("%q should be a size such as \"256MB\"", s)
	}
	return n * multiplier, nil
}

// parseDuration parses a duration such as "90s" or "1h30m", or a whole
// number of days such as "7d".
func parseDuration(s string) (time.Duration, error) {
	if days, found := strings.CutSuffix(s, "d"); found {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%q should be a duration such as \"1h\" or \"7d\"", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}
