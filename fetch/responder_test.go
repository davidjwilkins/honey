package fetch

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/davidjwilkins/honey/cache"
	"github.com/davidjwilkins/honey/multiplexer"
)

type testHandler struct {
	sync.Mutex
	count int
}

func (h *testHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var count int
	h.Lock()
	h.count++
	count = h.count
	h.Unlock()

	fmt.Fprintf(w, "Visitor count: %d.", count)
}

func testServer() *httptest.Server {
	return httptest.NewServer(&testHandler{})
}

func TestRespondFromEmptyCache(t *testing.T) {
	cacher := cache.NewDefaultCacher()
	request := newTestValidRequest()
	rw := httptest.NewRecorder()
	actualHash := cacher.Hash(request)
	hash, responded := RespondFromCache(cacher, rw, request)
	if hash != actualHash {
		t.Errorf("ResponeFromCache should return hash %s, got %s", actualHash, hash)
	}
	if responded {
		t.Error("RespondFromCache should not return true when nothing has been cached")
	}
}

func TestRespondFromPopulatedCache(t *testing.T) {
	server := testServer()
	cacher := cache.NewDefaultCacher()
	request := newTestValidRequest()
	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	response := cacher.Standardize(resp)
	actualHash := cacher.Hash(request)
	cacher.Cache(actualHash, response)

	rw := httptest.NewRecorder()
	hash, responded := RespondFromCache(cacher, rw, request)
	if hash != actualHash {
		t.Errorf("ResponeFromCache should return hash %s, got %s", actualHash, hash)
	}
	if !responded {
		t.Error("RespondFromCache should return true when able to respond from cache")
	}
	if rw.Header().Get("X-Honey-Cache") != "HIT" {
		t.Error("RespondFromCache should set X-Honey-Cache: HIT when responding from cache")
	}
	request.Header.Set("Pragma", "no-cache")
	_, responded = RespondFromCache(cacher, rw, request)
	if responded {
		t.Error("RespondFromCache should not respond when Pragma: no-cache")
	}
	request.Header.Del("Pragma")
	request.Header.Set("Cache-Control", "no-cache")
	_, responded = RespondFromCache(cacher, rw, request)
	if responded {
		t.Error("RespondFromCache should not respond when Cache-Control: no-cache")
	}
	request.Header.Set("Cache-Control", "test,no-cache")
	_, responded = RespondFromCache(cacher, rw, request)
	if responded {
		t.Error("RespondFromCache should not respond when Cache-Control contains no-cache")
	}
}

func TestFlushMultiplexer(t *testing.T) {
	server := testServer()
	cacher := cache.NewDefaultCacher()
	request := newTestValidRequest()
	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Request = request
	actualResponse := cacher.Standardize(resp)
	actualHash := cacher.Hash(request)

	rw1 := httptest.NewRecorder()
	rw2 := httptest.NewRecorder()
	rw3 := httptest.NewRecorder()
	multiplexer := multiplexer.NewMultiplexer(cacher, newTestValidRequest())
	multiplexer.AddWriter(rw1, newTestValidRequest())
	multiplexer.AddWriter(rw2, newTestValidRequest())
	multiplexer.AddWriter(rw3, newTestValidRequest())
	multiplexers.Store(actualHash, multiplexer)
	defer multiplexers.Delete(actualHash)
	FlushMultiplexer(cacher)(resp)
	if rw1.Body.String() != rw2.Body.String() || rw2.Body.String() != rw3.Body.String() {
		t.Error("Multiplexer should return the same body to all ResponseWriters")
	}
	if rw1.Code != rw2.Code || rw2.Code != rw3.Code {
		t.Error("Multiplexer should return the same status code to all ResponseWriters")
	}
	if rw1.Header().Get("X-Honey-Cache") != rw2.Header().Get("X-Honey-Cache") ||
		rw3.Header().Get("X-Honey-Cache") != rw2.Header().Get("X-Honey-Cache") ||
		rw1.Header().Get("X-Honey-Cache") != "MISS (MULTIPLEXED)" {
		t.Error("Multiplexer should set X-Honey-Cache: MISS (MULTIPLEXED) header")
	}
	cachedResponse, found := cacher.Load(actualHash)
	if !found {
		t.Error("Flushing multiplexer should add response to cache")
	}
	if string(cachedResponse.Body()) != string(actualResponse.Body()) {
		t.Error("Cached Response should be the actual Response")
	}
	if _, found := multiplexers.Load(actualHash); found {
		t.Error("Multiplexer should be removed after flushing")
	}
}

func TestRespondFromMultiplexer(t *testing.T) {
	server := testServer()
	cacher := cache.NewDefaultCacher()
	request := newTestValidRequest()
	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Request = request
	//actualResponse := cacher.Standardize(resp)
	actualHash := cacher.Hash(request)

	rw1 := httptest.NewRecorder()
	rw2 := httptest.NewRecorder()

	responded := RespondFromMultiplexer(actualHash, cacher, rw1, request)
	if responded {
		t.Error("RespondFromMultiplexer should return false when unable to respond from the multiplexer")
	}
	cb := func() {
		FlushMultiplexer(cacher)(resp)
	}
	onwait = &cb
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		responded = RespondFromMultiplexer(actualHash, cacher, rw2, request)
		wg.Done()
	}()
	wg.Wait()
	if !responded {
		t.Error("RespondFromMultiplexer should return true when able to respond from the multiplexer")
	}
}
