package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/config"
	"github.com/davidjwilkins/honey/serve"
	"github.com/quic-go/quic-go/http3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// selfSigned writes a certificate for www.example.com and its key to dir
func selfSigned(t *testing.T, dir string) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "www.example.com"}, DNSNames: []string{"www.example.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certFile, keyFile, pool
}

func TestServingHTTPSAndHTTP3(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "proto="+r.Header.Get("X-Forwarded-Proto"))
	}))
	defer origin.Close()
	certFile, keyFile, pool := selfSigned(t, t.TempDir())
	cfg, err := config.Parse(`
listen = "127.0.0.1:0"
hosts = ["www.example.com"]
trustedProxies = []
[backend]
uri = "` + origin.URL + `"
[tls]
listen = "127.0.0.1:0"
certFile = "` + certFile + `"
keyFile = "` + keyFile + `"
`)
	require.NoError(t, err)
	server, err := serve.New(serveOptions(cfg, newHandler(cfg)))
	require.NoError(t, err)
	require.NoError(t, server.Listen())
	go server.Serve()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	}()
	plain, secure := server.Addrs()
	_, port, _ := net.SplitHostPort(secure)

	get := func(client *http.Client, rawURL string, headers ...string) (*http.Response, string) {
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		require.NoError(t, err)
		req.Host = "www.example.com"
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		resp, err := client.Do(req)
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(body)
	}
	tlsConfig := &tls.Config{RootCAs: pool, ServerName: "www.example.com"}
	https := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig, ForceAttemptHTTP2: true}}
	h3 := &http3.Transport{TLSClientConfig: tlsConfig}
	defer h3.Close()

	resp, body := get(https, "https://"+secure+"/page", "X-Forwarded-Proto", "http")
	assert.Equal(t, "HTTP/2.0", resp.Proto)
	assert.Equal(t, "MISS", resp.Header.Get("X-Honey-Cache"))
	assert.Equal(t, "proto=https", body, "WordPress knows the visitor used HTTPS (whatever they claim)")
	assert.Contains(t, resp.Header.Get("Alt-Svc"), `h3=":`+port+`"`)

	resp, body = get(&http.Client{Transport: h3}, "https://"+secure+"/page")
	assert.Equal(t, "HTTP/3.0", resp.Proto)
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"), "HTTPS and HTTP/3 share the cache")
	assert.Equal(t, "proto=https", body)

	noRedirects := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ = get(noRedirects, "http://"+plain+"/page?x=1")
	assert.Equal(t, http.StatusMovedPermanently, resp.StatusCode)
	assert.Equal(t, "https://www.example.com:"+port+"/page?x=1", resp.Header.Get("Location"))

	req, _ := http.NewRequest(http.MethodGet, "http://"+plain+"/page", nil)
	req.Host = "evil.example"
	resp, err = noRedirects.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusMisdirectedRequest, resp.StatusCode, "other hosts are turned away, not redirected")
}
