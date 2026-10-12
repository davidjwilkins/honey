package optimize

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

// Keeping what's been learned across restarts: pages' main images,
// self-hosted fonts and converted images are saved to a file (next to the
// cache's, with its cache.persist setting), and loaded when Honey starts,
// so that they're not learned, fetched and converted all over again.  The
// site's stylesheets aren't: they're fetched from the backend again as
// pages need them, which is quick.

const (
	snapshotMagic   = "honey optimizer snapshot"
	snapshotVersion = 1
)

type snapshotHeader struct {
	Magic   string
	Version int
	Saved   time.Time
}

// snapshotRecord is one thing saved: a page's main images, a font (or a
// font which pages use, not yet fetched), or a converted image
type snapshotRecord struct {
	Key       string
	MainImage *mainImageSnapshot
	Font      *fontSnapshot
	Expected  bool
	Image     *imageSnapshot
}

type mainImageSnapshot struct {
	Candidates []uint64
	Known      [2]bool
	URL        [2]string
}

type fontSnapshot struct {
	Body, Br, Gzip []byte
	ContentType    string
	Fetched        time.Time
}

type imageSnapshot struct {
	Validator uint64
	Original  int
	Checked   time.Time
	Header    http.Header
	Formats   map[string]convertedSnapshot
	None      map[string]string
}

type convertedSnapshot struct {
	Body        []byte
	ContentType string
	ETag        string
	Modified    time.Time
}

// SaveTo writes what o has learned to the file at path, so that LoadFrom
// can restore it (e.g. after a restart).  The file is replaced atomically,
// and only its owner may read it.  It returns the number of things saved.
func (o *Optimizer) SaveTo(path string) (int, error) {
	if o == nil {
		return 0, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return 0, err
	}
	saved, err := o.writeSnapshot(tmp)
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

// records returns what o has learned, as records to save.  (What they
// refer to - bodies and the like - doesn't change once made, so it's
// shared rather than copied.)
func (o *Optimizer) records() []snapshotRecord {
	var records []snapshotRecord
	if s := o.images; s != nil {
		s.mu.Lock()
		for key, p := range s.pages {
			m := &mainImageSnapshot{Known: p.known, URL: p.url}
			for c := range p.candidates {
				m.Candidates = append(m.Candidates, c)
			}
			records = append(records, snapshotRecord{Key: key, MainImage: m})
		}
		s.mu.Unlock()
	}
	if s := o.fonts; s != nil {
		s.mu.Lock()
		for key, f := range s.files {
			records = append(records, snapshotRecord{Key: key, Font: &fontSnapshot{Body: f.body, Br: f.br, Gzip: f.gzip,
				ContentType: f.contentType, Fetched: f.fetched}})
		}
		for key := range s.expected {
			records = append(records, snapshotRecord{Key: key, Expected: true})
		}
		s.mu.Unlock()
	}
	if c := o.convert; c != nil {
		c.mu.Lock()
		for key, e := range c.images {
			if e.checked.IsZero() {
				// Not fetched yet
				continue
			}
			i := &imageSnapshot{Validator: e.validator, Original: e.original, Checked: e.checked, Header: e.header,
				Formats: map[string]convertedSnapshot{}, None: map[string]string{}}
			for f, img := range e.formats {
				i.Formats[f] = convertedSnapshot{Body: img.body, ContentType: img.contentType, ETag: img.etag, Modified: img.modified}
			}
			for f, why := range e.none {
				i.None[f] = why
			}
			records = append(records, snapshotRecord{Key: key, Image: i})
		}
		c.mu.Unlock()
	}
	return records
}

func (o *Optimizer) writeSnapshot(f io.Writer) (int, error) {
	w := bufio.NewWriter(f)
	enc := gob.NewEncoder(w)
	if err := enc.Encode(snapshotHeader{Magic: snapshotMagic, Version: snapshotVersion, Saved: o.opts.now()}); err != nil {
		return 0, err
	}
	records := o.records()
	for _, r := range records {
		if err := enc.Encode(r); err != nil {
			return 0, err
		}
	}
	return len(records), w.Flush()
}

// LoadFrom adds what was saved by SaveTo in the file at path to what o
// knows, for the optimizations it has on.  A missing file isn't an error.
// It returns the number of things loaded.
func (o *Optimizer) LoadFrom(path string) (int, error) {
	if o == nil {
		return 0, nil
	}
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
		return 0, fmt.Errorf("%s isn't an optimizer snapshot: %w", path, err)
	}
	if header.Magic != snapshotMagic || header.Version != snapshotVersion {
		return 0, fmt.Errorf("%s isn't a version %d optimizer snapshot", path, snapshotVersion)
	}
	loaded := 0
	for {
		var r snapshotRecord
		if err := dec.Decode(&r); errors.Is(err, io.EOF) {
			return loaded, nil
		} else if err != nil {
			return loaded, fmt.Errorf("reading %s: %w", path, err)
		}
		if o.restore(r) {
			loaded++
		}
	}
}

// restore restores a saved record, if what it's for is on, and returns
// whether it did
func (o *Optimizer) restore(r snapshotRecord) bool {
	switch {
	case r.MainImage != nil && o.images != nil:
		var candidates map[uint64]bool
		if len(r.MainImage.Candidates) > 0 {
			candidates = map[uint64]bool{}
			for _, c := range r.MainImage.Candidates {
				candidates[c] = true
			}
		}
		s := o.images
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.pages) >= s.max {
			return false
		}
		s.pages[r.Key] = &imagePage{candidates: candidates, known: r.MainImage.Known, url: r.MainImage.URL}
		return true
	case r.Font != nil && o.fonts != nil:
		o.fonts.keep(r.Key, &fontFile{body: r.Font.Body, br: r.Font.Br, gzip: r.Font.Gzip,
			contentType: r.Font.ContentType, fetched: r.Font.Fetched})
		return true
	case r.Expected && o.fonts != nil:
		o.fonts.expect(r.Key)
		return true
	case r.Image != nil && o.convert != nil:
		c := o.convert
		e := &imageEntry{validator: r.Image.Validator, original: r.Image.Original, checked: r.Image.Checked, header: r.Image.Header,
			formats: map[string]*convertedImage{}, pending: map[string]bool{}, none: map[string]string{}}
		if e.header == nil {
			e.header = http.Header{}
		}
		for f, img := range r.Image.Formats {
			e.formats[f] = &convertedImage{body: img.Body, contentType: img.ContentType, etag: img.ETag, modified: img.Modified}
		}
		for f, why := range r.Image.None {
			e.none[f] = why
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if len(c.images) >= maxImageEntries || c.bytes+e.size() > c.max {
			return false
		}
		if old := c.images[r.Key]; old != nil {
			c.bytes -= old.size()
		}
		c.images[r.Key] = e
		c.bytes += e.size()
		return true
	}
	return false
}
