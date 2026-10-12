package fetch

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/davidjwilkins/honey/cache"
	"github.com/davidjwilkins/honey/events"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPurgesAreLoggedTogether(t *testing.T) {
	o := versionedOrigin(nil)
	log := events.New(10)
	proxy := routeProxy(t, o, cache.Options{}, Options{Control: Control{Secret: secret}, Events: log})

	get(t, proxy.URL+"/blog/a/")
	get(t, proxy.URL+"/")
	// e.g. WordPress, when a post changes
	for _, path := range []string{"/blog/a/", "/", "/feed/", "/blog/*", "/2026/", "/2026/10/"} {
		purgeRequest(t, proxy.URL, path, path != "/")
	}
	recent := log.Recent(10)
	require.Len(t, recent, 1, "a burst of purges is one event")
	assert.Equal(t, events.Purge, recent[0].Kind)
	assert.Equal(t, 6, recent[0].Count)
	assert.Equal(t, "purged (soft and hard) /blog/a/, /, /feed/, /blog/* and 2 more · 2 responses · from 127.0.0.1", recent[0].Text)
}

func TestOtherHostsAreLogged(t *testing.T) {
	o := versionedOrigin(nil)
	log := events.New(10)
	proxy := routeProxy(t, o, cache.Options{}, Options{Hosts: []string{"www.example.com"}, Events: log})
	for _, host := range []string{"a.example", "b.example"} {
		req, err := http.NewRequest(http.MethodGet, proxy.URL+"/", nil)
		require.NoError(t, err)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
	}
	recent := log.Recent(10)
	require.Len(t, recent, 1)
	assert.Equal(t, events.Refused, recent[0].Kind)
	assert.Equal(t, 2, recent[0].Count)
	assert.Equal(t, `turned away requests for hosts other than www.example.com, most recently "b.example"`, recent[0].Text)
}

func TestCounter(t *testing.T) {
	o := noHeadersOrigin()
	backend, err := url.Parse(o.URL)
	require.NoError(t, err)
	c := cache.NewCacher(cache.Options{})
	t.Cleanup(o.Close)
	opts := Options{QueryParams: []string{"p"}}
	proxy := httptestServer(t, FetchWithOptions(c, Forwarder(c), backend, opts))
	for _, path := range []string{"/page?p=1", "/blog/one", "/blog/two"} {
		get(t, proxy+path)
	}
	count := Counter(c, backend, opts)
	for _, tc := range []struct {
		url    string
		prefix bool
		want   int
	}{
		{"/blog/", true, 2},
		{"/blog/*", false, 2},
		{"/blog/", false, 0},
		{"https://www.example.com/page?p=1&utm_source=x", false, 1},
		{"/", true, 3},
	} {
		n, err := count(tc.url, tc.prefix)
		require.NoError(t, err)
		assert.Equal(t, tc.want, n, tc.url)
	}
	_, err = count("mailto:x", false)
	assert.Error(t, err)

	n, err := Purger(c, backend, opts)("/blog/", true, false)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "Purger purges what Counter counts")
}

func httptestServer(t *testing.T, h http.Handler) string {
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s.URL
}
