package clientip

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// seen returns what next sees of a request from remote with headers
func seen(trusted []*net.IPNet, remote string, headers ...string) (client string, header http.Header) {
	handler := Middleware(trusted, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ip := From(r); ip != nil {
			client = ip.String()
		}
		header = r.Header
	}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remote
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Add(headers[i], headers[i+1])
	}
	handler.ServeHTTP(httptest.NewRecorder(), r)
	return client, header
}

func TestTrustedProxy(t *testing.T) {
	for _, tc := range []struct {
		name, remote, forwardedFor, want string
	}{
		{"caddy on the same host", "127.0.0.1:41234", "203.0.113.7", "203.0.113.7"},
		{"a proxy in a docker network", "172.18.0.5:41234", "203.0.113.7", "203.0.113.7"},
		{"several proxies", "127.0.0.1:41234", "203.0.113.7, 10.0.0.2", "203.0.113.7"},
		{"the visitor makes up an address", "127.0.0.1:41234", "1.2.3.4, 203.0.113.7", "203.0.113.7"},
		{"only proxies", "127.0.0.1:41234", "10.0.0.3, 10.0.0.2", "10.0.0.3"},
		{"no X-Forwarded-For", "127.0.0.1:41234", "", "127.0.0.1"},
		{"garbled", "127.0.0.1:41234", "203.0.113.7, nonsense", "127.0.0.1"},
		{"with a port", "127.0.0.1:41234", "[2001:db8::7]:5555", "2001:db8::7"},
	} {
		client, header := seen(DefaultTrusted, tc.remote, "X-Forwarded-For", tc.forwardedFor, "X-Forwarded-Proto", "https")
		assert.Equal(t, tc.want, client, tc.name)
		assert.Equal(t, tc.want, header.Get("X-Real-Ip"), tc.name)
		assert.Equal(t, "https", header.Get("X-Forwarded-Proto"), "%s: a trusted proxy's headers are kept", tc.name)
	}
}

func TestMultipleHeaderLines(t *testing.T) {
	client, _ := seen(DefaultTrusted, "127.0.0.1:1", "X-Forwarded-For", "203.0.113.7", "X-Forwarded-For", "10.0.0.2")
	assert.Equal(t, "203.0.113.7", client)
}

func TestUntrustedClient(t *testing.T) {
	client, header := seen(DefaultTrusted, "203.0.113.7:41234",
		"X-Forwarded-For", "10.0.0.1", "X-Forwarded-Proto", "https", "X-Real-Ip", "10.0.0.1",
		"Forwarded", "for=10.0.0.1;proto=https", "X-Forwarded-Host", "evil.example")
	assert.Equal(t, "203.0.113.7", client, "a visitor can't claim another address")
	for _, name := range []string{"X-Forwarded-For", "X-Forwarded-Proto", "Forwarded", "X-Forwarded-Host"} {
		assert.Empty(t, header.Get(name), "%s is removed", name)
	}
	assert.Equal(t, "203.0.113.7", header.Get("X-Real-Ip"))
}

func TestNoTrustedProxies(t *testing.T) {
	client, header := seen(nil, "127.0.0.1:41234", "X-Forwarded-For", "203.0.113.7", "X-Forwarded-Proto", "https")
	assert.Equal(t, "127.0.0.1", client)
	assert.Empty(t, header.Get("X-Forwarded-Proto"))
}

func TestFromWithoutMiddleware(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "198.51.100.4:80"
	assert.Equal(t, "198.51.100.4", From(r).String())
	r.RemoteAddr = "garbage"
	assert.Nil(t, From(r))
}
