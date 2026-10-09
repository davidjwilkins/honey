package cache

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
	"github.com/davidjwilkins/honey/utilities"
)

const (
	// DefaultBrotliMinBytes is the default size below which responses
	// aren't compressed, as the saving isn't worth it.
	DefaultBrotliMinBytes = 1 << 10

	// fastBrotliQuality is used when a response is first cached, as the
	// requester is waiting for it.  It is a few milliseconds for a typical
	// page, while the maximum quality is hundreds.
	fastBrotliQuality = 6
	// bestBrotliQuality is used to recompress cached responses in the
	// background.
	bestBrotliQuality = brotli.BestCompression
	// maxRecompressBytes is the largest response which is recompressed at
	// bestBrotliQuality; at a few hundred KB/s, larger ones would tie up
	// the recompressor for too long.
	maxRecompressBytes = 1 << 20
	// maxRecompressQueue bounds the number of responses waiting to be
	// recompressed, e.g. while a cold cache fills.  Responses which don't
	// fit in the queue keep their fast compression.
	maxRecompressQueue = 1000
)

func compressBrotli(body []byte, quality int) []byte {
	var buf bytes.Buffer
	w := brotli.NewWriterLevel(&buf, quality)
	w.Write(body)
	w.Close()
	return buf.Bytes()
}

// shouldCompress returns whether a response with the given status, headers
// and body should be stored brotli compressed as well as unencoded.
func shouldCompress(statusCode int, h http.Header, body []byte, minBytes int) bool {
	if statusCode != http.StatusOK || len(body) < minBytes {
		return false
	}
	if encoding := h.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return false
	}
	// https://www.rfc-editor.org/rfc/rfc9111#section-5.2.2.6
	if _, noTransform := utilities.Directive(h.Get("Cache-Control"), "no-transform"); noTransform {
		return false
	}
	return utilities.IsCompressible(h.Get("Content-Type"))
}

func brotliEtag(etag string) string {
	if etag == "" {
		return ""
	}
	return strings.TrimSuffix(etag, `"`) + `-br"`
}

// Negotiate returns the headers and body to send to the client which made
// request r: the brotli compressed body, if there is one and the client
// accepts it, or otherwise the unencoded body.  Range requests always get
// the unencoded body, which the range applies to.  The returned headers
// must not be modified.
func Negotiate(resp Response, r *http.Request) (http.Header, []byte) {
	impl, ok := resp.(*responseImpl)
	if !ok {
		return resp.Header(), resp.Body()
	}
	compressed := impl.brotliBody()
	if compressed == nil || r.Header.Get("Range") != "" || !utilities.AcceptsBrotli(r.Header.Get("Accept-Encoding")) {
		return resp.Header(), resp.Body()
	}
	h := resp.Header().Clone()
	h.Set("Content-Encoding", "br")
	h.Set("Content-Length", strconv.Itoa(len(compressed)))
	if etag := brotliEtag(h.Get("Etag")); etag != "" {
		h.Set("Etag", etag)
	}
	return h, compressed
}

// recompressor recompresses cached responses at bestBrotliQuality, one at
// a time, in the background.  Its goroutine only runs while there is work.
type recompressor struct {
	mu      sync.Mutex
	queue   []*responseImpl
	running bool
	pending sync.WaitGroup
}

func (q *recompressor) add(r *responseImpl) {
	if r.brotliBody() == nil || len(r.body) > maxRecompressBytes {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.queue) >= maxRecompressQueue {
		return
	}
	q.queue = append(q.queue, r)
	q.pending.Add(1)
	if !q.running {
		q.running = true
		go q.run()
	}
}

func (q *recompressor) run() {
	for {
		q.mu.Lock()
		if len(q.queue) == 0 {
			q.running = false
			q.mu.Unlock()
			return
		}
		r := q.queue[0]
		q.queue = q.queue[1:]
		q.mu.Unlock()

		if best := compressBrotli(r.body, bestBrotliQuality); len(best) < len(r.brotliBody()) {
			r.brotli.Store(&best)
		}
		q.pending.Done()
	}
}

// wait blocks until every queued response has been recompressed
func (q *recompressor) wait() {
	q.pending.Wait()
}
