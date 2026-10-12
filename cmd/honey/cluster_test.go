package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandlerCluster(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<p>"+r.URL.Path+"</p>")
	}))
	defer origin.Close()

	// Two Honeys, each with the other (and itself) as peers
	var listeners []net.Listener
	var peers string
	for i := 0; i < 2; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		listeners = append(listeners, l)
		if peers != "" {
			peers += ", "
		}
		peers += `"http://` + l.Addr().String() + `"`
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var honeys []*honey
	var servers []*http.Server
	for i, l := range listeners {
		cfg, err := config.Parse(`
[backend]
uri = "` + origin.URL + `"
[cache]
defaultTTL = "1h"
staleWhileRevalidate = "1h"
[control]
secret = "correct-horse-battery-staple"
[cluster]
listen = "` + l.Addr().String() + `"
name = "honey` + string(rune('1'+i)) + `"
peers = [` + peers + `]
secret = "a secret for the cluster"
lease = "1s"
`)
		require.NoError(t, err)
		h := build(cfg, time.Now())
		s := &http.Server{Handler: h.cluster.Handler()}
		go s.Serve(l)
		defer s.Close()
		go h.cluster.Run(ctx)
		honeys = append(honeys, h)
		servers = append(servers, s)
	}
	send := func(h *honey, method, path string, headers ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://www.example.com"+path, nil)
		for i := 0; i+1 < len(headers); i += 2 {
			r.Header.Set(headers[i], headers[i+1])
		}
		w := httptest.NewRecorder()
		h.handler.ServeHTTP(w, r)
		return w
	}
	a, b := honeys[0], honeys[1]
	require.Eventually(t, func() bool {
		sa, sb := a.cluster.Status(), b.cluster.Status()
		return len(sa.Peers) == 1 && sa.Peers[0].Name == "honey2" && len(sb.Peers) == 1 && sb.Peers[0].Name == "honey1"
	}, 5*time.Second, 10*time.Millisecond, "the Honeys find each other")
	for _, h := range honeys {
		w := send(h, http.MethodGet, "/.well-known/honey/health")
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	}

	cache := func(path string) {
		t.Helper()
		for _, h := range honeys {
			send(h, http.MethodGet, path)
			require.Equal(t, "HIT", send(h, http.MethodGet, path).Header().Get("X-Honey-Cache"))
		}
	}

	// A purge on one is applied on the other before it returns
	cache("/hello/")
	w := send(a, "PURGE", "/hello/", "X-Honey-Secret", "correct-horse-battery-staple")
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "MISS", send(b, http.MethodGet, "/hello/").Header().Get("X-Honey-Cache"))

	// Soft purges are soft there too, and prefixes are prefixes
	cache("/blog/a-post/")
	send(b, "PURGE", "/blog/*", "X-Honey-Secret", "correct-horse-battery-staple", "X-Honey-Purge", "soft")
	assert.Equal(t, "STALE", send(a, http.MethodGet, "/blog/a-post/").Header().Get("X-Honey-Cache"))

	// Out of touch, a Honey stops trusting its cache, and says so
	cache("/again/")
	servers[1].Close()
	require.Eventually(t, func() bool {
		return send(a, http.MethodGet, "/.well-known/honey/health").Code == http.StatusServiceUnavailable
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, "STALE", send(a, http.MethodGet, "/again/").Header().Get("X-Honey-Cache"))
}

func TestHealthWithoutACluster(t *testing.T) {
	cfg, err := config.Parse("[backend]\nuri = \"http://127.0.0.1:1\"\n")
	require.NoError(t, err)
	w := httptest.NewRecorder()
	build(cfg, time.Now()).handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/.well-known/honey/health", nil))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "ok\n", w.Body.String())
}
