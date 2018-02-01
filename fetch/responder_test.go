package fetch

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/davidjwilkins/honey/cache"
	"github.com/davidjwilkins/honey/multiplexer"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
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

type testCacher struct {
	mock.Mock
}

func (t *testCacher) CanCache(r *http.Request) bool {
	args := t.Called(r)
	return args.Bool(0)
}

func (t *testCacher) Hash(r *http.Request) string {
	args := t.Called(r)
	return args.String(0)
}

func (t *testCacher) Standardize(r *http.Response) cache.Response {
	args := t.Called(r)
	return args.Get(0).(cache.Response)
}
func (t *testCacher) Cache(hash string, response cache.Response) {
	t.Called(hash, response)
}
func (t *testCacher) Load(hash string) (cache.Response, bool) {
	args := t.Called(hash)
	return args.Get(0).(cache.Response), args.Bool(1)
}

type testResponse struct {
	mock.Mock
}

func (t *testResponse) Status() string {
	args := t.Called()
	return args.String(0)
}

func (t *testResponse) StatusCode() int {
	args := t.Called()
	return args.Int(0)
}

func (t *testResponse) Header() http.Header {
	args := t.Called()
	return args.Get(0).(http.Header)
}

func (t *testResponse) Body() []byte {
	args := t.Called()
	return args.Get(0).([]byte)
}

func (t *testResponse) Validate(r *http.Request) bool {
	args := t.Called(r)
	return args.Bool(0)
}

func (t *testResponse) Age() string {
	args := t.Called()
	return args.String(0)
}

type ResponderTestSuite struct {
	suite.Suite
	cacher   *testCacher
	response *testResponse
	request  *http.Request
	writer   *httptest.ResponseRecorder
}

// Make sure that VariableThatShouldStartAtFive is set to five
// before each test
func (suite *ResponderTestSuite) SetupTest() {
	suite.cacher = &testCacher{}
	suite.response = &testResponse{}
	suite.request = newTestValidRequest()
	suite.cacher.On("Hash", suite.request).Return("test-hash")
	suite.response.On("Body").Return([]byte("Test Response"))
	suite.writer = httptest.NewRecorder()
	suite.response.On("Header").Return(suite.writer.Header())
}

// In order for 'go test' to run this suite, we need to create
// a normal test function and pass our suite to suite.Run
func TestResponderTestSuite(t *testing.T) {
	suite.Run(t, new(ResponderTestSuite))
}

func (suite *ResponderTestSuite) TestRespondFromEmptyCache() {
	suite.cacher.On("Load", "test-hash").Return(suite.response, false)
	hash, responded := RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().Equal(hash, "test-hash", "ResponeFromCache should return the requests hash")
	suite.Assert().False(responded, "RespondFromCache should return false when not responded")
}

func (suite *ResponderTestSuite) TestRespondFromPopulatedCache() {
	suite.cacher.On("Load", "test-hash").Return(suite.response, true)
	suite.response.On("StatusCode").Return(http.StatusOK)
	hash, responded := RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().Equal(hash, "test-hash", "ResponeFromCache should return the requests hash")
	suite.Assert().True(responded, "RespondFromCache should return true when responded")
}

func (suite *ResponderTestSuite) TestRespondFromCacheMustRevalidateValid() {
	suite.cacher.On("Load", "test-hash").Return(suite.response, true)
	suite.request.Header.Set("Cache-Control", "must-revalidate")
	suite.response.On("Validate", suite.request).Return(true)
	suite.response.On("StatusCode").Return(http.StatusOK)
	_, responded := RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().Equal("HIT", suite.writer.Header().Get("X-Honey-Cache"), "RespondFromCache should set X-Honey-Cache: HIT")
	suite.Assert().True(responded, "RespondFromCache should return true when cache validates")
}

func (suite *ResponderTestSuite) TestRespondFromCacheMustRevalidateInvalid() {
	suite.cacher.On("Load", "test-hash").Return(suite.response, true)
	suite.request.Header.Set("Cache-Control", "must-revalidate")
	suite.response.On("Validate", suite.request).Return(false)
	_, responded := RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().Equal("", suite.writer.Header().Get("X-Honey-Cache"), "RespondFromCache should not set X-Honey-Cache when cache doesn't validate")
	suite.Assert().False(responded, "RespondFromCache should return false when cache doesn't validate")
}

func (suite *ResponderTestSuite) TestRespondFromCacheProxyRevalidateValid() {
	suite.cacher.On("Load", "test-hash").Return(suite.response, true)
	suite.request.Header.Set("Cache-Control", "proxy-revalidate")
	suite.response.On("Validate", suite.request).Return(true)
	suite.response.On("StatusCode").Return(http.StatusOK)
	_, responded := RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().Equal("HIT", suite.writer.Header().Get("X-Honey-Cache"), "RespondFromCache should set X-Honey-Cache: HIT")
	suite.Assert().True(responded, "RespondFromCache should return true when cache validates")
}

func (suite *ResponderTestSuite) TestRespondFromCacheProxyRevalidateInvalid() {
	suite.cacher.On("Load", "test-hash").Return(suite.response, true)
	suite.request.Header.Set("Cache-Control", "proxy-revalidate")
	suite.response.On("Validate", suite.request).Return(false)
	_, responded := RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().Equal("", suite.writer.Header().Get("X-Honey-Cache"), "RespondFromCache should not set X-Honey-Cache when cache doesn't validate")
	suite.Assert().False(responded, "RespondFromCache should return false when cache doesn't validate")
}

func (suite *ResponderTestSuite) TestRespondFromCacheEtagMatch() {
	suite.cacher.On("Load", "test-hash").Return(suite.response, true)
	suite.request.Header.Set("If-None-Match", `"abc123"`)
	suite.response.Header().Set("Etag", `"abc123"`)
	_, responded := RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().Equal(http.StatusNotModified, suite.writer.Code, "RespondFromCache should return 304 Not Modified if Etags match")
	suite.Assert().True(responded, "RespondFromCache should write the response if etags match")
}

func (suite *ResponderTestSuite) TestRespondFromCacheCopiesHeaders() {
	suite.cacher.On("Load", "test-hash").Return(suite.response, true)
	suite.response.Header().Set("X-Fake-Header", "test")
	suite.response.On("StatusCode").Return(http.StatusOK)
	RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().Equal("test", suite.writer.Header().Get("X-Fake-Header"), "RespondFromCache should copy headers from remote to response")
}

func (suite *ResponderTestSuite) TestRespondFromCacheCopiesStatusCode() {
	suite.cacher.On("Load", "test-hash").Return(suite.response, true)
	suite.response.On("StatusCode").Return(http.StatusVariantAlsoNegotiates)
	RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().Equal(http.StatusVariantAlsoNegotiates, suite.writer.Code, "RespondFromCache should copy status code from remote to response")
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
	multiplexer.Wait()
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
