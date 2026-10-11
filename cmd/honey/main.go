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

	handler, metricsHandler := newHandlers(cfg)
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
	go func() {
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
}

// newHandler builds the caching proxy described by cfg.
func newHandler(cfg *config.Config) http.Handler {
	handler, _ := newHandlers(cfg)
	return handler
}

// newHandlers builds the caching proxy described by cfg, and the handler
// for its metrics if they are enabled (or else nil).
func newHandlers(cfg *config.Config) (handler http.Handler, metricsHandler http.Handler) {
	cacher := cache.NewCacher(cache.Options{
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
		cacher.AddAllowedCookie(name)
	}
	for _, route := range cfg.Routes {
		if route.Regex != nil {
			cacher.AddSkipRegex(route.Regex)
		} else {
			cacher.AddSkipPrefix(route.Match)
		}
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
		recorder = metrics.New(cacher.Stats, maxBytes)
		transport = recorder.Transport(transport)
	}
	handler = fetch.FetchWithOptions(cacher, fetch.NewForwarder(cacher, transport), cfg.Backend, fetch.Options{
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
	return handler, metricsHandler
}

// accessLogOutput is where the access log is written
var accessLogOutput io.Writer = os.Stdout
