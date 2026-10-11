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
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/davidjwilkins/honey/accesslog"
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

	handler, metricsHandler, cacher := newHandlers(cfg)
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
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	var metricsServer *http.Server
	if metricsHandler != nil {
		mux := http.NewServeMux()
		mux.Handle("/metrics", metricsHandler)
		metricsServer = &http.Server{Addr: cfg.MetricsListen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			log.Printf("metrics on http://%s/metrics", cfg.MetricsListen)
			if err := metricsServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				log.Fatal(err)
			}
		}()
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
		if metricsServer != nil {
			metricsServer.Shutdown(shutdownCtx)
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

// newHandler builds the caching proxy described by cfg.
func newHandler(cfg *config.Config) http.Handler {
	handler, _, _ := newHandlers(cfg)
	return handler
}

// newHandlers builds the caching proxy described by cfg, and the handler
// for its metrics if they are enabled (or else nil).
func newHandlers(cfg *config.Config) (handler http.Handler, metricsHandler http.Handler, cacher snapshotter) {
	c := cache.NewCacher(cache.Options{
		MaxBytes:         cfg.Cache.MaxBytes,
		MaxObjectBytes:   cfg.Cache.MaxObjectBytes,
		DefaultTTL:       cfg.Cache.DefaultTTL,
		StaleIfError:     cfg.Cache.StaleIfError,
		SkipStaticFiles:  !cfg.Cache.StaticFiles,
		DisableBrotli:    !cfg.Cache.Brotli,
		DisableGzip:      !cfg.Cache.Gzip,
		CompressMinBytes: cfg.Cache.CompressMinBytes,
	})
	for _, name := range cfg.Cache.AllowedCookies {
		c.AddAllowedCookie(name)
	}
	for _, route := range cfg.Routes {
		r := cache.Route{
			Regex:        route.Regex,
			NoCache:      !route.Cache,
			DefaultTTL:   route.DefaultTTL,
			StaleIfError: route.StaleIfError,
			QueryParams:  route.QueryParams,
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
	if cfg.MetricsListen != "" {
		maxBytes := cfg.Cache.MaxBytes
		if maxBytes == 0 {
			maxBytes = cache.DefaultMaxBytes
		}
		recorder = metrics.New(c.Stats, maxBytes)
		transport = recorder.Transport(transport)
	}
	handler = fetch.FetchWithOptions(c, fetch.NewForwarder(c, transport), cfg.Backend, fetch.Options{
		Control:     control,
		QueryParams: cfg.Cache.QueryParams,
	})
	if recorder != nil {
		handler = recorder.Middleware(handler)
		metricsHandler = recorder
	}
	if cfg.AccessLog {
		logger := accesslog.New(accessLogOutput, accesslog.Format(cfg.AccessLogFormat))
		handler = accesslog.Middleware(logger, handler)
	}
	return handler, metricsHandler, c
}

// accessLogOutput is where the access log is written
var accessLogOutput io.Writer = os.Stdout
