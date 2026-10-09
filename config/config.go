// Package config loads Honey's TOML configuration file.
package config

import (
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
	"github.com/davidjwilkins/honey/fetch"
)

// DefaultListen is the address Honey listens on if none is configured.
const DefaultListen = ":8080"

// Config is a validated Honey configuration.
type Config struct {
	// Listen is the address to listen on, e.g. ":8080"
	Listen string
	// Backend is the server which requests are proxied to
	Backend *url.URL
	Cache   Cache
	Routes  []Route
	Control Control
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
	AllowedCookies []string
	// StaticFiles is whether static files (images, css, js...) are cached
	StaticFiles bool
	// Brotli and Gzip are whether compressible responses are compressed
	// with each, for clients which accept them
	Brotli           bool
	Gzip             bool
	CompressMinBytes int64
}

// Route is a rule for requests whose path starts with Match, or, if
// Regex is set, whose path and query string match it.  The only rule
// currently supported is not caching the matching requests.
type Route struct {
	Match string
	// Regex is the compiled Match, if the route is a regular expression
	Regex *regexp.Regexp
	Cache bool
}

// file is the layout of the TOML file
type file struct {
	Listen  string `toml:"listen"`
	Backend struct {
		URI string `toml:"uri"`
	} `toml:"backend"`
	Cache struct {
		MaxSize         string   `toml:"maxSize"`
		MaxObjectSize   string   `toml:"maxObjectSize"`
		DefaultTTL      string   `toml:"defaultTTL"`
		AllowedCookies  []string `toml:"allowedCookies"`
		StaticFiles     *bool    `toml:"staticFiles"`
		Brotli          *bool    `toml:"brotli"`
		Gzip            *bool    `toml:"gzip"`
		CompressMinSize string   `toml:"compressMinSize"`
	} `toml:"cache"`
	Control struct {
		AllowIPs     []string `toml:"allowIPs"`
		Secret       string   `toml:"secret"`
		SecretHeader string   `toml:"secretHeader"`
	} `toml:"control"`
	Routes []struct {
		Match string `toml:"match"`
		Regex bool   `toml:"regex"`
		Cache *bool  `toml:"cache"`
	} `toml:"route"`
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
	var f file
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

	cfg := &Config{Listen: f.Listen}
	if cfg.Listen == "" {
		cfg.Listen = DefaultListen
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
	cfg.Cache.Brotli = f.Cache.Brotli == nil || *f.Cache.Brotli
	cfg.Cache.Gzip = f.Cache.Gzip == nil || *f.Cache.Gzip
	if f.Cache.CompressMinSize != "" {
		cfg.Cache.CompressMinBytes, err = parseSize(f.Cache.CompressMinSize)
		if err != nil {
			return nil, fmt.Errorf("cache.compressMinSize: %w", err)
		}
	}
	if f.Cache.DefaultTTL != "" {
		cfg.Cache.DefaultTTL, err = time.ParseDuration(f.Cache.DefaultTTL)
		if err != nil || cfg.Cache.DefaultTTL < time.Second {
			return nil, fmt.Errorf("cache.defaultTTL must be a duration of at least 1s, e.g. \"5m\"")
		}
	}
	cfg.Cache.AllowedCookies = f.Cache.AllowedCookies

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
		if r.Cache == nil || *r.Cache {
			return nil, fmt.Errorf("route %d (%q): only cache = false is supported", i+1, r.Match)
		}
		route := Route{Match: r.Match, Cache: *r.Cache}
		if r.Regex {
			route.Regex, err = regexp.Compile(r.Match)
			if err != nil {
				return nil, fmt.Errorf("route %d: %w", i+1, err)
			}
		}
		cfg.Routes = append(cfg.Routes, route)
	}
	return cfg, nil
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
