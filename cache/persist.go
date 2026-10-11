package cache

import (
	"bufio"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const (
	snapshotMagic   = "honey cache snapshot"
	snapshotVersion = 1
)

type snapshotHeader struct {
	Magic   string
	Version int
	Saved   time.Time
}

// snapshotEntry is one entry of the store: a response, or the Vary header
// last seen for a URL
type snapshotEntry struct {
	Key      string
	Size     int64
	RemoveAt time.Time
	Vary     string
	Response *snapshotResponse
}

type snapshotResponse struct {
	Status         string
	StatusCode     int
	Header         http.Header
	RequestHeaders http.Header
	Body           []byte
	Gzip           []byte
	Brotli         []byte
	Stored         time.Time
	InitialAge     int
	BaseKey        string
}

// SaveTo writes the cache's contents to the file at path, so that LoadFrom
// can restore them (e.g. after a restart).  The file is replaced
// atomically, so it is never left partly written, and only its owner may
// read it.  It returns the number of responses saved.
func (c *defaultCacher) SaveTo(path string) (int, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return 0, err
	}
	saved, err := c.writeSnapshot(tmp)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return 0, err
	}
	return saved, nil
}

func (c *defaultCacher) writeSnapshot(f io.Writer) (int, error) {
	w := bufio.NewWriter(f)
	enc := gob.NewEncoder(w)
	if err := enc.Encode(snapshotHeader{Magic: snapshotMagic, Version: snapshotVersion, Saved: time.Now()}); err != nil {
		return 0, err
	}
	saved := 0
	// Least recently used first, so that loading them in order restores
	// the order, and drops the least recently used if they don't all fit
	for _, entry := range c.store.entries() {
		e := snapshotEntry{Key: entry.key, Size: entry.size, RemoveAt: entry.removeAt}
		switch value := entry.value.(type) {
		case string:
			e.Vary = value
		case *responseImpl:
			e.Response = &snapshotResponse{
				Status:         value.status,
				StatusCode:     value.statusCode,
				Header:         value.headers,
				RequestHeaders: value.requestHeaders,
				Body:           value.body,
				Gzip:           value.gzip,
				Brotli:         value.brotliBody(),
				Stored:         value.now,
				InitialAge:     value.initialAge,
				BaseKey:        value.baseKey,
			}
			saved++
		default:
			continue
		}
		if err := enc.Encode(e); err != nil {
			return 0, err
		}
	}
	return saved, w.Flush()
}

// LoadFrom adds the responses saved by SaveTo in the file at path to the
// cache, apart from any which can no longer be served.  A missing file
// isn't an error (there is nothing to load).  If the file is unreadable or
// from an incompatible version, it returns an error, having loaded any
// responses it could.  It returns the number of responses loaded.
func (c *defaultCacher) LoadFrom(path string) (int, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	defer f.Close()
	dec := gob.NewDecoder(bufio.NewReader(f))
	var header snapshotHeader
	if err := dec.Decode(&header); err != nil {
		return 0, fmt.Errorf("%s isn't a cache snapshot: %w", path, err)
	}
	if header.Magic != snapshotMagic || header.Version != snapshotVersion {
		return 0, fmt.Errorf("%s isn't a version %d cache snapshot", path, snapshotVersion)
	}
	now := time.Now()
	loaded := 0
	for {
		var e snapshotEntry
		if err := dec.Decode(&e); errors.Is(err, io.EOF) {
			return loaded, nil
		} else if err != nil {
			return loaded, fmt.Errorf("reading %s: %w", path, err)
		}
		if !e.RemoveAt.IsZero() && now.After(e.RemoveAt) {
			continue
		}
		if e.Response == nil {
			c.store.set(e.Key, e.Vary, e.Size, e.RemoveAt)
			continue
		}
		c.store.set(e.Key, e.Response.restore(), e.Size, e.RemoveAt)
		loaded++
	}
}

func (s *snapshotResponse) restore() *responseImpl {
	r := &responseImpl{
		status:         s.Status,
		statusCode:     s.StatusCode,
		headers:        s.Header,
		requestHeaders: s.RequestHeaders,
		body:           s.Body,
		gzip:           s.Gzip,
		now:            s.Stored,
		initialAge:     s.InitialAge,
		baseKey:        s.BaseKey,
		cookies:        make(map[string]*http.Cookie),
	}
	if r.headers == nil {
		r.headers = http.Header{}
	}
	if r.requestHeaders == nil {
		r.requestHeaders = http.Header{}
	}
	if s.Brotli != nil {
		compressed := s.Brotli
		r.brotli.Store(&compressed)
	}
	for _, cookie := range (&http.Response{Header: r.headers}).Cookies() {
		r.cookies[cookie.Name] = cookie
	}
	return r
}
