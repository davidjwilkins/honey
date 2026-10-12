package fetch

import (
	"io"
	"net/http"
	"testing"

	"github.com/davidjwilkins/honey/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostAllowed(t *testing.T) {
	hosts := []string{"www.example.com", "Example.com", "*.example.org"}
	for host, allowed := range map[string]bool{
		"www.example.com":      true,
		"WWW.EXAMPLE.COM:8080": true,
		"www.example.com.":     true,
		"example.com":          true,
		"fr.example.org":       true,
		"a.b.example.org:443":  true,
		"example.org":          false,
		"evilexample.org":      false,
		"cdn.example.com":      false,
		"www.example.com.evil": false,
		"127.0.0.1":            false,
		"":                     false,
	} {
		assert.Equal(t, allowed, HostAllowed(host, hosts), host)
	}
	assert.False(t, HostAllowed("www.example.com", nil))
}

func TestOnlyAllowedHostsAreServed(t *testing.T) {
	o := versionedOrigin(nil)
	proxy := routeProxy(t, o, cache.Options{}, Options{Hosts: []string{"www.example.com"}, Control: Control{Secret: secret}})
	getHost := func(host string) (int, string) {
		req, err := http.NewRequest(http.MethodGet, proxy.URL+"/", nil)
		require.NoError(t, err)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	status, body := getHost("www.example.com")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "v1", body)
	for _, host := range []string{"random-1234.example.com", "127.0.0.1"} {
		status, _ := getHost(host)
		assert.Equal(t, http.StatusMisdirectedRequest, status, host)
	}
	assert.Equal(t, 1, o.Hits(), "requests for other hosts don't reach the backend")
	assert.Equal(t, "Purged 1 responses\n", purgeRequest(t, proxy.URL, "/", false), "but purges are allowed from anywhere trusted")
}
