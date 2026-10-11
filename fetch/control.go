package fetch

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/davidjwilkins/honey/cache"
	"github.com/davidjwilkins/honey/utilities"
)

// DefaultSecretHeader is the request header which carries Control.Secret,
// if no other header is configured.
const DefaultSecretHeader = "X-Honey-Secret"

// MethodPurge is the request method which removes responses from the cache.
const MethodPurge = "PURGE"

// PurgeModeHeader is the request header which makes a PURGE request soft
// (with the value "soft"): the responses are expired rather than removed,
// so that they can be served once more while they are refreshed (see
// cache's Expire).
const PurgeModeHeader = "X-Honey-Purge"

// Control says which requests are trusted to control the cache: to purge
// responses from it, and to make it fetch a fresh response with
// Cache-Control: no-cache or Pragma: no-cache.  Requests from anyone else
// can't purge, and their no-cache is ignored, so that they can't use it to
// send every request to the backend.  The zero Control trusts nobody.
type Control struct {
	// AllowIPs are the networks which trusted requests come from.  Note
	// that if Honey is behind another proxy on the same host, every
	// request will come from that proxy's address.
	AllowIPs []*net.IPNet
	// Secret, if set, makes requests trusted if they send it in the
	// SecretHeader header (whichever address they come from).
	Secret string
	// SecretHeader is the header Secret is sent in.  Defaults to
	// DefaultSecretHeader.
	SecretHeader string
}

// Trusted returns whether r is allowed to control the cache.
func (c Control) Trusted(r *http.Request) bool {
	if c.Secret != "" {
		if sent := r.Header.Get(c.secretHeader()); sent != "" &&
			subtle.ConstantTimeCompare([]byte(sent), []byte(c.Secret)) == 1 {
			return true
		}
	}
	if len(c.AllowIPs) == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, network := range c.AllowIPs {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func (c Control) secretHeader() string {
	if c.SecretHeader != "" {
		return c.SecretHeader
	}
	return DefaultSecretHeader
}

// ParseIPs parses IP addresses (e.g. "127.0.0.1") and networks in CIDR
// notation (e.g. "10.0.0.0/8") for Control.AllowIPs.
func ParseIPs(addresses []string) ([]*net.IPNet, error) {
	var networks []*net.IPNet
	for _, address := range addresses {
		if !strings.Contains(address, "/") {
			ip := net.ParseIP(address)
			if ip == nil {
				return nil, fmt.Errorf("%q is not an IP address or network", address)
			}
			bits := 8 * net.IPv6len
			if ip.To4() != nil {
				ip, bits = ip.To4(), 8*net.IPv4len
			}
			networks = append(networks, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, network, err := net.ParseCIDR(address)
		if err != nil {
			return nil, fmt.Errorf("%q is not an IP address or network", address)
		}
		networks = append(networks, network)
	}
	return networks, nil
}

// purger is implemented by Cachers which can remove responses
type purger interface {
	Purge(u *url.URL, prefix bool) int
}

// expirePurger is implemented by Cachers which can also soft purge
type expirePurger interface {
	purger
	Expire(u *url.URL, prefix bool) int
}

// applyControl handles the parts of r which only trusted requests may use.
// It responds to PURGE requests itself, returning true if it did.  For
// untrusted requests, it removes no-cache, so that they are served from the
// cache like any other request.  It always removes the secret header, so
// that it never reaches the backend.
func (c Control) applyControl(cacher interface{}, w http.ResponseWriter, r *http.Request) (responded bool) {
	trusted := c.Trusted(r)
	r.Header.Del(c.secretHeader())
	if r.Method == MethodPurge {
		purge(cacher, trusted, w, r)
		return true
	}
	if !trusted {
		if strings.EqualFold(r.Header.Get("Pragma"), "no-cache") {
			r.Header.Del("Pragma")
		}
		if cc := r.Header.Get("Cache-Control"); cc != "" {
			if _, noCache := utilities.Directive(cc, "no-cache"); noCache {
				if cc = withoutDirective(cc, "no-cache"); cc == "" {
					r.Header.Del("Cache-Control")
				} else {
					r.Header.Set("Cache-Control", cc)
				}
			}
		}
	}
	return false
}

// purge removes the cached responses for r's URL - or, if the path ends
// with "*", for every URL starting with what precedes it.
func purge(cacher interface{}, trusted bool, w http.ResponseWriter, r *http.Request) {
	if !trusted {
		http.Error(w, "Purging the cache isn't allowed", http.StatusForbidden)
		return
	}
	p, ok := cacher.(purger)
	if !ok {
		http.Error(w, "This cache can't be purged", http.StatusNotImplemented)
		return
	}
	soft := strings.EqualFold(r.Header.Get(PurgeModeHeader), "soft")
	purged := purgeURL(p, *r.URL, false, soft)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if soft {
		fmt.Fprintf(w, "Expired %d responses\n", purged)
		return
	}
	fmt.Fprintf(w, "Purged %d responses\n", purged)
}

// purgeURL removes the cached responses for u - or, if prefix is set or
// the path ends with "*", for every URL starting with u (without the "*"
// and any query string).  With soft, it expires them instead, if the
// cache can.
func purgeURL(p purger, u url.URL, prefix, soft bool) int {
	if strings.HasSuffix(u.Path, "*") {
		prefix = true
		u.Path = strings.TrimSuffix(u.Path, "*")
		u.RawPath = ""
	}
	if prefix {
		u.RawQuery = ""
		u.ForceQuery = false
	}
	if e, ok := p.(expirePurger); ok && soft {
		return e.Expire(&u, prefix)
	}
	return p.Purge(&u, prefix)
}

// Purger returns a function which removes the cached responses for a URL -
// or, if prefix is set, for every URL starting with it - as a trusted
// PURGE request for it to FetchWithOptions(c, ..., backend, opts) would,
// and returns how many it removed.  With soft, it expires them instead
// (see PurgeModeHeader).  The URL may be a full URL or a path, e.g.
// "/blog/": either way, every host's responses for the path are purged.
func Purger(c cache.Cacher, backend *url.URL, opts Options) func(rawURL string, prefix, soft bool) (int, error) {
	filterQuery := queryFilter(c, opts.QueryParams)
	return func(rawURL string, prefix, soft bool) (int, error) {
		p, ok := c.(purger)
		if !ok {
			return 0, fmt.Errorf("this cache can't be purged")
		}
		u, err := url.Parse(strings.TrimSpace(rawURL))
		if err != nil || (u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" {
			return 0, fmt.Errorf("%q isn't a URL or path", rawURL)
		}
		if u.Path == "" {
			u.Path = "/"
		} else if !strings.HasPrefix(u.Path, "/") {
			return 0, fmt.Errorf("%q isn't a URL or path: paths start with /", rawURL)
		}
		u.Fragment, u.RawFragment = "", ""
		r := &http.Request{Method: MethodPurge, URL: u, Header: http.Header{}}
		SwitchBackend(r, backend)
		r = withRoute(c, r)
		filterQuery(r)
		return purgeURL(p, *r.URL, prefix, soft), nil
	}
}

func withoutDirective(cc, name string) string {
	var kept []string
	for _, part := range strings.Split(cc, ",") {
		directive, _, _ := strings.Cut(strings.TrimSpace(part), "=")
		if part = strings.TrimSpace(part); part != "" && !strings.EqualFold(strings.TrimSpace(directive), name) {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, ", ")
}
