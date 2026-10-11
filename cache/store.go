package cache

import (
	"container/list"
	"sync"
	"time"
)

// store is a size-bounded, least-recently-used map.  Each entry may also
// have a deadline after which it is dropped, so that memory is reclaimed
// for responses which can no longer be served (not even as stale).
type store struct {
	mu       sync.Mutex
	maxBytes int64
	bytes    int64
	order    *list.List // front is most recently used
	items    map[string]*list.Element
}

type storeEntry struct {
	key      string
	value    interface{}
	size     int64
	removeAt time.Time // zero means never, other than by LRU eviction
}

func newStore(maxBytes int64) *store {
	return &store{
		maxBytes: maxBytes,
		order:    list.New(),
		items:    make(map[string]*list.Element),
	}
}

// get returns the value for key, unless it is missing or past its deadline.
func (s *store) get(key string, now time.Time) (interface{}, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.items[key]
	if !ok {
		return nil, false
	}
	entry := el.Value.(*storeEntry)
	if !entry.removeAt.IsZero() && now.After(entry.removeAt) {
		s.remove(el)
		return nil, false
	}
	s.order.MoveToFront(el)
	return entry.value, true
}

// set stores value under key, evicting the least recently used entries
// until the store is within its size limit.  Values larger than the
// whole store are not stored.
func (s *store) set(key string, value interface{}, size int64, removeAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.items[key]; ok {
		s.remove(el)
	}
	if size > s.maxBytes {
		return
	}
	s.items[key] = s.order.PushFront(&storeEntry{key: key, value: value, size: size, removeAt: removeAt})
	s.bytes += size
	for s.bytes > s.maxBytes {
		s.remove(s.order.Back())
	}
}

func (s *store) delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.items[key]; ok {
		s.remove(el)
	}
}

// deleteMatching removes every entry whose key match returns true for,
// and returns how many it removed.
func (s *store) deleteMatching(match func(key string) bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for key, el := range s.items {
		if match(key) {
			s.remove(el)
			removed++
		}
	}
	return removed
}

// entries returns a copy of the entries, least recently used first.
func (s *store) entries() []storeEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := make([]storeEntry, 0, len(s.items))
	for el := s.order.Back(); el != nil; el = el.Prev() {
		entries = append(entries, *el.Value.(*storeEntry))
	}
	return entries
}

// stats returns the number of entries and their total size.
func (s *store) stats() (entries int, bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items), s.bytes
}

// remove must be called with s.mu held
func (s *store) remove(el *list.Element) {
	entry := s.order.Remove(el).(*storeEntry)
	delete(s.items, entry.key)
	s.bytes -= entry.size
	if removable, ok := entry.value.(interface{ markRemoved() }); ok {
		removable.markRemoved()
	}
}
