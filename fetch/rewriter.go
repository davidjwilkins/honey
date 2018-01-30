package fetch

import (
	"net/http"
	"net/url"
)

type rewriter struct {
	backend *url.URL
}

// SwitchBackend changes the host and scheme of a request
// to match the backend that it should be forwarder to.
// We have to do this before Rewrite is called by forward
// because otherwise we can't match the URL in the cache or
// multiplexer.  It sets the X-Forwarded-Proto header if not
// already set to indicate the protocol (HTTP or HTTPS) that a
// client used to connect, and sets the Host header to indicate
// the actual hostname requested.
func (r *rewriter) SwitchBackend(req *http.Request) {
	req.Host = req.URL.Host
	req.URL.Host = r.backend.Host
	if req.Header.Get("X-Forwarded-Proto") == "" {
		req.Header.Add("X-Forwarded-Proto", req.URL.Scheme)
	}
	req.URL.Scheme = r.backend.Scheme

}

// NewRewriter returns a forward.Rewriter
func NewRewriter(backend *url.URL) *rewriter {
	return &rewriter{backend: backend}
}
