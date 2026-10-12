package serve

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
	"net/http/httptrace"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go/http3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeCert writes a self-signed certificate for www.example.com, with
// serial, and its key, to dir, and returns their paths and the
// certificate
func writeCert(t *testing.T, dir string, serial int64) (certFile, keyFile string, cert *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: "www.example.com"},
		DNSNames:              []string{"www.example.com"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err = x509.ParseCertificate(der)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	return certFile, keyFile, cert
}

// echo responds with how the request came, after Early Hints
func echo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Link", "</style.css>; rel=preload; as=style")
	w.WriteHeader(http.StatusEarlyHints)
	w.Header().Del("Link")
	io.WriteString(w, r.Method+" "+r.Proto+" tls="+map[bool]string{true: "yes", false: "no"}[r.TLS != nil]+" "+r.Host+r.URL.RequestURI())
}

// start serves opts, and returns the server and its plain and secure
// addresses
func start(t *testing.T, opts Options) (*Server, string, string) {
	t.Helper()
	s, err := New(opts)
	require.NoError(t, err)
	require.NoError(t, s.Listen())
	served := make(chan error, 1)
	go func() { served <- s.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		assert.NoError(t, s.Shutdown(ctx))
		select {
		case err := <-served:
			assert.NoError(t, err, "Serve returns nothing once shut down")
		case <-time.After(5 * time.Second):
			t.Error("Serve didn't return")
		}
	})
	plain, secure := s.Addrs()
	return s, plain, secure
}

func tlsOptions(t *testing.T) (Options, *x509.Certificate) {
	certFile, keyFile, cert := writeCert(t, t.TempDir(), 1)
	return Options{
		Listen:      "127.0.0.1:0",
		Handler:     http.HandlerFunc(echo),
		HostAllowed: func(host string) bool { return host == "www.example.com" },
		TLS:         &TLS{Listen: "127.0.0.1:0", CertFile: certFile, KeyFile: keyFile, HTTP3: true, Redirect: true},
	}, cert
}

// clientFor returns a TLS config which trusts cert, and connects to addr
// whatever the host
func trusting(cert *x509.Certificate) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &tls.Config{RootCAs: pool, ServerName: "www.example.com"}
}

// fetch requests rawURL (on www.example.com) from addr with client, and
// returns the response, its body, and any Early Hints' Link headers
func fetch(t *testing.T, client *http.Client, method, rawURL string) (*http.Response, string, []string) {
	t.Helper()
	var hints []string
	trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
		if code == http.StatusEarlyHints {
			hints = append(hints, header.Values("Link")...)
		}
		return nil
	}}
	req, err := http.NewRequest(method, rawURL, nil)
	require.NoError(t, err)
	req.Host = "www.example.com"
	resp, err := client.Do(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body), hints
}

func TestHTTPSAndHTTP2(t *testing.T) {
	opts, cert := tlsOptions(t)
	_, _, secure := start(t, opts)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: trusting(cert), ForceAttemptHTTP2: true}}
	resp, body, hints := fetch(t, client, http.MethodGet, "https://"+secure+"/page?x=1")
	assert.Equal(t, "GET HTTP/2.0 tls=yes www.example.com/page?x=1", body)
	_, port, _ := net.SplitHostPort(secure)
	assert.Equal(t, `h3=":`+port+`"; ma=2592000`, resp.Header.Get("Alt-Svc"), "browsers are told about HTTP/3")
	assert.Equal(t, []string{"</style.css>; rel=preload; as=style"}, hints, "Early Hints work over HTTP/2")
}

func TestHTTP3(t *testing.T) {
	opts, cert := tlsOptions(t)
	_, _, secure := start(t, opts)
	transport := &http3.Transport{TLSClientConfig: trusting(cert)}
	defer transport.Close()
	resp, body, hints := fetch(t, &http.Client{Transport: transport}, http.MethodGet, "https://"+secure+"/page")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "GET HTTP/3.0 tls=yes www.example.com/page", body, "the same port, over UDP")
	assert.Equal(t, []string{"</style.css>; rel=preload; as=style"}, hints, "Early Hints work over HTTP/3")

	opts.TLS.HTTP3 = false
	_, _, secure = start(t, opts)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: trusting(cert)}}
	resp, _, _ = fetch(t, client, http.MethodGet, "https://"+secure+"/page")
	assert.Empty(t, resp.Header.Get("Alt-Svc"), "without HTTP/3, browsers aren't told about it")
}

func TestPlainHTTPIsRedirected(t *testing.T) {
	opts, _ := tlsOptions(t)
	_, plain, secure := start(t, opts)
	_, port, _ := net.SplitHostPort(secure)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, _, _ := fetch(t, client, http.MethodGet, "http://"+plain+"/page?x=1")
	assert.Equal(t, http.StatusMovedPermanently, resp.StatusCode)
	assert.Equal(t, "https://www.example.com:"+port+"/page?x=1", resp.Header.Get("Location"))

	resp, _, _ = fetch(t, client, http.MethodPost, "http://"+plain+"/wp-comments-post.php")
	assert.Equal(t, http.StatusPermanentRedirect, resp.StatusCode, "keeping the method and body")

	resp, body, _ := fetch(t, client, MethodPurge, "http://"+plain+"/page")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "PURGE HTTP/1.1 tls=no www.example.com/page", body, "purges from WordPress still work over plain HTTP")

	req, _ := http.NewRequest(http.MethodGet, "http://"+plain+"/", nil)
	req.Host = "evil.example"
	resp, err := client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "other hosts aren't redirected, but left to the proxy (which refuses them)")

	opts.TLS.Redirect = false
	_, plain, _ = start(t, opts)
	_, body, _ = fetch(t, client, http.MethodGet, "http://"+plain+"/page")
	assert.Equal(t, "GET HTTP/1.1 tls=no www.example.com/page", body, "unless redirect is off")
}

func TestRedirectToTheStandardPort(t *testing.T) {
	s := &Server{opts: Options{TLS: &TLS{Listen: ":443"}}}
	h := s.redirect(http.NotFoundHandler())
	for host, want := range map[string]string{
		"www.example.com":      "https://www.example.com/a",
		"www.example.com:80":   "https://www.example.com/a",
		"[2001:db8::1]:80":     "https://[2001:db8::1]/a",
		"WWW.Example.com:8080": "https://WWW.Example.com/a",
	} {
		req, _ := http.NewRequest(http.MethodGet, "http://x/a", nil)
		req.Host = host
		w := &recorder{header: http.Header{}}
		h.ServeHTTP(w, req)
		assert.Equal(t, want, w.header.Get("Location"), host)
	}
}

type recorder struct {
	header http.Header
	status int
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(p []byte) (int, error) { return len(p), nil }
func (r *recorder) WriteHeader(status int)      { r.status = status }

func TestCertificateFilesAreReloaded(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, _ := writeCert(t, dir, 1)
	files, err := newCertFiles(certFile, keyFile)
	require.NoError(t, err)
	got, err := files.GetCertificate(nil)
	require.NoError(t, err)
	assert.Equal(t, int64(1), got.Leaf.SerialNumber.Int64())

	// certbot renews it
	writeCert(t, dir, 2)
	later := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(certFile, later, later))
	got, _ = files.GetCertificate(nil)
	assert.Equal(t, int64(1), got.Leaf.SerialNumber.Int64(), "the files are only checked every so often")
	files.checked = time.Now().Add(-checkCertFilesEvery)
	got, _ = files.GetCertificate(nil)
	assert.Equal(t, int64(2), got.Leaf.SerialNumber.Int64(), "then the new one is loaded")

	// A half-written file keeps the old one
	require.NoError(t, os.WriteFile(certFile, []byte("-----BEGIN CERT"), 0o600))
	evenLater := later.Add(time.Minute)
	require.NoError(t, os.Chtimes(certFile, evenLater, evenLater))
	files.checked = time.Now().Add(-checkCertFilesEvery)
	got, err = files.GetCertificate(nil)
	require.NoError(t, err)
	assert.Equal(t, int64(2), got.Leaf.SerialNumber.Int64())

	_, err = newCertFiles(filepath.Join(dir, "missing.pem"), keyFile)
	assert.Error(t, err, "the files must be there at startup")
}

func TestAutomaticCertificates(t *testing.T) {
	for _, tc := range []struct {
		tls TLS
		err string
	}{
		{TLS{Auto: true, CertDir: "x"}, "need the hosts"},
		{TLS{Auto: true, Hosts: []string{"*.example.com"}, CertDir: "x"}, "wildcard"},
		{TLS{Auto: true, Hosts: []string{"www.example.com"}}, "directory"},
		{TLS{}, "needs Auto, or CertFile and KeyFile"},
	} {
		_, err := New(Options{Listen: ":0", Handler: http.NotFoundHandler(), TLS: &tc.tls})
		if assert.Error(t, err) {
			assert.Contains(t, err.Error(), tc.err)
		}
	}

	opts := Options{
		Listen:  "127.0.0.1:0",
		Handler: http.HandlerFunc(echo),
		TLS:     &TLS{Listen: "127.0.0.1:0", Auto: true, Hosts: []string{"www.example.com"}, CertDir: t.TempDir(), Redirect: true},
	}
	_, plain, secure := start(t, opts)

	// Certificates are only got for the hosts (this fails before asking
	// the CA for one)
	conn, err := tls.Dial("tcp", secure, &tls.Config{ServerName: "evil.example", InsecureSkipVerify: true})
	if err == nil {
		conn.Close()
	}
	assert.Error(t, err)

	// HTTP-01 challenges are answered (here: there's no such challenge),
	// not redirected
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _, _ := fetch(t, client, http.MethodGet, "http://"+plain+"/.well-known/acme-challenge/token")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp, _, _ = fetch(t, client, http.MethodGet, "http://"+plain+"/page")
	assert.Equal(t, http.StatusMovedPermanently, resp.StatusCode)
	assert.True(t, strings.HasPrefix(resp.Header.Get("Location"), "https://www.example.com:"))
}
