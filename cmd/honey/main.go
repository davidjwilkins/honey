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
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/davidjwilkins/honey/cache"
	"github.com/davidjwilkins/honey/config"
	"github.com/davidjwilkins/honey/fetch"
)

func main() {
	configPath := flag.String("config", "honey.toml", "path to the config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatal(err)
	}

	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           newHandler(cfg),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		log.Print("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()

	log.Printf("honey listening on %s, proxying to %s", cfg.Listen, cfg.Backend)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// newHandler builds the caching proxy described by cfg.
func newHandler(cfg *config.Config) http.Handler {
	cacher := cache.NewCacher(cache.Options{
		MaxBytes:        cfg.Cache.MaxBytes,
		MaxObjectBytes:  cfg.Cache.MaxObjectBytes,
		DefaultTTL:      cfg.Cache.DefaultTTL,
		SkipStaticFiles: !cfg.Cache.StaticFiles,
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
	return fetch.Fetch(cacher, fetch.Forwarder(cacher), cfg.Backend)
}
