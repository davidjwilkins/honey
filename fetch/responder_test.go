package fetch

import (
	"bytes"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/davidjwilkins/honey/cache"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
)

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

func (t *testResponse) Validate(r *http.Request) (bool, int) {
	args := t.Called(r)
	return args.Bool(0), args.Int(1)
}

func (t *testResponse) Age() string {
	args := t.Called()
	return args.String(0)
}

type testMultiplexer struct {
	mock.Mock
}

func (t *testMultiplexer) AddWriter(w http.ResponseWriter, r *http.Request) {
	t.Called(w, r)
}

func (t *testMultiplexer) Write(r cache.Response) bool {
	args := t.Called(r)
	return args.Bool(0)
}

func (t *testMultiplexer) Cacheable() (bool, error) {
	args := t.Called()
	return args.Bool(0), args.Error(1)
}
func (t *testMultiplexer) Wait() {
	t.Called()
}

type ResponderTestSuite struct {
	suite.Suite
	cacher       *testCacher
	response     *testResponse
	request      *http.Request
	writer       *httptest.ResponseRecorder
	multiplexer  *testMultiplexer
	httpResponse *http.Response
}

func newResponse() *http.Response {
	return &http.Response{
		Status:           "200 OK",
		StatusCode:       http.StatusOK,
		Proto:            "HTTP/1.0",
		ProtoMajor:       1,
		ProtoMinor:       0,
		Header:           http.Header{},
		Body:             ioutil.NopCloser(bytes.NewReader([]byte("Example Response Body"))),
		ContentLength:    int64(len([]byte("Example Response Body"))),
		TransferEncoding: nil,
		Close:            true,
		Uncompressed:     true,
		Trailer:          http.Header{},
		Request:          nil,
		TLS:              nil,
	}
}

func (suite *ResponderTestSuite) SetupTest() {
	suite.cacher = &testCacher{}
	suite.response = &testResponse{}
	suite.request = newTestValidRequest()
	suite.cacher.On("Hash", suite.request).Return("test-hash")
	suite.response.On("Body").Return([]byte("Test Response"))
	suite.writer = httptest.NewRecorder()
	suite.multiplexer = &testMultiplexer{}
	suite.multiplexer.On("Lock")
	suite.multiplexer.On("Unlock")
	suite.multiplexer.On("Done")
	suite.multiplexer.On("Wait")
	suite.response.On("Header").Return(suite.writer.Header())
	suite.httpResponse = newResponse()
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
	suite.response.On("Validate", suite.request).Return(true, http.StatusNotModified)
	suite.response.On("StatusCode").Return(http.StatusOK)
	_, responded := RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().Equal("HIT", suite.writer.Header().Get("X-Honey-Cache"), "RespondFromCache should set X-Honey-Cache: HIT")
	suite.Assert().True(responded, "RespondFromCache should return true when cache validates")
}

func (suite *ResponderTestSuite) TestRespondFromCacheMustRevalidateInvalid() {
	suite.cacher.On("Load", "test-hash").Return(suite.response, true)
	suite.request.Header.Set("Cache-Control", "must-revalidate")
	suite.response.On("Validate", suite.request).Return(false, 0)
	_, responded := RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().Equal("", suite.writer.Header().Get("X-Honey-Cache"), "RespondFromCache should not set X-Honey-Cache when cache doesn't validate")
	suite.Assert().False(responded, "RespondFromCache should return false when cache doesn't validate")
}

func (suite *ResponderTestSuite) TestRespondFromCacheProxyRevalidateValid() {
	suite.cacher.On("Load", "test-hash").Return(suite.response, true)
	suite.request.Header.Set("Cache-Control", "proxy-revalidate")
	suite.response.On("Validate", suite.request).Return(true, http.StatusNotModified)
	suite.response.On("StatusCode").Return(http.StatusOK)
	_, responded := RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().Equal("HIT", suite.writer.Header().Get("X-Honey-Cache"), "RespondFromCache should set X-Honey-Cache: HIT")
	suite.Assert().True(responded, "RespondFromCache should return true when cache validates")
}

func (suite *ResponderTestSuite) TestRespondFromCacheProxyRevalidateNoCache() {
	suite.cacher.On("Load", "test-hash").Return(suite.response, false)
	suite.request.Header.Set("Cache-Control", "proxy-revalidate")
	suite.response.On("StatusCode").Return(http.StatusOK)
	RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().True(suite.response.AssertNotCalled(suite.T(), "Validate", suite.request))
}

func (suite *ResponderTestSuite) TestRespondFromCacheProxyRevalidateInvalid() {
	suite.cacher.On("Load", "test-hash").Return(suite.response, true)
	suite.request.Header.Set("Cache-Control", "proxy-revalidate")
	suite.response.On("Validate", suite.request).Return(false, 0)
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

func (suite *ResponderTestSuite) TestFlushMultiplexerMiss() {
	multiplexers.Store("test-hash", suite.multiplexer)
	defer multiplexers.Delete("test-hash")
	suite.httpResponse.Request = suite.request
	suite.cacher.On("Standardize", suite.httpResponse).Return(suite.response)
	suite.cacher.On("Cache", "test-hash", suite.response)
	suite.multiplexer.On("Write", suite.response).Return(true)
	suite.multiplexer.On("Delete", "test-hash")
	suite.response.On("Validate", suite.request).Return(false, 0)
	var done = make(chan bool)
	FlushMultiplexer(suite.cacher, done)(suite.httpResponse)
	<-done
	suite.Assert().Equal("MISS", suite.httpResponse.Header.Get("X-Honey-Cache"))
	suite.Assert().Equal(true, suite.cacher.AssertCalled(suite.T(), "Cache", "test-hash", suite.response))
	suite.Assert().Equal(true, suite.multiplexer.AssertCalled(suite.T(), "Write", suite.response))
	suite.Assert().Equal(http.StatusOK, suite.httpResponse.StatusCode)
}

func (suite *ResponderTestSuite) TestFlushMultiplexerMissButValidates() {
	multiplexers.Store("test-hash", suite.multiplexer)
	defer multiplexers.Delete("test-hash")
	suite.httpResponse.Request = suite.request
	suite.cacher.On("Standardize", suite.httpResponse).Return(suite.response)
	suite.cacher.On("Cache", "test-hash", suite.response)
	suite.multiplexer.On("Write", suite.response).Return(true)
	suite.multiplexer.On("Delete", "test-hash")
	suite.response.On("Validate", suite.request).Return(true, http.StatusNotModified)
	var done = make(chan bool)
	FlushMultiplexer(suite.cacher, done)(suite.httpResponse)
	<-done
	suite.Assert().Equal("MISS", suite.httpResponse.Header.Get("X-Honey-Cache"))
	suite.Assert().Equal(true, suite.cacher.AssertCalled(suite.T(), "Cache", "test-hash", suite.response))
	suite.Assert().Equal(true, suite.multiplexer.AssertCalled(suite.T(), "Write", suite.response))
	suite.Assert().Equal(http.StatusNotModified, suite.httpResponse.StatusCode)
}

func (suite *ResponderTestSuite) TestFlushMultiplexerHit() {
	multiplexers.Store("test-hash", suite.multiplexer)
	defer multiplexers.Delete("test-hash")
	suite.httpResponse.Request = suite.request
	suite.request.Header.Set("If-None-Match", `"abc123"`)
	suite.response.Header().Set("Etag", `"abc123"`)
	suite.cacher.On("Standardize", suite.httpResponse).Return(suite.response)
	suite.cacher.On("Cache", "test-hash", suite.response)
	suite.multiplexer.On("Write", suite.response).Return(true)
	suite.multiplexer.On("Delete", "test-hash")
	var done = make(chan bool)
	FlushMultiplexer(suite.cacher, done)(suite.httpResponse)
	<-done
	suite.Assert().Equal("MISS", suite.httpResponse.Header.Get("X-Honey-Cache"))
	suite.Assert().Equal(true, suite.cacher.AssertCalled(suite.T(), "Cache", "test-hash", suite.response))
	suite.Assert().Equal(true, suite.multiplexer.AssertCalled(suite.T(), "Write", suite.response))
	suite.Assert().Equal(http.StatusNotModified, suite.httpResponse.StatusCode)
}

func (suite *ResponderTestSuite) TestRespondFromMultiplexerInitialRequest() {
	found := RespondFromMultiplexer("test-hash", suite.cacher, suite.writer, suite.request)
	suite.Assert().False(found, "Respond from multiplexer should return false on first request")
}

func (suite *ResponderTestSuite) TestRespondFromMultiplexerMultiplexedRequests() {
	suite.multiplexer.On("AddWriter", suite.writer, suite.request)
	suite.multiplexer.On("Wait")
	multiplexers.Store("test-hash", suite.multiplexer)
	found := RespondFromMultiplexer("test-hash", suite.cacher, suite.writer, suite.request)
	suite.Assert().True(found, "Respond from multiplexer should return true on second request")
}
