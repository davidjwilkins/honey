// Package serve runs Honey's servers for visitors: plain HTTP, and, if TLS
// is configured, HTTPS (HTTP/1.1 and HTTP/2) and HTTP/3, with certificates
// from Let's Encrypt (or another ACME CA) or from files.
package serve

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go/http3"
	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// MethodPurge is the method of purge requests, which aren't redirected to
// HTTPS, so that WordPress can send them to Honey's plain HTTP address
const MethodPurge = "PURGE"

// Options configure the servers
type Options struct {
	// Listen is the address plain HTTP is served on, e.g. ":80"
	Listen string
	// Handler handles visitors' requests
	Handler http.Handler
	// HostAllowed, if set, says which hosts plain HTTP requests are
	// redirected to HTTPS for (others go to Handler, which may refuse
	// them)
	HostAllowed func(host string) bool
	// TLS, if set, serves HTTPS too
	TLS *TLS
	// ErrorLog, if set, logs what goes wrong with connections
	ErrorLog *log.Logger
}

// TLS configures HTTPS
type TLS struct {
	// Listen is the address HTTPS (TCP) and HTTP/3 (UDP) are served on,
	// e.g. ":443"
	Listen string
	// Auto gets certificates from an ACME CA (Let's Encrypt, unless CA is
	// set) for Hosts, as they're first asked for, and renews them.  They
	// are kept in CertDir.  Email, if set, is given to the CA, e.g. for
	// notices about problems with the certificates.
	Auto    bool
	Hosts   []string
	Email   string
	CertDir string
	CA      string
	// CertFile and KeyFile are a certificate (with its chain) and its key,
	// instead of Auto.  They're loaded again when they change (e.g. when
	// certbot renews them).
	CertFile, KeyFile string
	// HTTP3 serves HTTP/3 too, and tells browsers about it (with Alt-Svc)
	HTTP3 bool
	// Redirect redirects plain HTTP requests to HTTPS, other than purges
	// (and ACME challenges)
	Redirect bool
}

// Server is Honey's servers for visitors
type Server struct {
	opts  Options
	plain *http.Server
	https *http.Server
	h3    *http3.Server

	plainListener net.Listener
	tlsListener   net.Listener
	udp           net.PacketConn
}

// timeouts are set on each server
func newHTTPServer(handler http.Handler, errorLog *log.Logger) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          errorLog,
	}
}

// New sets up the servers described by opts, without listening yet
func New(opts Options) (*Server, error) {
	s := &Server{opts: opts}
	plainHandler := opts.Handler
	if t := opts.TLS; t != nil {
		var tlsConfig *tls.Config
		var getCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)
		var challenges func(http.Handler) http.Handler
		switch {
		case t.Auto:
			m, err := autocertManager(t)
			if err != nil {
				return nil, err
			}
			// Including the TLS-ALPN-01 challenge, so that certificates can be
			// had without plain HTTP on port 80
			tlsConfig = m.TLSConfig()
			getCertificate = m.GetCertificate
			challenges = m.HTTPHandler
		case t.CertFile != "":
			files, err := newCertFiles(t.CertFile, t.KeyFile)
			if err != nil {
				return nil, err
			}
			getCertificate = files.GetCertificate
			tlsConfig = &tls.Config{GetCertificate: getCertificate, NextProtos: []string{"h2", "http/1.1"}}
		default:
			return nil, errors.New("TLS needs Auto, or CertFile and KeyFile")
		}
		tlsConfig.MinVersion = tls.VersionTLS12

		httpsHandler := opts.Handler
		if t.HTTP3 {
			s.h3 = &http3.Server{
				Handler:     opts.Handler,
				TLSConfig:   http3.ConfigureTLSConfig(&tls.Config{GetCertificate: getCertificate, MinVersion: tls.VersionTLS13}),
				IdleTimeout: 2 * time.Minute,
			}
			// Browsers start with HTTPS, and switch to HTTP/3 once they're told
			// they can
			h3 := s.h3
			httpsHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				h3.SetQUICHeaders(w.Header())
				opts.Handler.ServeHTTP(w, r)
			})
		}
		s.https = newHTTPServer(httpsHandler, opts.ErrorLog)
		s.https.TLSConfig = tlsConfig

		if t.Redirect {
			plainHandler = s.redirect(opts.Handler)
		}
		if challenges != nil {
			plainHandler = challenges(plainHandler)
		}
	}
	s.plain = newHTTPServer(plainHandler, opts.ErrorLog)
	return s, nil
}

// autocertManager returns the certificate manager for t.Auto
func autocertManager(t *TLS) (*autocert.Manager, error) {
	if len(t.Hosts) == 0 {
		return nil, errors.New("automatic certificates need the hosts to get them for")
	}
	for _, host := range t.Hosts {
		if strings.Contains(host, "*") {
			return nil, fmt.Errorf("can't get an automatic certificate for %s: wildcard certificates need a DNS challenge, so use a certificate file", host)
		}
	}
	if t.CertDir == "" {
		return nil, errors.New("automatic certificates need a directory to keep them in")
	}
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      autocert.DirCache(t.CertDir),
		HostPolicy: autocert.HostWhitelist(t.Hosts...),
		Email:      t.Email,
	}
	if t.CA != "" {
		m.Client = &acme.Client{DirectoryURL: t.CA}
	}
	return m, nil
}

// redirect returns a handler which redirects plain HTTP requests to HTTPS,
// except purges, and requests for hosts which aren't allowed, which go to
// next
func (s *Server) redirect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		if r.Method == MethodPurge || host == "" || (s.opts.HostAllowed != nil && !s.opts.HostAllowed(host)) {
			next.ServeHTTP(w, r)
			return
		}
		if port := s.tlsPort(); port != "443" && port != "" {
			host = net.JoinHostPort(host, port)
		} else if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		status := http.StatusMovedPermanently
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// So that the method and body are kept
			status = http.StatusPermanentRedirect
		}
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), status)
	})
}

// tlsPort returns the port HTTPS is served on
func (s *Server) tlsPort() string {
	if s.tlsListener != nil {
		_, port, _ := net.SplitHostPort(s.tlsListener.Addr().String())
		return port
	}
	if s.opts.TLS != nil {
		_, port, _ := net.SplitHostPort(s.opts.TLS.Listen)
		return port
	}
	return ""
}

// Listen opens the servers' ports, so that their addresses are known (e.g.
// if they were ":0")
func (s *Server) Listen() error {
	var err error
	if s.plainListener, err = net.Listen("tcp", s.opts.Listen); err != nil {
		return err
	}
	if s.https == nil {
		return nil
	}
	if s.tlsListener, err = net.Listen("tcp", s.opts.TLS.Listen); err != nil {
		s.Close()
		return err
	}
	if s.h3 != nil {
		// The same port as HTTPS, which Alt-Svc points to
		host, _, _ := net.SplitHostPort(s.opts.TLS.Listen)
		addr := net.JoinHostPort(host, s.tlsPort())
		if s.udp, err = net.ListenPacket("udp", addr); err != nil {
			s.Close()
			return fmt.Errorf("HTTP/3: %w", err)
		}
		port, _ := strconv.Atoi(s.tlsPort())
		s.h3.Port = port
	}
	return nil
}

// Addrs returns the addresses plain HTTP and HTTPS (and HTTP/3) are served
// on, once Listen has been called
func (s *Server) Addrs() (plain, secure string) {
	if s.plainListener != nil {
		plain = s.plainListener.Addr().String()
	}
	if s.tlsListener != nil {
		secure = s.tlsListener.Addr().String()
	}
	return plain, secure
}

// Serve serves requests (calling Listen first, if it hasn't been) until
// Shutdown is called, or a server fails, and returns why
func (s *Server) Serve() error {
	if s.plainListener == nil {
		if err := s.Listen(); err != nil {
			return err
		}
	}
	errs := make(chan error, 3)
	var wg sync.WaitGroup
	run := func(serve func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := serve(); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				errs <- err
			}
		}()
	}
	run(func() error { return s.plain.Serve(s.plainListener) })
	if s.https != nil {
		run(func() error { return s.https.ServeTLS(s.tlsListener, "", "") })
	}
	if s.h3 != nil {
		run(func() error { return s.h3.Serve(s.udp) })
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case err := <-errs:
		s.Close()
		return err
	case <-done:
		return nil
	}
}

// Shutdown stops the servers, letting requests in progress finish
func (s *Server) Shutdown(ctx context.Context) error {
	var errs []error
	errs = append(errs, s.plain.Shutdown(ctx))
	if s.https != nil {
		errs = append(errs, s.https.Shutdown(ctx))
	}
	if s.h3 != nil {
		errs = append(errs, s.h3.Shutdown(ctx))
	}
	if s.udp != nil {
		s.udp.Close()
	}
	return errors.Join(errs...)
}

// Close stops the servers straight away
func (s *Server) Close() {
	s.plain.Close()
	if s.plainListener != nil {
		s.plainListener.Close()
	}
	if s.https != nil {
		s.https.Close()
	}
	if s.tlsListener != nil {
		s.tlsListener.Close()
	}
	if s.h3 != nil {
		s.h3.Close()
	}
	if s.udp != nil {
		s.udp.Close()
	}
}
