package capture

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// server is a ResponseWriter which, like net/http's, sends informational
// responses (httptest.ResponseRecorder takes them for the final status)
type server struct {
	header   http.Header
	statuses []int
	body     []byte
}

func (s *server) Header() http.Header { return s.header }
func (s *server) WriteHeader(status int) {
	s.statuses = append(s.statuses, status)
}
func (s *server) Write(p []byte) (int, error) {
	s.body = append(s.body, p...)
	return len(p), nil
}

func TestInformationalResponsesArentTheStatus(t *testing.T) {
	s := &server{header: http.Header{}}
	w := NewWriter(s)
	w.Header().Set("Link", "</a.css>; rel=preload; as=style")
	w.WriteHeader(http.StatusEarlyHints)
	w.Header().Del("Link")
	w.Header().Set("X-Honey-Cache", "NO-CACHE")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("page"))
	assert.Equal(t, []int{http.StatusEarlyHints, http.StatusOK}, s.statuses, "both are passed on")
	assert.Equal(t, http.StatusOK, w.Status(), "the 103 came before the response")
	assert.Equal(t, "NO-CACHE", w.Cache())
	assert.Equal(t, int64(4), w.Bytes())
}
