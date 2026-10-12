package fetch

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"hash/crc32"
	"html"
	"net/http"
	"sync"
)

// esiPage is a cached page which uses ESI, parsed once (see cache.Derived)
// rather than for every visitor: the text around its includes, with its
// <!--esi ... --> comments revealed and <esi:remove> elements removed, and
// its includes.  The text is also compressed once, so that the page can be
// sent gzipped with only its includes compressed for each visitor (see
// gzip).
type esiPage struct {
	// parts are the text around the includes: one more than includes
	parts    [][]byte
	includes []map[string]string

	deflateOnce sync.Once
	// deflated are parts, each compressed into a raw deflate stream which
	// ends with a sync flush, so that they can be joined to others
	deflated [][]byte
}

// parseESIPage parses a page which uses ESI (see esiPage)
func parseESIPage(body []byte) *esiPage {
	body = esiComment.ReplaceAll(body, []byte("$1"))
	body = esiRemove.ReplaceAll(body, nil)
	matches := esiInclude.FindAllSubmatchIndex(body, -1)
	if len(matches) > maxIncludes {
		// The rest are left as they are
		matches = matches[:maxIncludes]
	}
	p := &esiPage{}
	last := 0
	for _, m := range matches {
		attrs := map[string]string{}
		for _, a := range esiAttr.FindAllSubmatch(body[m[2]:m[3]], -1) {
			// e.g. src="/?p=1&amp;honey-esi=comments"
			attrs[string(a[1])] = html.UnescapeString(string(a[2]) + string(a[3]))
		}
		p.parts = append(p.parts, body[last:m[0]])
		p.includes = append(p.includes, attrs)
		last = m[1]
	}
	p.parts = append(p.parts, body[last:])
	return p
}

// fetch fetches the page's includes for visitor (see include), at once,
// and returns their content: nil for any which failed
func (p *esiPage) fetch(visitor *http.Request, handler http.Handler, secretHeader string) [][]byte {
	contents := make([][]byte, len(p.includes))
	var wg sync.WaitGroup
	for i, attrs := range p.includes {
		wg.Add(1)
		go func(i int, attrs map[string]string) {
			defer wg.Done()
			for _, src := range []string{attrs["src"], attrs["alt"]} {
				if content, ok := include(src, visitor, handler, secretHeader); ok {
					contents[i] = content
					return
				}
			}
			// onerror="continue" or not, a failed include leaves a gap,
			// rather than failing the whole page
		}(i, attrs)
	}
	wg.Wait()
	return contents
}

// size returns the length of the page with contents in its includes
func (p *esiPage) size(contents [][]byte) int {
	n := 0
	for _, part := range p.parts {
		n += len(part)
	}
	for _, c := range contents {
		n += len(c)
	}
	return n
}

// plain returns the page with contents in its includes
func (p *esiPage) plain(contents [][]byte) []byte {
	b := make([]byte, 0, p.size(contents))
	for i, part := range p.parts {
		b = append(b, part...)
		if i < len(contents) {
			b = append(b, contents[i]...)
		}
	}
	return b
}

var (
	// gzipHeader is a gzip member's header: deflate, with no name or time
	gzipHeader = []byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 255}
	// finalBlock is an empty, final deflate block, which ends the stream
	finalBlock = func() []byte {
		var b bytes.Buffer
		w, _ := flate.NewWriter(&b, flate.BestSpeed)
		w.Close()
		return b.Bytes()
	}()
	deflaters = sync.Pool{New: func() any {
		w, _ := flate.NewWriter(nil, flate.DefaultCompression)
		return w
	}}
)

// deflate appends data to b as a raw deflate stream ending with a sync
// flush (so that more can follow), compressed with no references to what
// came before
func deflate(b *bytes.Buffer, data []byte, level int) {
	if len(data) == 0 {
		return
	}
	if level != flate.DefaultCompression {
		w, _ := flate.NewWriter(b, level)
		w.Write(data)
		w.Flush()
		return
	}
	w := deflaters.Get().(*flate.Writer)
	w.Reset(b)
	w.Write(data)
	w.Flush()
	deflaters.Put(w)
}

// gzip returns the page with contents in its includes, gzipped: its parts
// compressed once (see deflated), and its includes for each visitor.  A
// deflate stream can be made of streams which each end with a sync flush,
// so they're joined, then ended with an empty final block, and the gzip
// trailer's checksum and length.
func (p *esiPage) gzip(contents [][]byte) []byte {
	p.deflateOnce.Do(func() {
		p.deflated = make([][]byte, len(p.parts))
		for i, part := range p.parts {
			var b bytes.Buffer
			deflate(&b, part, flate.BestCompression)
			p.deflated[i] = b.Bytes()
		}
	})
	var b bytes.Buffer
	b.Write(gzipHeader)
	crc := uint32(0)
	for i, part := range p.parts {
		b.Write(p.deflated[i])
		crc = crc32.Update(crc, crc32.IEEETable, part)
		if i < len(contents) {
			deflate(&b, contents[i], flate.DefaultCompression)
			crc = crc32.Update(crc, crc32.IEEETable, contents[i])
		}
	}
	b.Write(finalBlock)
	var trailer [8]byte
	binary.LittleEndian.PutUint32(trailer[:4], crc)
	binary.LittleEndian.PutUint32(trailer[4:], uint32(p.size(contents)))
	b.Write(trailer[:])
	return b.Bytes()
}
