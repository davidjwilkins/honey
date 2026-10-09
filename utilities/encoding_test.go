package utilities

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPreferredEncoding(t *testing.T) {
	for acceptEncoding, expected := range map[string]string{
		"":                           "",
		"identity":                   "",
		"deflate":                    "",
		"gzip":                       "gzip",
		"gzip, deflate, br":          "br",
		"br":                         "br",
		"BR;q=0.5":                   "br",
		"gzip, br;q=0":               "gzip",
		"gzip;q=0, br;q=0":           "",
		"br;q=0.5, gzip":             "gzip",
		"br;q=0.5, gzip;q=0.5":       "br",
		"*":                          "br",
		"*;q=0":                      "",
		"*, br;q=0":                  "gzip",
		"gzip;q=1.0, br; q=0.1":      "gzip",
		"br;q=0.4, identity;q=0.5":   "",
		"gzip;q=0.8, identity;q=0.5": "gzip",
		"brotli":                     "",
	} {
		assert.Equal(t, expected, PreferredEncoding(acceptEncoding, "br", "gzip"), acceptEncoding)
	}
	assert.Equal(t, "gzip", PreferredEncoding("gzip, br", "gzip"), "only available codings are chosen")
	assert.Equal(t, "", PreferredEncoding("gzip, br"), "with nothing available, the response is unencoded")
}

func TestCacheVary(t *testing.T) {
	assert.Equal(t, "", CacheVary("Accept-Encoding"))
	assert.Equal(t, "Accept-Language,Cookie", CacheVary("Accept-Language, accept-encoding, Cookie"))
	assert.Equal(t, "*", CacheVary("*"))
}

func TestIsCompressible(t *testing.T) {
	for _, contentType := range []string{"text/html; charset=utf-8", "text/css", "application/javascript", "application/json", "application/ld+json", "image/svg+xml", "application/rss+xml", "font/ttf"} {
		assert.True(t, IsCompressible(contentType), contentType)
	}
	for _, contentType := range []string{"", "image/png", "image/jpeg", "video/mp4", "font/woff2", "application/zip", "application/pdf", "nonsense"} {
		assert.False(t, IsCompressible(contentType), contentType)
	}
}
