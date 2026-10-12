package fetch

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/davidjwilkins/honey/cache"
	"github.com/davidjwilkins/honey/clientip"
	"github.com/davidjwilkins/honey/events"
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
	// AllowIPs are the networks which trusted requests come from: the
	// visitor's address, if Honey is behind a trusted proxy (see
	// clientip.Middleware), or else the address the request came from.
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
	// The visitor's address, if Honey is behind a trusted proxy
	ip := clientip.From(r)
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
func (c Control) applyControl(cacher interface{}, w http.ResponseWriter, r *http.Request, onPurge PurgeHook, log *events.Log) (responded bool) {
	trusted := c.Trusted(r)
	r.Header.Del(c.secretHeader())
	if r.Method == MethodPurge {
		purge(cacher, trusted, w, r, onPurge, log)
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
func purge(cacher interface{}, trusted bool, w http.ResponseWriter, r *http.Request, onPurge PurgeHook, log *events.Log) {
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
	purged := purgeURL(p, *r.URL, false, soft, onPurge)
	from := "a trusted request"
	if ip := clientip.From(r); ip != nil {
		from = ip.String()
	}
	logPurge(log, from, r.URL, soft, purged)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if soft {
		fmt.Fprintf(w, "Expired %d responses\n", purged)
		return
	}
	fmt.Fprintf(w, "Purged %d responses\n", purged)
}

// purgeBurst is how close together purges from the same place are logged
// as one event: e.g. WordPress purges a dozen pages when a post changes
const purgeBurst = 5 * time.Second

// maxPurgePaths is how many of the paths purged together are listed
const maxPurgePaths = 4

// purgeEvent is what is logged about purges from one place
type purgeEvent struct {
	paths     []string
	more      int
	responses int
	soft      bool
	hard      bool
}

// logPurge logs a PURGE request for u from the address from, which purged
// count responses, combining it with any others from there just before.
func logPurge(log *events.Log, from string, u *url.URL, soft bool, count int) {
	log.Add(events.Purge, "purge from "+from, purgeBurst, func(e *events.Event) {
		p, _ := e.Data().(*purgeEvent)
		if p == nil {
			p = &purgeEvent{}
			e.SetData(p)
		}
		if len(p.paths) < maxPurgePaths {
			p.paths = append(p.paths, u.RequestURI())
		} else {
			p.more++
		}
		p.responses += count
		p.soft = p.soft || soft
		p.hard = p.hard || !soft
		mode := "purged"
		switch {
		case p.soft && p.hard:
			mode = "purged (soft and hard)"
		case p.soft:
			mode = "soft purged"
		}
		paths := strings.Join(p.paths, ", ")
		if p.more > 0 {
			paths += fmt.Sprintf(" and %d more", p.more)
		}
		e.Text = fmt.Sprintf("%s %s · %d %s · from %s", mode, paths, p.responses, plural(p.responses, "response", "responses"), from)
	})
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// PurgeHook is called after a purge (see Options.OnPurge), with the path
// (and query string) purged, whether everything under it was purged, and
// whether it was soft.
type PurgeHook func(path string, prefix, soft bool)

// purgeURL removes the cached responses for u - or, if prefix is set or
// the path ends with "*", for every URL starting with u (without the "*"
// and any query string).  With soft, it expires them instead, if the
// cache can.  It then calls onPurge, if it isn't nil.
func purgeURL(p purger, u url.URL, prefix, soft bool, onPurge PurgeHook) int {
	u, prefix = purgeTarget(u, prefix)
	var purged int
	if e, ok := p.(expirePurger); ok && soft {
		purged = e.Expire(&u, prefix)
	} else {
		purged = p.Purge(&u, prefix)
	}
	if onPurge != nil {
		onPurge(u.RequestURI(), prefix, soft)
	}
	return purged
}

// purgeTarget returns what a purge of u purges: if its path ends with "*",
// everything starting with what precedes it, and if it purges everything
// starting with a path, it ignores the query string.
func purgeTarget(u url.URL, prefix bool) (url.URL, bool) {
	if strings.HasSuffix(u.Path, "*") {
		prefix = true
		u.Path = strings.TrimSuffix(u.Path, "*")
		u.RawPath = ""
	}
	if prefix {
		u.RawQuery = ""
		u.ForceQuery = false
	}
	return u, prefix
}

// counter is implemented by Cachers which can count the responses a purge
// would remove
type counter interface {
	Count(u *url.URL, prefix bool) int
}

// Counter returns a function which counts the cached responses Purger(c,
// backend, opts) would purge for the same arguments.
func Counter(c cache.Cacher, backend *url.URL, opts Options) func(rawURL string, prefix bool) (int, error) {
	target := parsePurge(c, backend, opts)
	return func(rawURL string, prefix bool) (int, error) {
		cnt, ok := c.(counter)
		if !ok {
			return 0, fmt.Errorf("this cache can't count its responses")
		}
		r, err := target(rawURL)
		if err != nil {
			return 0, err
		}
		u, prefix := purgeTarget(*r.URL, prefix)
		return cnt.Count(&u, prefix), nil
	}
}

// parsePurge returns a function which parses a URL or path to purge (or
// count), into the trusted PURGE request for it which FetchWithOptions(c,
// ..., backend, opts) would handle.
func parsePurge(c cache.Cacher, backend *url.URL, opts Options) func(rawURL string) (*http.Request, error) {
	filterQuery := queryFilter(c, opts.QueryParams)
	return func(rawURL string) (*http.Request, error) {
		u, err := url.Parse(strings.TrimSpace(rawURL))
		if err != nil || (u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" {
			return nil, fmt.Errorf("%q isn't a URL or path", rawURL)
		}
		if u.Path == "" {
			u.Path = "/"
		} else if !strings.HasPrefix(u.Path, "/") {
			return nil, fmt.Errorf("%q isn't a URL or path: paths start with /", rawURL)
		}
		u.Fragment, u.RawFragment = "", ""
		r := &http.Request{Method: MethodPurge, URL: u, Header: http.Header{}}
		SwitchBackend(r, backend)
		r = withRoute(c, r)
		filterQuery(r)
		return r, nil
	}
}

// Purger returns a function which removes the cached responses for a URL -
// or, if prefix is set, for every URL starting with it - as a trusted
// PURGE request for it to FetchWithOptions(c, ..., backend, opts) would,
// and returns how many it removed.  With soft, it expires them instead
// (see PurgeModeHeader).  The URL may be a full URL or a path, e.g.
// "/blog/": either way, every host's responses for the path are purged.
func Purger(c cache.Cacher, backend *url.URL, opts Options) func(rawURL string, prefix, soft bool) (int, error) {
	target := parsePurge(c, backend, opts)
	return func(rawURL string, prefix, soft bool) (int, error) {
		p, ok := c.(purger)
		if !ok {
			return 0, fmt.Errorf("this cache can't be purged")
		}
		r, err := target(rawURL)
		if err != nil {
			return 0, err
		}
		return purgeURL(p, *r.URL, prefix, soft, opts.OnPurge), nil
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
