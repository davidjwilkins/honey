package cache

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/davidjwilkins/honey/utilities"
)

// Entry describes a cached response, for browsing the cache
type Entry struct {
	// Key is its key in the cache
	Key string
	// Method and URL are what it was fetched for, and Variant is the
	// rest of its key: the values of the headers it Varies on, and the
	// allowed cookies, if any
	Method, URL, Variant string
	// Host and Path are from URL
	Host, Path string
	StatusCode int
	// ContentType is its Content-Type
	ContentType string
	// Bytes is the size of its (unencoded) body, and Brotli and Gzip the
	// size of its compressed copies, or 0 if it has none
	Bytes, Brotli, Gzip int
	// Size is roughly how much memory it takes up in the cache
	Size int64
	// Fetched is when it was fetched (less any Age it had then)
	Fetched time.Time
	// Lifetime is how long it is fresh for, from Fetched (0 if it has been
	// soft purged), and Stale how long after that it may still be served
	// while it is refreshed (per stale-while-revalidate)
	Lifetime, Stale time.Duration
	// Expired is whether it has been soft purged
	Expired bool
	// Hits is how many requests it has been served to from the cache
	Hits int64
	// RemoveAt is when it will be removed from the cache, even if it isn't
	// evicted to make room first (zero for never)
	RemoveAt time.Time
	// Fragments are the names of the shared fragments in it, if any
	Fragments []string
}

// Fresh returns whether e is fresh at now
func (e Entry) Fresh(now time.Time) bool {
	return now.Before(e.Fetched.Add(e.Lifetime))
}

// Usable returns whether e can be served at now: if it is fresh, or stale
// but within its stale-while-revalidate time
func (e Entry) Usable(now time.Time) bool {
	return now.Before(e.Fetched.Add(e.Lifetime + e.Stale))
}

// Entries returns the cached responses (but not Vary records), most
// recently used first.
func (c *defaultCacher) Entries() []Entry {
	stored := c.store.entries()
	entries := make([]Entry, 0, len(stored))
	for i := len(stored) - 1; i >= 0; i-- {
		if e, ok := describe(stored[i]); ok {
			entries = append(entries, e)
		}
	}
	return entries
}

// Entry returns the cached response with key, and its description.
func (c *defaultCacher) Entry(key string) (Entry, Response, bool) {
	for _, s := range c.store.entries() {
		if s.key == key {
			if e, ok := describe(s); ok {
				return e, s.value.(Response), true
			}
		}
	}
	return Entry{}, nil, false
}

// describe describes a store entry, if it is a cached response
func describe(s storeEntry) (Entry, bool) {
	r, ok := s.value.(*responseImpl)
	if !ok {
		return Entry{}, false
	}
	e := Entry{
		Key:         s.key,
		StatusCode:  r.statusCode,
		ContentType: r.headers.Get("Content-Type"),
		Bytes:       len(r.body),
		Brotli:      len(r.brotliBody()),
		Gzip:        len(r.gzip),
		Size:        s.size,
		Fetched:     r.now.Add(-time.Duration(r.initialAge) * time.Second),
		Expired:     r.expired.Load(),
		Hits:        r.hits.Load(),
		RemoveAt:    s.removeAt,
		Stale:       time.Duration(utilities.StaleWhileRevalidate(r.headers)) * time.Second,
	}
	if !e.Expired {
		e.Lifetime = time.Duration(utilities.FreshnessLifetime(r.headers)) * time.Second
	}
	for _, s := range r.segments {
		if s.name != "" {
			e.Fragments = append(e.Fragments, s.name)
		}
	}
	// The key is the base key (method :: URL), then any variant parts
	base := r.baseKey
	if base == "" || !strings.HasPrefix(s.key, base) {
		base, _, _ = strings.Cut(s.key, " :: cookies :: ")
	}
	e.Variant = strings.TrimSpace(strings.TrimPrefix(s.key, base))
	e.Method, e.URL, _ = strings.Cut(base, " :: ")
	// The URL the client asked for (see baseKey), not the backend's
	if u, err := url.Parse(e.URL); err == nil {
		e.Host = u.Host
		e.Path = u.RequestURI()
	} else {
		e.Path = withoutOrigin(e.URL)
	}
	return e, true
}

// RequestHeader returns the headers resp was fetched with which it Varies
// on, if any
func RequestHeader(resp Response) http.Header {
	vary := utilities.CacheVary(strings.Join(resp.Header().Values("Vary"), ","))
	varied := http.Header{}
	if vary == "" {
		return varied
	}
	for _, name := range strings.Split(vary, ",") {
		name = http.CanonicalHeaderKey(strings.TrimSpace(name))
		if values := resp.RequestHeaders().Values(name); len(values) > 0 {
			varied[name] = values
		}
	}
	return varied
}
