package fetch

import (
	"net/http"
	"net/url"

	"github.com/davidjwilkins/honey/cache"
	"github.com/vulcand/oxy/forward"
)

// Fetch will fetch and save responses from the cache, if possible.
// If not, it will attempt to do a single request from the backend,
// and multiplex the response to all requesters.
func Fetch(c cache.Cacher, backend *url.URL) http.HandlerFunc {
	rewriter := NewRewriter(backend)
	forwarder := GetForwarder(c)
	return func(w http.ResponseWriter, r *http.Request) {
		rewriter.SwitchBackend(r)
		cacheable := c.CanCache(r)
		if cacheable {
			hash, responded := RespondFromCache(c, w, r)
			if responded {
				return
			}
			responded = RespondFromMultiplexer(hash, c, w, r)
			if responded {
				return
			}
		} else {
			w.Header().Set("X-Honey-Cache", "NO-CACHE")
		}
		forwarder.ServeHTTP(w, r)
	}
}

// GetForwarer returns a new forward.Forwarder which saves responses
// into cache.Cacher c.  It panics if it cannot create the forwarder.
func GetForwarder(c cache.Cacher) *forward.Forwarder {
	forwarder, err := forward.New(
		forward.ResponseModifier(FlushMultiplexer(c)),
	)
	if err != nil {
		panic(err)
	}
	return forwarder
}
