package fetch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"

	"github.com/davidjwilkins/honey/cache"
	"github.com/davidjwilkins/honey/clientip"
	"github.com/davidjwilkins/honey/events"
)

// FragmentPurgeHeader, on a PURGE request, names the shared fragments to
// purge (see cache/fragments.go), rather than its URL: a name, a name
// ending with "*" for every fragment whose name starts with what precedes
// it, or several, separated by commas.  The request's path is ignored.
const FragmentPurgeHeader = "X-Honey-Fragment"

// fragmentPurger is implemented by Cachers which keep shared fragments
type fragmentPurger interface {
	PurgeFragments(name string) (int, []string)
}

// FragmentPurger returns a function which purges the shared fragments
// called name (as FragmentPurgeHeader would), and returns how many there
// were.  So that every page showing them gets their new content, it
// refreshes one page which shows each of them, by expiring it and
// requesting it from handler - the caching proxy (FetchWithOptions'
// handler, without metrics or an access log) - in the background.  It
// returns how many pages it refreshes.
func FragmentPurger(c cache.Cacher, handler http.Handler) func(name string) (fragments, pages int, err error) {
	return func(name string) (int, int, error) {
		p, ok := c.(fragmentPurger)
		if !ok {
			return 0, 0, fmt.Errorf("this cache doesn't keep shared fragments")
		}
		purged := 0
		var sources []string
		for _, n := range strings.Split(name, ",") {
			if n = strings.TrimSpace(n); n == "" {
				continue
			}
			count, pages := p.PurgeFragments(n)
			purged += count
			sources = append(sources, pages...)
		}
		refreshed := 0
		seen := map[string]bool{}
		for _, source := range sources {
			if seen[source] {
				continue
			}
			seen[source] = true
			if refreshPage(c, handler, source) {
				refreshed++
			}
		}
		return purged, refreshed, nil
	}
}

// refreshPage expires the cached page at rawURL, and requests it from
// handler in the background, so that it is fetched from the backend again
func refreshPage(c cache.Cacher, handler http.Handler, rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	if e, ok := c.(expirePurger); ok {
		e.Expire(&url.URL{Path: u.Path, RawPath: u.RawPath, RawQuery: u.RawQuery}, false)
	}
	go func() {
		// As a server receives it, from a visitor with no cookies
		r := &http.Request{
			Method:     http.MethodGet,
			URL:        &url.URL{Path: u.Path, RawPath: u.RawPath, RawQuery: u.RawQuery},
			RequestURI: u.RequestURI(),
			Host:       u.Host,
			Header:     http.Header{"X-Forwarded-Proto": {u.Scheme}, "User-Agent": {"Honey fragment refresh"}},
			Proto:      "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			RemoteAddr: "127.0.0.1:0",
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		handler.ServeHTTP(httptest.NewRecorder(), r.WithContext(ctx))
	}()
	return true
}

// handleFragmentPurge handles a PURGE request with the
// FragmentPurgeHeader, if it is trusted
func handleFragmentPurge(w http.ResponseWriter, r *http.Request, opts Options, purge func(string) (int, int, error)) {
	if !opts.Control.Trusted(r) {
		http.Error(w, "Purging the cache isn't allowed", http.StatusForbidden)
		return
	}
	name := r.Header.Get(FragmentPurgeHeader)
	fragments, pages, err := purge(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotImplemented)
		return
	}
	if opts.OnFragmentPurge != nil {
		opts.OnFragmentPurge(name)
	}
	from := "a trusted request"
	if ip := clientip.From(r); ip != nil {
		from = ip.String()
	}
	opts.Events.Add(events.Purge, "fragments from "+from, purgeBurst, func(e *events.Event) {
		e.Text = fmt.Sprintf("purged the shared fragment %s · %d %s · refreshing %d %s · from %s",
			name, fragments, plural(fragments, "fragment", "fragments"), pages, plural(pages, "page", "pages"), from)
	})
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "Purged %d fragments, refreshing %d pages\n", fragments, pages)
}
