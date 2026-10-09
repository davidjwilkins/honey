// Package capture records what a handler wrote in its response, for
// metrics and logging.
package capture

import (
	"bufio"
	"errors"
	"net"
	"net/http"
)

// Writer wraps a ResponseWriter, recording the status, the number of body
// bytes written, and how the cache handled the request (its X-Honey-Cache
// header).  It passes Flush, Hijack and Unwrap through, so that streamed
// responses and websockets still work.
type Writer struct {
	http.ResponseWriter
	wroteHeader bool
	status      int
	cache       string
	bytes       int64
}

// NewWriter returns a Writer which writes to w.
func NewWriter(w http.ResponseWriter) *Writer {
	return &Writer{ResponseWriter: w}
}

func (w *Writer) record(status int) {
	w.wroteHeader = true
	w.status = status
	w.cache = w.Header().Get("X-Honey-Cache")
}

// WriteHeader records the status and the X-Honey-Cache header.
func (w *Writer) WriteHeader(status int) {
	if !w.wroteHeader {
		w.record(status)
	}
	w.ResponseWriter.WriteHeader(status)
}

// Write records the number of bytes written.
func (w *Writer) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.record(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Status returns the status written, or 200 if nothing was (as net/http
// sends a 200 then).
func (w *Writer) Status() int {
	if !w.wroteHeader {
		return http.StatusOK
	}
	return w.status
}

// Cache returns the X-Honey-Cache header as it was when the status was
// written.
func (w *Writer) Cache() string {
	if !w.wroteHeader {
		return w.Header().Get("X-Honey-Cache")
	}
	return w.cache
}

// Bytes returns the number of body bytes written.
func (w *Writer) Bytes() int64 {
	return w.bytes
}

// Flush lets streamed responses (e.g. large files) be flushed through.
func (w *Writer) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack lets the proxy take over the connection, e.g. for websockets.
func (w *Writer) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("capture: the ResponseWriter can't be hijacked")
	}
	if !w.wroteHeader {
		w.record(http.StatusSwitchingProtocols)
	}
	return h.Hijack()
}

// Unwrap lets http.ResponseController reach the underlying ResponseWriter.
func (w *Writer) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
