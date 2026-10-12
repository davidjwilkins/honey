package serve

import (
	"crypto/tls"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// checkCertFilesEvery is how often certificate files are checked for
// changes
const checkCertFilesEvery = time.Minute

// certFiles serves a certificate from files, loading them again when they
// change
type certFiles struct {
	certFile, keyFile string

	mu      sync.Mutex
	cert    *tls.Certificate
	modTime time.Time
	checked time.Time
}

func newCertFiles(certFile, keyFile string) (*certFiles, error) {
	c := &certFiles{certFile: certFile, keyFile: keyFile}
	if err := c.load(time.Now()); err != nil {
		return nil, err
	}
	return c, nil
}

// modified returns when the files were last changed
func (c *certFiles) modified() (time.Time, error) {
	var latest time.Time
	for _, name := range []string{c.certFile, c.keyFile} {
		info, err := os.Stat(name)
		if err != nil {
			return time.Time{}, err
		}
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	return latest, nil
}

func (c *certFiles) load(now time.Time) error {
	modTime, err := c.modified()
	if err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		return fmt.Errorf("loading the certificate %s and key %s: %w", c.certFile, c.keyFile, err)
	}
	c.cert, c.modTime, c.checked = &cert, modTime, now
	return nil
}

// GetCertificate returns the certificate, loading it again if the files
// have changed (keeping the old one if the new one can't be loaded, e.g.
// while they're being written)
func (c *certFiles) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if now.Sub(c.checked) >= checkCertFilesEvery {
		c.checked = now
		if modTime, err := c.modified(); err == nil && !modTime.Equal(c.modTime) {
			if err := c.load(now); err != nil {
				log.Printf("keeping the old certificate: %v", err)
			} else {
				log.Printf("loaded the new certificate from %s", c.certFile)
			}
		}
	}
	return c.cert, nil
}
