package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadExampleConfig(t *testing.T) {
	cfg, err := Load("wordpress.toml")
	require.NoError(t, err)
	assert.Equal(t, ":8080", cfg.Listen)
	assert.Equal(t, "https://www.insomniac.com", cfg.Backend.String())
	assert.Equal(t, int64(256<<20), cfg.Cache.MaxBytes)
	assert.Equal(t, 5*time.Minute, cfg.Cache.DefaultTTL)
	require.Len(t, cfg.Routes, 1)
	for _, uri := range []string{"/feed", "/wp-admin/edit.php", "/wp-login.php", "/post?preview=true", "/post?p=1&preview=true"} {
		assert.True(t, cfg.Routes[0].Regex.MatchString(uri), uri)
	}
	for _, uri := range []string{"/", "/post", "/post?preview=false"} {
		assert.False(t, cfg.Routes[0].Regex.MatchString(uri), uri)
	}
}

func TestParseMinimalConfig(t *testing.T) {
	cfg, err := Parse(`
[backend]
uri = "http://localhost:8081"
`)
	require.NoError(t, err)
	assert.Equal(t, DefaultListen, cfg.Listen)
	assert.Equal(t, "localhost:8081", cfg.Backend.Host)
	assert.Zero(t, cfg.Cache.MaxBytes, "unset values should be left to the cache's defaults")
	assert.Zero(t, cfg.Cache.DefaultTTL)
	assert.Zero(t, cfg.Cache.MaxObjectBytes)
	assert.True(t, cfg.Cache.StaticFiles, "static files should be cached by default")
	assert.True(t, cfg.Cache.Brotli, "brotli should be on by default")
	assert.Zero(t, cfg.Cache.BrotliMinBytes)
}

func TestParseBrotli(t *testing.T) {
	cfg, err := Parse(`
[backend]
uri = "https://www.example.com"

[cache]
brotli = false
brotliMinSize = "4KB"
`)
	require.NoError(t, err)
	assert.False(t, cfg.Cache.Brotli)
	assert.Equal(t, int64(4<<10), cfg.Cache.BrotliMinBytes)
}

func TestParseStaticFilesAndObjectSize(t *testing.T) {
	cfg, err := Parse(`
[backend]
uri = "https://www.example.com"

[cache]
maxObjectSize = "2MB"
staticFiles = false
`)
	require.NoError(t, err)
	assert.Equal(t, int64(2<<20), cfg.Cache.MaxObjectBytes)
	assert.False(t, cfg.Cache.StaticFiles)
}

func TestParseRoutesAndCookies(t *testing.T) {
	cfg, err := Parse(`
[backend]
uri = "https://www.example.com"

[cache]
allowedCookies = ["site_lang_id"]
maxSize = "1gb"

[[route]]
match = "/account"
cache = false
`)
	require.NoError(t, err)
	assert.Equal(t, []string{"site_lang_id"}, cfg.Cache.AllowedCookies)
	assert.Equal(t, int64(1<<30), cfg.Cache.MaxBytes)
	require.Len(t, cfg.Routes, 1)
	assert.Equal(t, "/account", cfg.Routes[0].Match)
	assert.Nil(t, cfg.Routes[0].Regex)
}

func TestParseErrors(t *testing.T) {
	const backend = "[backend]\nuri = \"https://www.example.com\"\n"
	cases := map[string]string{
		"missing backend":     ``,
		"relative backend":    "[backend]\nuri = \"www.example.com\"\n",
		"unknown setting":     backend + "[default]\nerror = \"stale\"\n",
		"misspelled setting":  backend + "[cache]\nmax_size = \"1MB\"\n",
		"bad size":            backend + "[cache]\nmaxSize = \"lots\"\n",
		"bad object size":     backend + "[cache]\nmaxObjectSize = \"lots\"\n",
		"object over max":     backend + "[cache]\nmaxSize = \"1MB\"\nmaxObjectSize = \"2MB\"\n",
		"bad staticFiles":     backend + "[cache]\nstaticFiles = \"yes\"\n",
		"bad brotli":          backend + "[cache]\nbrotli = \"yes\"\n",
		"bad brotliMinSize":   backend + "[cache]\nbrotliMinSize = \"small\"\n",
		"bad ttl":             backend + "[cache]\ndefaultTTL = \"5\"\n",
		"route without cache": backend + "[[route]]\nmatch = \"/a\"\n",
		"route caching":       backend + "[[route]]\nmatch = \"/a\"\ncache = true\n",
		"route without match": backend + "[[route]]\ncache = false\n",
		"bad route regex":     backend + "[[route]]\nmatch = \"(\"\nregex = true\ncache = false\n",
	}
	for name, data := range cases {
		_, err := Parse(data)
		assert.Error(t, err, name)
	}
}

func TestParseSize(t *testing.T) {
	for input, expected := range map[string]int64{
		"1024":  1024,
		"10KB":  10 << 10,
		"256MB": 256 << 20,
		"2 GB":  2 << 30,
		"512b":  512,
	} {
		size, err := parseSize(input)
		assert.NoError(t, err, input)
		assert.Equal(t, expected, size, input)
	}
	for _, input := range []string{"", "MB", "0MB", "-1MB", "1TB", "1.5GB"} {
		_, err := parseSize(input)
		assert.Error(t, err, input)
	}
}
