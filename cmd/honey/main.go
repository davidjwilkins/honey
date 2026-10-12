// Command honey runs the Honey caching reverse proxy.
//
// Usage:
//
//	honey -config honey.toml
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/davidjwilkins/honey/accesslog"
	"github.com/davidjwilkins/honey/admin"
	"github.com/davidjwilkins/honey/cache"
	"github.com/davidjwilkins/honey/cdn"
	"github.com/davidjwilkins/honey/clientip"
	"github.com/davidjwilkins/honey/config"
	"github.com/davidjwilkins/honey/events"
	"github.com/davidjwilkins/honey/fetch"
	"github.com/davidjwilkins/honey/guard"
	"github.com/davidjwilkins/honey/metrics"
	"github.com/davidjwilkins/honey/optimize"
	"github.com/davidjwilkins/honey/serve"
	"github.com/davidjwilkins/honey/warm"
)

// version is set when building a release, with
// -ldflags "-X main.version=v1.2.3"
var version = "dev"

func main() {
	configPath := flag.String("config", "honey.toml", "path to the config file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("honey", version)
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatal(err)
	}

	if limit, set := memoryLimit(cfg.Cache.MaxBytes, os.Getenv("GOMEMLIMIT")); set {
		debug.SetMemoryLimit(limit)
		log.Printf("memory limit %dMB (for a %dMB cache; set GOMEMLIMIT to change it)", limit>>20, effectiveMaxBytes(cfg.Cache.MaxBytes)>>20)
	}
	h := build(cfg, time.Now())
	cacher := h.cacher
	if cfg.Cache.Persist != "" {
		loaded, err := cacher.LoadFrom(cfg.Cache.Persist)
		if err != nil {
			// Start anyway: the cache will fill up again
			log.Printf("loading the cache: %v", err)
			h.events.Add(events.Error, "", 0, events.Text(fmt.Sprintf("couldn't load the cache: %v", err)))
		}
		log.Printf("loaded %d cached responses from %s", loaded, cfg.Cache.Persist)
		h.events.Add(events.Info, "", 0, events.Text(fmt.Sprintf("started, with %d cached responses from %s", loaded, cfg.Cache.Persist)))
	} else {
		h.events.Add(events.Info, "", 0, events.Text("started"))
	}
	stopPersisting := persistEvery(cacher, cfg.Cache.Persist, cfg.Cache.PersistInterval)
	server, err := serve.New(serveOptions(cfg, h.handler))
	if err != nil {
		log.Fatal(err)
	}
	if err := server.Listen(); err != nil {
		log.Fatal(err)
	}
	// The metrics and admin page are served on their own (private) addresses
	var private []*http.Server
	servePrivate := func(addr string, handler http.Handler, what, path string) {
		s := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
		private = append(private, s)
		go func() {
			log.Printf("%s on %s%s", what, displayURL(addr), path)
			if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				log.Fatal(err)
			}
		}()
	}
	if cfg.MetricsListen != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", h.metrics)
		servePrivate(cfg.MetricsListen, mux, "metrics", "metrics")
	}
	if h.admin != nil {
		servePrivate(cfg.AdminListen, h.admin, "admin page", "")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if h.warmer != nil {
		// After loading any saved cache, so that it only fetches what's
		// missing
		go h.warmer.Run(ctx)
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-ctx.Done()
		log.Print("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, s := range private {
			s.Shutdown(shutdownCtx)
		}
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()

	plain, secure := server.Addrs()
	if cfg.TLS != nil {
		protocols := "HTTPS"
		if cfg.TLS.HTTP3 {
			protocols += " and HTTP/3"
		}
		log.Printf("honey %s listening on %s, with %s on %s, proxying to %s", version, plain, protocols, secure, cfg.Backend)
	} else {
		log.Printf("honey %s listening on %s, proxying to %s", version, plain, cfg.Backend)
	}
	if err := server.Serve(); err != nil {
		log.Fatal(err)
	}
	// Serve returns as soon as Shutdown starts; wait for the requests in
	// progress to finish before saving the cache and exiting
	<-drained
	stopPersisting()
	// Purges waiting to be passed on
	h.cdn.Flush()
	if cfg.Cache.Persist != "" {
		saveCache(cacher, cfg.Cache.Persist)
	}
}

// serveOptions returns the options for the servers for visitors
func serveOptions(cfg *config.Config, handler http.Handler) serve.Options {
	opts := serve.Options{Listen: cfg.Listen, Handler: handler}
	if cfg.Hosts != nil {
		opts.HostAllowed = func(host string) bool { return fetch.HostAllowed(host, cfg.Hosts) }
	}
	if t := cfg.TLS; t != nil {
		opts.TLS = &serve.TLS{
			Listen: t.Listen, Auto: t.Auto, Hosts: cfg.Hosts, Email: t.Email, CertDir: t.CertDir, CA: t.CA,
			CertFile: t.CertFile, KeyFile: t.KeyFile, HTTP3: t.HTTP3, Redirect: t.Redirect,
		}
	}
	return opts
}

// memoryLimit returns the soft memory limit for Go's garbage collector, for
// a cache of maxBytes, unless GOMEMLIMIT (gomemlimit) sets one.  By default,
// Go lets its memory grow to twice what's in use before collecting the
// garbage, which for a full cache is several times maxSize.  With the
// limit, it collects more often as memory nears the cache's size, plus
// room for the requests in progress and the compressors.
func memoryLimit(maxBytes int64, gomemlimit string) (int64, bool) {
	if gomemlimit != "" {
		return 0, false
	}
	maxBytes = effectiveMaxBytes(maxBytes)
	return maxBytes + maxBytes/4 + 128<<20, true
}

func effectiveMaxBytes(maxBytes int64) int64 {
	if maxBytes <= 0 {
		return cache.DefaultMaxBytes
	}
	return maxBytes
}

// snapshotter is a cache which can be saved to and loaded from a file
type snapshotter interface {
	SaveTo(path string) (int, error)
	LoadFrom(path string) (int, error)
}

func saveCache(cacher snapshotter, path string) {
	start := time.Now()
	saved, err := cacher.SaveTo(path)
	if err != nil {
		log.Printf("saving the cache: %v", err)
		return
	}
	log.Printf("saved %d cached responses to %s in %s", saved, path, time.Since(start).Round(time.Millisecond))
}

// persistEvery saves the cache to path every interval, until the returned
// function is called.  It does nothing if path or interval is unset.
func persistEvery(cacher snapshotter, path string, interval time.Duration) (stop func()) {
	if path == "" || interval <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				saveCache(cacher, path)
			case <-done:
				return
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}

// displayURL returns the URL to show for a server listening on addr
func displayURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host == "" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

// honey is the caching proxy described by a config
type honey struct {
	handler http.Handler
	// metrics are the proxy's counts, if metrics or the admin page are on
	metrics *metrics.Recorder
	// admin serves the admin page, if it is on
	admin  http.Handler
	cacher snapshotter
	// warmer warms the cache from the sitemap, if it is on
	warmer *warm.Warmer
	// events are what the admin page shows has happened, if it is on
	events *events.Log
	// cdn passes purges on to a CDN, if there is one
	cdn *cdn.Purger
}

// newHandler builds the caching proxy described by cfg.
func newHandler(cfg *config.Config) http.Handler {
	return build(cfg, time.Now()).handler
}

// build builds the caching proxy described by cfg, which started at
// started.
func build(cfg *config.Config, started time.Time) *honey {
	// What's worth knowing about, for the admin page
	var eventLog *events.Log
	if cfg.AdminListen != "" {
		eventLog = events.New(events.DefaultMax)
	}
	// Purges are passed on to the CDN in front of Honey, if there is one
	var purger *cdn.Purger
	if cfg.CDN != nil {
		var err error
		purger, err = cdn.New(cdn.Options{Provider: cfg.CDN.Provider, Zone: cfg.CDN.Zone, Token: cfg.CDN.Token,
			Hosts: cfg.CDN.Hosts, Events: eventLog})
		if err != nil {
			log.Printf("cdn: %v", err)
		}
	}
	// Pages are changed as they're cached, so that browsers load them
	// faster; once a page's main image is learned, it's cached again with
	// it preloaded
	var refresh func(host, path string)
	optimizer := optimize.New(optimize.Options{
		DelayScripts:     cfg.Optimize.DelayScripts,
		DelayMatching:    cfg.Optimize.DelayScriptsMatching,
		KeepMatching:     cfg.Optimize.KeepScriptsMatching,
		DelayTimeout:     cfg.Optimize.DelayScriptsTimeout,
		PreloadMainImage: cfg.Optimize.PreloadMainImage,
		SelfHostFonts:    cfg.Optimize.SelfHostFonts,
		BackendHost:      cfg.BackendHost,
		Events:           eventLog,
		Refresh:          func(host, path string) { refresh(host, path) },
	})
	var rewrite func(*http.Response, []byte) []byte
	if optimizer != nil {
		rewrite = optimizer.Rewrite
	}
	c := cache.NewCacher(cache.Options{
		MaxBytes:             cfg.Cache.MaxBytes,
		MaxObjectBytes:       cfg.Cache.MaxObjectBytes,
		DefaultTTL:           cfg.Cache.DefaultTTL,
		StaleIfError:         cfg.Cache.StaleIfError,
		StaleWhileRevalidate: cfg.Cache.StaleWhileRevalidate,
		SkipStaticFiles:      !cfg.Cache.StaticFiles,
		DisableBrotli:        !cfg.Cache.Brotli,
		DisableGzip:          !cfg.Cache.Gzip,
		CompressMinBytes:     cfg.Cache.CompressMinBytes,
		BypassCookies:        cfg.Cache.BypassCookies,
		ESICookies:           cfg.Cache.ESICookies,
		StripCookies:         cfg.Cache.StripCookies,
		BrowserCacheControl:  cfg.Cache.BrowserCacheControl,
		Events:               eventLog,
		RewritePage:          rewrite,
	})
	refresh = func(host, path string) {
		c.Expire(&url.URL{Path: path}, false)
		purger.Purge(path, false, nil)
	}
	for _, name := range cfg.Cache.AllowedCookies {
		c.AddAllowedCookie(name)
	}
	for _, route := range cfg.Routes {
		r := cache.Route{
			Regex:                route.Regex,
			NoCache:              !route.Cache,
			DefaultTTL:           route.DefaultTTL,
			StaleIfError:         route.StaleIfError,
			StaleWhileRevalidate: route.StaleWhileRevalidate,
			QueryParams:          route.QueryParams,
			BypassCookies:        route.BypassCookies,
			BrowserCacheControl:  route.BrowserCacheControl,
		}
		if route.Regex == nil {
			r.Prefix = route.Match
		}
		c.AddRoute(r)
	}
	control := fetch.Control{
		AllowIPs:     cfg.Control.AllowIPs,
		Secret:       cfg.Control.Secret,
		SecretHeader: cfg.Control.SecretHeader,
	}
	backendTransport := http.DefaultTransport.(*http.Transport).Clone()
	backendTransport.ResponseHeaderTimeout = cfg.BackendTimeout
	transport := fetch.StallTimeout(backendTransport, cfg.BackendStallTimeout)
	var recorder *metrics.Recorder
	if cfg.MetricsListen != "" || cfg.AdminListen != "" {
		maxBytes := cfg.Cache.MaxBytes
		if maxBytes == 0 {
			maxBytes = cache.DefaultMaxBytes
		}
		recorder = metrics.New(c.Stats, maxBytes)
		recorder.LogTo(eventLog)
		transport = recorder.Transport(transport)
	}
	h := &honey{metrics: recorder, cacher: c, events: eventLog, cdn: purger}
	// Addresses which keep failing to log in, and (if it's on) their
	// requests to WordPress.  Requests from this host (e.g. WordPress's
	// own) are never limited.
	limits := guard.NewLimits(guard.LimitOptions{
		LoginAttempts: cfg.Security.LoginAttempts,
		LoginLockout:  cfg.Security.LoginLockout,
		Rate:          cfg.Security.RateLimit,
		Burst:         cfg.Security.RateBurst,
		ProbeLimit:    cfg.Security.ProbeLimit,
		ProbeBan:      cfg.Security.ProbeBan,
		Allow:         append(loopback(), cfg.Security.AllowIPs...),
		Events:        eventLog,
	})
	fetchOpts := fetch.Options{
		Control:     control,
		QueryParams: cfg.Cache.QueryParams,
		BackendHost: cfg.BackendHost,
		Hosts:       cfg.Hosts,
		Events:      eventLog,
		EarlyHints:  cfg.Cache.EarlyHints,
		BackendGate: limits.AllowBackend,
		HideUsers:   cfg.Security.HideUsers,
		ListPurged:  purger != nil,
		OnPurge: func(p fetch.Purged) {
			purger.Purge(p.Path, p.Prefix, p.Cached)
			// Everything has been purged: fill the cache again
			if h.warmer != nil && p.Prefix && p.Path == "/" {
				h.warmer.Trigger()
			}
		},
	}
	if purger != nil {
		// Pages showing purged shared fragments are updated in the cache
		// without being purged, but the CDN's copies need purging
		fetchOpts.OnFragmentPurge = func(names string) {
			shows := func(fragments []string) bool {
				for _, name := range strings.Split(names, ",") {
					name = strings.TrimSpace(name)
					prefix, isPrefix := strings.CutSuffix(name, "*")
					for _, f := range fragments {
						if name != "" && (f == name || (isPrefix && strings.HasPrefix(f, prefix))) {
							return true
						}
					}
				}
				return false
			}
			for _, e := range c.Entries() {
				if shows(e.Fragments) {
					purger.Purge(e.Path, false, nil)
				}
			}
		}
	}
	h.handler = fetch.FetchWithOptions(c, fetch.NewForwarder(c, transport), cfg.Backend, fetchOpts)
	// The cache itself, without the metrics, access log and so on
	proxy := h.handler
	if cfg.Warm.Sitemap != nil {
		// It requests pages from the cache itself, without the metrics or
		// access log, so that warming isn't counted as visits
		h.warmer = warm.New(h.handler, warm.Options{
			Sitemap:  cfg.Warm.Sitemap,
			Rate:     cfg.Warm.Rate,
			MaxPages: cfg.Warm.MaxPages,
			Interval: cfg.Warm.Interval,
			Events:   eventLog,
		})
	}
	// Browsers report pages' main images to Honey itself
	h.handler = optimizer.Middleware(h.handler)
	if purger != nil {
		// The CDN mustn't keep pages Honey doesn't, or a page Honey serves
		// while it refreshes it
		h.handler = cdn.Protect(h.handler)
	}
	// Visitors' requests are guarded; the warmer's and purges' aren't
	h.handler = guard.Middleware(guard.Options{
		BlockPaths: cfg.Security.BlockPaths,
		XMLRPC:     cfg.Security.XMLRPC,
		Headers:    cfg.Security.Headers,
		HSTS:       cfg.Security.HSTS,
		AdminIPs:   adminIPs(cfg.Security.AdminIPs),
		HideUsers:  cfg.Security.HideUsers,
		Limits:     limits,
		Events:     eventLog,
	}, h.handler)
	if recorder != nil {
		h.handler = recorder.Middleware(h.handler)
	}
	if cfg.AccessLog {
		logger := accesslog.New(accessLogOutput, accesslog.Format(cfg.AccessLogFormat))
		h.handler = accesslog.Middleware(logger, h.handler)
	}
	// First, so that everything after it knows who each visitor is
	h.handler = clientip.Middleware(cfg.TrustedProxies, h.handler)
	if cfg.AdminListen != "" {
		h.admin = admin.Handler(admin.Options{
			Version: version,
			Backend: cfg.Backend.String(),
			Hosts:   cfg.Hosts,
			Started: started,
			Metrics: recorder,
			Events:  eventLog,
			Purge:   fetch.Purger(c, cfg.Backend, fetchOpts),
			Count:   fetch.Counter(c, cfg.Backend, fetchOpts),
			Explain: func(r *http.Request) fetch.Explanation {
				return fetch.Explain(c, cfg.Backend, fetchOpts, r)
			},
			Origin:  siteOrigin(cfg),
			Entries: c.Entries,
			Entry:   c.Entry,
			// Pages are refreshed for fragments by the cache itself
			Fragments:      c.Fragments,
			PurgeFragments: fetch.FragmentPurger(c, proxy),
			Settings:       cfg.File,
			Lockouts:       limits.Lockouts,
			Unblock:        limits.Unblock,
		})
	}
	return h
}

// loopback returns the loopback networks
func loopback() []*net.IPNet {
	var networks []*net.IPNet
	for _, cidr := range []string{"127.0.0.0/8", "::1/128"} {
		_, n, _ := net.ParseCIDR(cidr)
		networks = append(networks, n)
	}
	return networks
}

// adminIPs returns the addresses the dashboard is served to: nil (any), or
// those configured and this host's
func adminIPs(configured []*net.IPNet) []*net.IPNet {
	if configured == nil {
		return nil
	}
	return append(loopback(), configured...)
}

// siteOrigin guesses the site's scheme and host (e.g.
// https://www.example.com), for paths tested on the admin page: from the
// sitemap, if the cache is warmed, or else the first of the hosts.
func siteOrigin(cfg *config.Config) string {
	if cfg.Warm.Sitemap != nil {
		return cfg.Warm.Sitemap.Scheme + "://" + cfg.Warm.Sitemap.Host
	}
	for _, host := range cfg.Hosts {
		if !strings.HasPrefix(host, "*.") {
			return "https://" + host
		}
	}
	return ""
}

// accessLogOutput is where the access log is written
var accessLogOutput io.Writer = os.Stdout
