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
	require.Len(t, cfg.Routes, 2)
	assert.Equal(t, "/wp-content/uploads/", cfg.Routes[1].Match)
	assert.Equal(t, 7*24*time.Hour, cfg.Routes[1].DefaultTTL)
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
	assert.True(t, cfg.Cache.Gzip, "gzip should be on by default")
	assert.Zero(t, cfg.Cache.CompressMinBytes)
}

func TestParseBrotli(t *testing.T) {
	cfg, err := Parse(`
[backend]
uri = "https://www.example.com"

[cache]
brotli = false
gzip = false
compressMinSize = "4KB"
`)
	require.NoError(t, err)
	assert.False(t, cfg.Cache.Brotli)
	assert.False(t, cfg.Cache.Gzip)
	assert.Equal(t, int64(4<<10), cfg.Cache.CompressMinBytes)
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
		"bad gzip":            backend + "[cache]\ngzip = \"yes\"\n",
		"old brotliMinSize":   backend + "[cache]\nbrotliMinSize = \"1KB\"\n",
		"bad compressMinSize": backend + "[cache]\ncompressMinSize = \"small\"\n",
		"bad ttl":             backend + "[cache]\ndefaultTTL = \"5\"\n",
		"route doing nothing": backend + "[[route]]\nmatch = \"/a\"\n",
		"route caching":       backend + "[[route]]\nmatch = \"/a\"\ncache = true\n",
		"no-cache overrides":  backend + "[[route]]\nmatch = \"/a\"\ncache = false\ndefaultTTL = \"1h\"\n",
		"bad route ttl":       backend + "[[route]]\nmatch = \"/a\"\ndefaultTTL = \"soon\"\n",
		"bad route stale":     backend + "[[route]]\nmatch = \"/a\"\nstaleIfError = \"-1h\"\n",
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

func TestParseControl(t *testing.T) {
	cfg, err := Parse(`
[backend]
uri = "https://www.example.com"

[control]
allowIPs = ["127.0.0.1", "10.0.0.0/8"]
secret = "0123456789abcdef"
secretHeader = "X-Purge-Key"
`)
	require.NoError(t, err)
	require.Len(t, cfg.Control.AllowIPs, 2)
	assert.Equal(t, "127.0.0.1/32", cfg.Control.AllowIPs[0].String())
	assert.Equal(t, "10.0.0.0/8", cfg.Control.AllowIPs[1].String())
	assert.Equal(t, "0123456789abcdef", cfg.Control.Secret)
	assert.Equal(t, "X-Purge-Key", cfg.Control.SecretHeader)

	cfg, err = Parse("[backend]\nuri = \"https://www.example.com\"\n")
	require.NoError(t, err)
	assert.Empty(t, cfg.Control.AllowIPs, "nobody is trusted by default")
	assert.Empty(t, cfg.Control.Secret)
}

func TestParseControlErrors(t *testing.T) {
	const backend = "[backend]\nuri = \"https://www.example.com\"\n[control]\n"
	for name, data := range map[string]string{
		"bad ip":                backend + "allowIPs = [\"localhost\"]\n",
		"short secret":          backend + "secret = \"hunter2\"\n",
		"header without secret": backend + "secretHeader = \"X-Key\"\n",
		"unknown setting":       backend + "password = \"0123456789abcdef\"\n",
	} {
		_, err := Parse(data)
		assert.Error(t, err, name)
	}
}

func TestParseTimeouts(t *testing.T) {
	cfg, err := Parse("[backend]\nuri = \"https://www.example.com\"\n")
	require.NoError(t, err)
	assert.Equal(t, DefaultBackendTimeout, cfg.BackendTimeout)
	assert.Zero(t, cfg.Cache.StaleIfError, "serving stale content is off unless configured")

	cfg, err = Parse("[backend]\nuri = \"https://www.example.com\"\ntimeout = \"5s\"\n[cache]\nstaleIfError = \"1h\"\n")
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, cfg.BackendTimeout)
	assert.Equal(t, time.Hour, cfg.Cache.StaleIfError)

	cfg, err = Parse("[backend]\nuri = \"https://www.example.com\"\ntimeout = \"0\"\n")
	require.NoError(t, err)
	assert.Zero(t, cfg.BackendTimeout, "0 means no limit")

	for _, data := range []string{
		"[backend]\nuri = \"https://www.example.com\"\ntimeout = \"5\"\n",
		"[backend]\nuri = \"https://www.example.com\"\ntimeout = \"10ms\"\n",
		"[backend]\nuri = \"https://www.example.com\"\ntimeout = \"-1s\"\n",
		"[backend]\nuri = \"https://www.example.com\"\n[cache]\nstaleIfError = \"forever\"\n",
	} {
		_, err := Parse(data)
		assert.Error(t, err, data)
	}
}

func TestParseMetrics(t *testing.T) {
	cfg, err := Parse("[backend]\nuri = \"https://www.example.com\"\n")
	require.NoError(t, err)
	assert.Empty(t, cfg.MetricsListen, "metrics are off by default")
	cfg, err = Parse("[backend]\nuri = \"https://www.example.com\"\n[metrics]\nlisten = \"127.0.0.1:9090\"\n")
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:9090", cfg.MetricsListen)
}

func TestParseQueryParams(t *testing.T) {
	cfg, err := Parse("[backend]\nuri = \"https://www.example.com\"\n")
	require.NoError(t, err)
	assert.Nil(t, cfg.Cache.QueryParams, "every parameter is kept unless configured")

	cfg, err = Parse("[backend]\nuri = \"https://www.example.com\"\n[cache]\nqueryParams = [\"p\", \"ver\"]\n")
	require.NoError(t, err)
	assert.Equal(t, []string{"p", "ver"}, cfg.Cache.QueryParams)

	cfg, err = Parse("[backend]\nuri = \"https://www.example.com\"\n[cache]\nqueryParams = []\n")
	require.NoError(t, err)
	assert.NotNil(t, cfg.Cache.QueryParams, "an empty list removes every parameter, which isn't the same as not setting it")
	assert.Empty(t, cfg.Cache.QueryParams)
}

func TestParseLog(t *testing.T) {
	cfg, err := Parse("[backend]\nuri = \"https://www.example.com\"\n")
	require.NoError(t, err)
	assert.False(t, cfg.AccessLog)
	assert.Equal(t, "text", cfg.AccessLogFormat)

	cfg, err = Parse("[backend]\nuri = \"https://www.example.com\"\n[log]\naccess = true\nformat = \"json\"\n")
	require.NoError(t, err)
	assert.True(t, cfg.AccessLog)
	assert.Equal(t, "json", cfg.AccessLogFormat)

	_, err = Parse("[backend]\nuri = \"https://www.example.com\"\n[log]\nformat = \"xml\"\n")
	assert.Error(t, err)
}

func TestParseStallTimeout(t *testing.T) {
	cfg, err := Parse("[backend]\nuri = \"https://www.example.com\"\n")
	require.NoError(t, err)
	assert.Equal(t, DefaultBackendTimeout, cfg.BackendStallTimeout)
	cfg, err = Parse("[backend]\nuri = \"https://www.example.com\"\nstallTimeout = \"10s\"\n")
	require.NoError(t, err)
	assert.Equal(t, 10*time.Second, cfg.BackendStallTimeout)
	cfg, err = Parse("[backend]\nuri = \"https://www.example.com\"\nstallTimeout = \"0\"\n")
	require.NoError(t, err)
	assert.Zero(t, cfg.BackendStallTimeout)
	_, err = Parse("[backend]\nuri = \"https://www.example.com\"\nstallTimeout = \"5ms\"\n")
	assert.Error(t, err)
}

func TestParseRouteSettings(t *testing.T) {
	cfg, err := Parse(`
[backend]
uri = "https://www.example.com"

[[route]]
match = "/wp-content/"
defaultTTL = "7d"

[[route]]
match = "/checkout"
staleIfError = "0"

[[route]]
match = "^/search"
regex = true
queryParams = ["s", "paged"]

[[route]]
match = "/wp-admin"
cache = false
`)
	require.NoError(t, err)
	require.Len(t, cfg.Routes, 4)
	assert.Equal(t, 7*24*time.Hour, cfg.Routes[0].DefaultTTL)
	assert.True(t, cfg.Routes[0].Cache)
	assert.Nil(t, cfg.Routes[0].StaleIfError)
	require.NotNil(t, cfg.Routes[1].StaleIfError)
	assert.Zero(t, *cfg.Routes[1].StaleIfError, "0 turns serving stale off for the route")
	require.NotNil(t, cfg.Routes[2].QueryParams)
	assert.Equal(t, []string{"s", "paged"}, *cfg.Routes[2].QueryParams)
	assert.NotNil(t, cfg.Routes[2].Regex)
	assert.False(t, cfg.Routes[3].Cache)
}

func TestParseDuration(t *testing.T) {
	for input, expected := range map[string]time.Duration{"90s": 90 * time.Second, "1h30m": 90 * time.Minute, "7d": 7 * 24 * time.Hour, "0": 0, "0d": 0} {
		d, err := parseDuration(input)
		assert.NoError(t, err, input)
		assert.Equal(t, expected, d, input)
	}
	for _, input := range []string{"", "d", "1.5d", "-1d", "7days", "soon"} {
		_, err := parseDuration(input)
		assert.Error(t, err, input)
	}
}

func TestParsePersist(t *testing.T) {
	cfg, err := Parse("[backend]\nuri = \"https://www.example.com\"\n")
	require.NoError(t, err)
	assert.Empty(t, cfg.Cache.Persist, "the cache isn't saved unless configured")
	assert.Zero(t, cfg.Cache.PersistInterval)

	cfg, err = Parse("[backend]\nuri = \"https://www.example.com\"\n[cache]\npersist = \"/var/lib/honey/cache\"\n")
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/honey/cache", cfg.Cache.Persist)
	assert.Equal(t, DefaultPersistInterval, cfg.Cache.PersistInterval)

	cfg, err = Parse("[backend]\nuri = \"https://www.example.com\"\n[cache]\npersist = \"/c\"\npersistInterval = \"0\"\n")
	require.NoError(t, err)
	assert.Zero(t, cfg.Cache.PersistInterval, "0 means only saving on shutdown")

	for _, data := range []string{
		"[backend]\nuri = \"https://www.example.com\"\n[cache]\npersistInterval = \"5m\"\n",
		"[backend]\nuri = \"https://www.example.com\"\n[cache]\npersist = \"/c\"\npersistInterval = \"10ms\"\n",
	} {
		_, err := Parse(data)
		assert.Error(t, err, data)
	}
}
