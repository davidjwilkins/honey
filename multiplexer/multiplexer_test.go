package multiplexer

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/davidjwilkins/honey/cache"
)

type testHandler struct {
	sync.Mutex
	count int
}

func (h *testHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.Lock()
	h.count++
	w.Header().Set("Vary", "Accept-Language")
	fmt.Fprintf(w, "Visitor count: %d.", h.count)
	h.Unlock()
}

func testServer(count int) *httptest.Server {
	return httptest.NewServer(&testHandler{
		count: count,
	})
}

func newTestValidRequest() *http.Request {
	url, err := url.Parse("https://www.insomniac.com")
	if err != nil {
		panic(err)
	}
	return &http.Request{
		Method: http.MethodGet,
		URL:    url,
		Header: http.Header{},
	}
}

func noopHandler(w http.ResponseWriter, r *http.Request) {

}

func newTestMultiplexer(withWriter bool) Multiplexer {
	cacher := cache.NewDefaultCacher()
	multiplexer := NewMultiplexer(cacher, newTestValidRequest(), (&testHandler{}).ServeHTTP)
	if withWriter {
		rec := httptest.NewRecorder()
		multiplexer.AddWriter(rec, newTestValidRequest())
	}
	return multiplexer
}

func TestAddWriter(t *testing.T) {
	multiplexer := newTestMultiplexer(false).(*multiplexer)
	if len(multiplexer.requests) != 0 {
		t.Errorf("multiplexer should save no writers when initialized")
	}
	multiplexer.AddWriter(httptest.NewRecorder(), newTestValidRequest())
	if len(multiplexer.requests) != 1 {
		t.Errorf("AddWriter should save the writer to the multiplexer")
	}
}

func TestCanCacheReturnsErrorBeforeWrite(t *testing.T) {
	multiplexer := newTestMultiplexer(false)
	_, err := multiplexer.Cacheable()
	if err == nil {
		t.Errorf("Cacheable should return error if Write has not been called")
	}
	cacher := cache.NewDefaultCacher()
	server := testServer(0)
	defer server.Close()
	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	multiplexer.Write(cacher.Standardize(resp))
	_, err = multiplexer.Cacheable()
	if err != nil {
		t.Errorf("Cacheable should not return error if Write has been called")
	}
}

func TestWriteReturnsTheSameResponseToMultipleRequests(t *testing.T) {
	server := testServer(0)
	defer server.Close()
	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	multiplexer := newTestMultiplexer(false)
	rec1 := httptest.NewRecorder()
	rec2 := httptest.NewRecorder()
	multiplexer.AddWriter(rec1, newTestValidRequest())
	multiplexer.AddWriter(rec2, newTestValidRequest())
	multiplexer.Write(cache.NewDefaultCacher().Standardize(resp))
	response1 := string(rec1.Body.Bytes())
	response2 := string(rec2.Body.Bytes())
	if response1 != "Visitor count: 1." {
		t.Errorf("Write should return the correct reponsse")
	}
	if response1 != response2 {
		t.Errorf("Write should return the same reponse to all writers")
	}
}

// TODO: Figure out how to test that it is bucketing properly
func TestWriteReturnsDifferentResponseToMultipleRequestsIfVary(t *testing.T) {
	server := testServer(5) // set it to something different since it's a different instance
	// than the multiplexer
	defer server.Close()
	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	multiplexer := newTestMultiplexer(false)
	rec1 := httptest.NewRecorder()
	rec2 := httptest.NewRecorder()
	rec3 := httptest.NewRecorder()
	req1 := newTestValidRequest()
	req2 := newTestValidRequest()
	req3 := newTestValidRequest()
	req1.Header.Set("Accept-Language", "en")
	req2.Header.Set("Accept-Language", "ru")
	req3.Header.Set("Accept-Language", "en")
	multiplexer.AddWriter(rec1, req1)
	multiplexer.AddWriter(rec2, req2)
	multiplexer.AddWriter(rec3, req3)
	resp.Request = req1
	multiplexer.Write(cache.NewDefaultCacher().Standardize(resp))
	response1 := string(rec1.Body.Bytes())
	response2 := string(rec2.Body.Bytes())
	response3 := string(rec3.Body.Bytes())

	if response1 != response3 {
		t.Errorf("Requests with identical Vary headers should get the same response. Got:\n%s\n%s", response1, response3)
	}
	if response1 == response2 {
		t.Errorf("Requests with different Vary headers should get a different response. Got:\n%s\n%s", response1, response2)
	}
}
