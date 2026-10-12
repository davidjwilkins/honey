package capture

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInformationalResponsesArentTheStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	w := NewWriter(rec)
	w.Header().Set("Link", "</a.css>; rel=preload; as=style")
	w.WriteHeader(http.StatusEarlyHints)
	w.Header().Del("Link")
	w.Header().Set("X-Honey-Cache", "NO-CACHE")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("page"))
	assert.Equal(t, http.StatusOK, w.Status(), "the 103 came before the response")
	assert.Equal(t, "NO-CACHE", w.Cache())
	assert.Equal(t, int64(4), w.Bytes())
}
