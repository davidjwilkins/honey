package utilities

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Directive looks up a directive in a Cache-Control header value.  It
// returns the directive's value (with any surrounding quotes removed),
// and whether the directive was present at all.
func Directive(cacheControl, name string) (value string, found bool) {
	for _, part := range strings.Split(cacheControl, ",") {
		key, value, _ := strings.Cut(strings.TrimSpace(part), "=")
		if strings.EqualFold(strings.TrimSpace(key), name) {
			return strings.Trim(strings.TrimSpace(value), `"`), true
		}
	}
	return "", false
}

// DirectiveSeconds returns the value of a delta-seconds directive (such as
// max-age) in a Cache-Control header value, and whether it was present and
// valid.
func DirectiveSeconds(cacheControl, name string) (seconds int, found bool) {
	value, found := Directive(cacheControl, name)
	if !found {
		return 0, false
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 0 {
		return 0, false
	}
	return seconds, true
}

// FreshnessLifetime returns how many seconds a response with headers h may
// be served from a shared cache without revalidating, following
// https://www.rfc-editor.org/rfc/rfc9111#section-4.2.1: s-maxage takes
// priority over max-age, which takes priority over Expires minus Date.
// A response with no-cache or no-store, or with no explicit expiration
// information, has a lifetime of 0.
func FreshnessLifetime(h http.Header) int {
	cc := h.Get("Cache-Control")
	// no-cache with a field-name list (e.g. no-cache="set-cookie") only
	// applies to those fields, so it doesn't stop the response being reused
	if value, found := Directive(cc, "no-cache"); found && value == "" {
		return 0
	}
	if _, found := Directive(cc, "no-store"); found {
		return 0
	}
	if seconds, found := DirectiveSeconds(cc, "s-maxage"); found {
		return seconds
	}
	if seconds, found := DirectiveSeconds(cc, "max-age"); found {
		return seconds
	}
	if h.Get("Expires") == "" {
		return 0
	}
	// An invalid Expires (e.g. "0") means already expired
	expires, err := http.ParseTime(h.Get("Expires"))
	if err != nil {
		return 0
	}
	date, err := http.ParseTime(h.Get("Date"))
	if err != nil {
		return 0
	}
	if lifetime := int(expires.Sub(date) / time.Second); lifetime > 0 {
		return lifetime
	}
	return 0
}

// StaleWhileRevalidate returns the number of seconds past its freshness
// lifetime that a response with headers h may be served while it is
// revalidated in the background (https://www.rfc-editor.org/rfc/rfc5861#section-3).
func StaleWhileRevalidate(h http.Header) int {
	seconds, _ := DirectiveSeconds(h.Get("Cache-Control"), "stale-while-revalidate")
	return seconds
}

// StaleIfError returns the number of seconds past its freshness lifetime
// that a response may be served if the backend errors
// (https://www.rfc-editor.org/rfc/rfc5861#section-4).  As an extension to
// the spec, a value of "*" means it may be served indefinitely, which is
// reported as forever.
func StaleIfError(cacheControl string) (seconds int, forever bool) {
	value, found := Directive(cacheControl, "stale-if-error")
	if !found {
		return 0, false
	}
	if strings.Trim(value, "*") == "" && value != "" {
		return 0, true
	}
	seconds, _ = DirectiveSeconds(cacheControl, "stale-if-error")
	return seconds, false
}
