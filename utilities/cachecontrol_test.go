package utilities

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDirective(t *testing.T) {
	value, found := Directive(`public, Max-Age="60", no-cache`, "max-age")
	assert.True(t, found)
	assert.Equal(t, "60", value)
	_, found = Directive("public, s-maxage=60", "max-age")
	assert.False(t, found)
	value, found = Directive("public, no-cache", "no-cache")
	assert.True(t, found)
	assert.Equal(t, "", value)
}

func TestFreshnessLifetime(t *testing.T) {
	header := func(pairs ...string) http.Header {
		h := http.Header{}
		for i := 0; i+1 < len(pairs); i += 2 {
			h.Set(pairs[i], pairs[i+1])
		}
		return h
	}
	assert.Equal(t, 0, FreshnessLifetime(header()))
	assert.Equal(t, 60, FreshnessLifetime(header("Cache-Control", "max-age=60")))
	assert.Equal(t, 120, FreshnessLifetime(header("Cache-Control", "max-age=60, s-maxage=120")))
	assert.Equal(t, 0, FreshnessLifetime(header("Cache-Control", "max-age=60, no-cache")))
	assert.Equal(t, 60, FreshnessLifetime(header("Cache-Control", `max-age=60, no-cache="set-cookie"`)))
	assert.Equal(t, 0, FreshnessLifetime(header("Cache-Control", "max-age=60, no-store")))
	assert.Equal(t, 300, FreshnessLifetime(header(
		"Date", "Mon, 05 Oct 2026 10:00:00 GMT",
		"Expires", "Mon, 05 Oct 2026 10:05:00 GMT",
	)))
	assert.Equal(t, 0, FreshnessLifetime(header(
		"Date", "Mon, 05 Oct 2026 10:00:00 GMT",
		"Expires", "0",
	)))
}

func TestStaleDirectives(t *testing.T) {
	h := http.Header{}
	h.Set("Cache-Control", "max-age=60, stale-while-revalidate=30, stale-if-error=600")
	assert.Equal(t, 30, StaleWhileRevalidate(h))
	seconds, forever := StaleIfError(h.Get("Cache-Control"))
	assert.Equal(t, 600, seconds)
	assert.False(t, forever)
	_, forever = StaleIfError("stale-if-error=*")
	assert.True(t, forever)
	seconds, forever = StaleIfError("max-age=60")
	assert.Equal(t, 0, seconds)
	assert.False(t, forever)
}
