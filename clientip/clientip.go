// Package clientip works out the address of the visitor a request is
// from, when Honey is behind a proxy (e.g. Caddy or nginx handling TLS),
// and stops visitors from claiming to be someone else.
package clientip

import (
	"context"
	"net"
	"net/http"
	"strings"
)

// DefaultTrusted are the proxies trusted if none are configured: loopback
// and private addresses, where a proxy in front of Honey usually is (on
// the same host, or in the same Docker network or private network).
var DefaultTrusted = mustParse(
	"127.0.0.0/8", "::1/128",
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	"fc00::/7",
)

func mustParse(networks ...string) []*net.IPNet {
	var parsed []*net.IPNet
	for _, network := range networks {
		_, n, err := net.ParseCIDR(network)
		if err != nil {
			panic(err)
		}
		parsed = append(parsed, n)
	}
	return parsed
}

// forwardingHeaders are the headers a proxy uses to say who a request is
// from, and how they sent it
var forwardingHeaders = []string{
	"Forwarded",
	"X-Forwarded-For",
	"X-Forwarded-Host",
	"X-Forwarded-Port",
	"X-Forwarded-Proto",
	"X-Real-Ip",
}

type key struct{}

// Middleware works out which address each request to next is from, for
// From.  Requests from the trusted proxies are from the address they say,
// in X-Forwarded-For: the nearest one which isn't another trusted proxy.
// Their forwarding headers (e.g. X-Forwarded-Proto) are kept.  Requests
// from anywhere else are from their own address, and any forwarding
// headers they send are removed, so that they can't claim to be from
// elsewhere, or to have been sent with https.  (SetRealIP tells the backend
// the address, as requests are forwarded to it.)
func Middleware(trusted []*net.IPNet, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote := remoteIP(r)
		client := remote
		if contains(trusted, remote) {
			client = forwardedFor(r, trusted, remote)
		} else {
			for _, name := range forwardingHeaders {
				r.Header.Del(name)
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), key{}, client)))
	})
}

// SetRealIP sets r's X-Real-IP header to the address it is from (see
// From), for the backend.  It's done as requests are forwarded to the
// backend, rather than for every request, as adding a header to a request
// costs the cache time on every hit.
func SetRealIP(r *http.Request) {
	if ip := From(r); ip != nil {
		r.Header.Set("X-Real-Ip", ip.String())
	} else {
		r.Header.Del("X-Real-Ip")
	}
}

// From returns the address of the visitor r is from (see Middleware), or,
// if Middleware didn't handle it, of whatever sent it.  It returns nil if
// neither is known.
func From(r *http.Request) net.IP {
	if ip, ok := r.Context().Value(key{}).(net.IP); ok {
		return ip
	}
	return remoteIP(r)
}

func remoteIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(host)
}

// forwardedFor returns the address in r's X-Forwarded-For which isn't a
// trusted proxy, nearest to Honey - or the furthest, if they all are.  If
// the header is missing or garbled, it is remote.
func forwardedFor(r *http.Request, trusted []*net.IPNet, remote net.IP) net.IP {
	var hops []string
	for _, line := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(line, ",")...)
	}
	client := remote
	for i := len(hops) - 1; i >= 0; i-- {
		ip := parseHop(hops[i])
		if ip == nil {
			// Whoever added it is the furthest we can trust
			break
		}
		client = ip
		if !contains(trusted, ip) {
			break
		}
	}
	return client
}

// parseHop parses an X-Forwarded-For address, which may have a port
func parseHop(hop string) net.IP {
	hop = strings.TrimSpace(hop)
	if ip := net.ParseIP(hop); ip != nil {
		return ip
	}
	if host, _, err := net.SplitHostPort(hop); err == nil {
		return net.ParseIP(host)
	}
	return nil
}

func contains(networks []*net.IPNet, ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, n := range networks {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
