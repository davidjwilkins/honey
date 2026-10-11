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
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/davidjwilkins/honey/accesslog"
	"github.com/davidjwilkins/honey/admin"
	"github.com/davidjwilkins/honey/cache"
	"github.com/davidjwilkins/honey/config"
	"github.com/davidjwilkins/honey/fetch"
	"github.com/davidjwilkins/honey/metrics"
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

	h := build(cfg, time.Now())
	cacher := h.cacher
	if cfg.Cache.Persist != "" {
		loaded, err := cacher.LoadFrom(cfg.Cache.Persist)
		if err != nil {
			// Start anyway: the cache will fill up again
			log.Printf("loading the cache: %v", err)
		}
		log.Printf("loaded %d cached responses from %s", loaded, cfg.Cache.Persist)
	}
	stopPersisting := persistEvery(cacher, cfg.Cache.Persist, cfg.Cache.PersistInterval)
	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h.handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	// The metrics and admin page are served on their own (private) addresses
	var private []*http.Server
	serve := func(addr string, handler http.Handler, what, path string) {
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
		serve(cfg.MetricsListen, mux, "metrics", "metrics")
	}
	if h.admin != nil {
		serve(cfg.AdminListen, h.admin, "admin page", "")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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

	log.Printf("honey %s listening on %s, proxying to %s", version, cfg.Listen, cfg.Backend)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	// ListenAndServe returns as soon as Shutdown starts; wait for the
	// requests in progress to finish before saving the cache and exiting
	<-drained
	stopPersisting()
	if cfg.Cache.Persist != "" {
		saveCache(cacher, cfg.Cache.Persist)
	}
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
}

// newHandler builds the caching proxy described by cfg.
func newHandler(cfg *config.Config) http.Handler {
	return build(cfg, time.Now()).handler
}

// build builds the caching proxy described by cfg, which started at
// started.
func build(cfg *config.Config, started time.Time) *honey {
	c := cache.NewCacher(cache.Options{
		MaxBytes:            cfg.Cache.MaxBytes,
		MaxObjectBytes:      cfg.Cache.MaxObjectBytes,
		DefaultTTL:          cfg.Cache.DefaultTTL,
		StaleIfError:        cfg.Cache.StaleIfError,
		SkipStaticFiles:     !cfg.Cache.StaticFiles,
		DisableBrotli:       !cfg.Cache.Brotli,
		DisableGzip:         !cfg.Cache.Gzip,
		CompressMinBytes:    cfg.Cache.CompressMinBytes,
		BypassCookies:       cfg.Cache.BypassCookies,
		BrowserCacheControl: cfg.Cache.BrowserCacheControl,
	})
	for _, name := range cfg.Cache.AllowedCookies {
		c.AddAllowedCookie(name)
	}
	for _, route := range cfg.Routes {
		r := cache.Route{
			Regex:               route.Regex,
			NoCache:             !route.Cache,
			DefaultTTL:          route.DefaultTTL,
			StaleIfError:        route.StaleIfError,
			QueryParams:         route.QueryParams,
			BypassCookies:       route.BypassCookies,
			BrowserCacheControl: route.BrowserCacheControl,
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
		transport = recorder.Transport(transport)
	}
	fetchOpts := fetch.Options{
		Control:     control,
		QueryParams: cfg.Cache.QueryParams,
		BackendHost: cfg.BackendHost,
	}
	h := &honey{
		handler: fetch.FetchWithOptions(c, fetch.NewForwarder(c, transport), cfg.Backend, fetchOpts),
		metrics: recorder,
		cacher:  c,
	}
	if recorder != nil {
		h.handler = recorder.Middleware(h.handler)
	}
	if cfg.AccessLog {
		logger := accesslog.New(accessLogOutput, accesslog.Format(cfg.AccessLogFormat))
		h.handler = accesslog.Middleware(logger, h.handler)
	}
	if cfg.AdminListen != "" {
		h.admin = admin.Handler(admin.Options{
			Version: version,
			Backend: cfg.Backend.String(),
			Started: started,
			Metrics: recorder,
			Purge:   fetch.Purger(c, cfg.Backend, fetchOpts),
		})
	}
	return h
}

// accessLogOutput is where the access log is written
var accessLogOutput io.Writer = os.Stdout
