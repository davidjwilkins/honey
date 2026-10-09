package utilities

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAcceptsBrotli(t *testing.T) {
	for acceptEncoding, expected := range map[string]bool{
		"":                      false,
		"gzip, deflate":         false,
		"gzip, deflate, br":     true,
		"br":                    true,
		"BR;q=0.5":              true,
		"gzip, br;q=0":          false,
		"*":                     true,
		"*;q=0":                 false,
		"*, br;q=0":             false,
		"gzip;q=1.0, br; q=0.1": true,
		"identity":              false,
		"brotli":                false,
	} {
		assert.Equal(t, expected, AcceptsBrotli(acceptEncoding), acceptEncoding)
	}
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
