// Package accesslog logs each request Honey serves.
package accesslog

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/davidjwilkins/honey/internal/capture"
)

// Format is how log lines are written.
type Format string

const (
	// Text writes key=value pairs.
	Text Format = "text"
	// JSON writes a JSON object per line.
	JSON Format = "json"
)

// New returns a logger which writes to out in format.
func New(out io.Writer, format Format) *slog.Logger {
	if format == JSON {
		return slog.New(slog.NewJSONHandler(out, nil))
	}
	return slog.New(slog.NewTextHandler(out, nil))
}

// Middleware logs a line to logger for each request next serves: its
// method and URI (as the client sent them), status, response body size,
// duration, how the cache handled it (its X-Honey-Cache header), and the
// client's address (and X-Forwarded-For, if it was sent).  Request headers
// aren't logged, so secrets sent in them aren't either.
func Middleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Recorded before next runs, as Honey rewrites the request
		method, uri := r.Method, r.RequestURI
		if uri == "" {
			uri = r.URL.RequestURI()
		}
		start := time.Now()
		rw := capture.NewWriter(w)
		next.ServeHTTP(rw, r)

		attrs := []slog.Attr{
			slog.String("method", method),
			slog.String("uri", uri),
			slog.Int("status", rw.Status()),
			slog.Int64("bytes", rw.Bytes()),
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
			slog.String("cache", rw.Cache()),
			slog.String("remote", r.RemoteAddr),
		}
		if forwardedFor := r.Header.Get("X-Forwarded-For"); forwardedFor != "" {
			attrs = append(attrs, slog.String("forwarded_for", forwardedFor))
		}
		logger.LogAttrs(r.Context(), slog.LevelInfo, "request", attrs...)
	})
}
