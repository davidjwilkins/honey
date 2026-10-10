package fetch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// errStalled is returned when reading a backend response's body stalls.
var errStalled error = stallError{}

type stallError struct{}

func (stallError) Error() string   { return "backend response stalled" }
func (stallError) Timeout() bool   { return true }
func (stallError) Temporary() bool { return true }

// StallTimeout returns a RoundTripper which sends requests with next, and
// fails a response body read (with an error whose Timeout method returns
// true) if the backend sends nothing for timeout while it is being waited
// for.  Time spent waiting for the reader - e.g. a slow client - doesn't
// count.  A timeout of 0 means no limit.
func StallTimeout(next http.RoundTripper, timeout time.Duration) http.RoundTripper {
	if timeout <= 0 {
		return next
	}
	return roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		ctx, cancel := context.WithCancel(r.Context())
		resp, err := next.RoundTrip(r.WithContext(ctx))
		if err != nil {
			cancel()
			return resp, err
		}
		resp.Body = &stallBody{body: resp.Body, timeout: timeout, cancel: cancel}
		return resp, nil
	})
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type stallBody struct {
	body    io.ReadCloser
	timeout time.Duration
	cancel  context.CancelFunc

	mu      sync.Mutex
	stalled bool
}

func (b *stallBody) Read(p []byte) (int, error) {
	timer := time.AfterFunc(b.timeout, func() {
		b.mu.Lock()
		b.stalled = true
		b.mu.Unlock()
		// Cancelling the request's context makes the blocked read return
		b.cancel()
	})
	n, err := b.body.Read(p)
	timer.Stop()
	if err != nil && !errors.Is(err, io.EOF) {
		b.mu.Lock()
		stalled := b.stalled
		b.mu.Unlock()
		if stalled {
			return n, errStalled
		}
	}
	return n, err
}

func (b *stallBody) Close() error {
	b.cancel()
	return b.body.Close()
}
