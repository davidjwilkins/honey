package fetch

import (
	"net"
	"strings"
)

// HostAllowed returns whether host (a Host header, which may have a port)
// is one of hosts: either exactly (ignoring case), or, for a host starting
// with "*.", any subdomain of the rest (e.g. "*.example.com" allows
// "fr.example.com", but not "example.com").
func HostAllowed(host string, hosts []string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return false
	}
	for _, allowed := range hosts {
		allowed = strings.ToLower(allowed)
		if parent, wildcard := strings.CutPrefix(allowed, "*."); wildcard {
			if strings.HasSuffix(host, "."+parent) {
				return true
			}
		} else if host == allowed {
			return true
		}
	}
	return false
}
