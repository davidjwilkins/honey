package fetch

import (
	"bytes"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/davidjwilkins/honey/cache"
	"github.com/davidjwilkins/honey/singleflight"
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
func (t *testCacher) Load(hash string, req *http.Request) (cache.Response, bool) {
	args := t.Called(hash, req)
	return args.Get(0).(cache.Response), args.Bool(1)
}

func (t *testCacher) AllowedCookies() []string {
	args := t.Called()
	return args.Get(0).([]string)
}

type testResponse struct {
	mock.Mock
	age string
}

func (t *testResponse) Status() string {
	args := t.Called()
	return args.String(0)
}

func (t *testResponse) RequestHeaders() http.Header {
	args := t.Called()
	return args.Get(0).(http.Header)
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
	if t.age == "" {
		return "0"
	}
	return t.age
}

func (t *testResponse) Cookie(name string) (*http.Cookie, error) {
	args := t.Called()
	return args.Get(0).(*http.Cookie), args.Error(1)
}

type ResponderTestSuite struct {
	suite.Suite
	cacher       *testCacher
	response     *testResponse
	request      *http.Request
	writer       *httptest.ResponseRecorder
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
	suite.response.On("Header").Return(suite.writer.Header())
	suite.response.Header().Set("Cache-Control", "max-age=60")
	suite.httpResponse = newResponse()
}

// In order for 'go test' to run this suite, we need to create
// a normal test function and pass our suite to suite.Run
func TestResponderTestSuite(t *testing.T) {
	suite.Run(t, new(ResponderTestSuite))
}

func (suite *ResponderTestSuite) TestRespondFromEmptyCache() {
	suite.cacher.On("Load", "test-hash", suite.request).Return(suite.response, false)
	hash, responded, _ := RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().Equal(hash, "test-hash", "ResponeFromCache should return the requests hash")
	suite.Assert().False(responded, "RespondFromCache should return false when not responded")
}

func (suite *ResponderTestSuite) TestRespondFromPopulatedCache() {
	suite.cacher.On("Load", "test-hash", suite.request).Return(suite.response, true)
	suite.response.On("StatusCode").Return(http.StatusOK)
	hash, responded, _ := RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().Equal(hash, "test-hash", "ResponeFromCache should return the requests hash")
	suite.Assert().True(responded, "RespondFromCache should return true when responded")
}

func (suite *ResponderTestSuite) TestRespondFromCacheMustRevalidateValid() {
	suite.cacher.On("Load", "test-hash", suite.request).Return(suite.response, true)
	suite.request.Header.Set("Cache-Control", "must-revalidate")
	suite.response.On("Validate", suite.request).Return(true, http.StatusNotModified)
	suite.response.On("StatusCode").Return(http.StatusOK)
	_, responded, _ := RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().Equal("HIT", suite.writer.Header().Get("X-Honey-Cache"), "RespondFromCache should set X-Honey-Cache: HIT")
	suite.Assert().True(responded, "RespondFromCache should return true when cache validates")
}

func (suite *ResponderTestSuite) TestRespondFromCacheMustRevalidateInvalid() {
	suite.cacher.On("Load", "test-hash", suite.request).Return(suite.response, true)
	suite.request.Header.Set("Cache-Control", "must-revalidate")
	suite.response.On("Validate", suite.request).Return(false, 0)
	_, responded, revalidate := RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().Equal("", suite.writer.Header().Get("X-Honey-Cache"), "RespondFromCache should not set X-Honey-Cache when cache doesn't validate")
	suite.Assert().False(responded, "RespondFromCache should return false when cache doesn't validate")
	suite.Assert().False(revalidate, "RespondFromCache should return false when not a stale-while-refresh")
}

func (suite *ResponderTestSuite) TestRespondFromCacheExpired() {
	suite.cacher.On("Load", "test-hash", suite.request).Return(suite.response, true)
	suite.response.age = "61"
	_, responded, revalidate := RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().False(responded, "RespondFromCache should not respond with an expired response")
	suite.Assert().False(revalidate, "RespondFromCache should not revalidate without stale-while-revalidate")
}

func (suite *ResponderTestSuite) TestRespondFromCacheExpiredByExpiresHeader() {
	suite.cacher.On("Load", "test-hash", suite.request).Return(suite.response, true)
	suite.response.Header().Del("Cache-Control")
	suite.response.Header().Set("Date", "Mon, 05 Oct 2026 10:00:00 GMT")
	suite.response.Header().Set("Expires", "Mon, 05 Oct 2026 10:01:00 GMT")
	suite.response.age = "61"
	_, responded, _ := RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().False(responded, "RespondFromCache should not respond with a response past its Expires")
}

func (suite *ResponderTestSuite) TestRespondFromCacheStaleWhileRevalidate() {
	suite.cacher.On("Load", "test-hash", suite.request).Return(suite.response, true)
	suite.response.Header().Set("Cache-Control", "max-age=60, stale-while-revalidate=30")
	suite.response.age = "80"
	suite.response.On("StatusCode").Return(http.StatusOK)
	_, responded, revalidate := RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().Equal("STALE", suite.writer.Header().Get("X-Honey-Cache"), "RespondFromCache should set X-Honey-Cache: STALE when serving stale content")
	suite.Assert().Equal("80", suite.writer.Header().Get("Age"), "RespondFromCache should set the Age header")
	suite.Assert().True(responded, "RespondFromCache should return true when serving stale")
	suite.Assert().True(revalidate, "RespondFromCache should return revalidate:true when serving stale")
}

func (suite *ResponderTestSuite) TestRespondFromCacheStaleWhileRevalidateExpired() {
	suite.cacher.On("Load", "test-hash", suite.request).Return(suite.response, true)
	suite.response.Header().Set("Cache-Control", "max-age=60, stale-while-revalidate=30")
	suite.response.age = "95"
	_, responded, revalidate := RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().False(responded, "RespondFromCache should not serve stale content past the stale-while-revalidate window")
	suite.Assert().False(revalidate)
}

func (suite *ResponderTestSuite) TestRespondFromCacheProxyRevalidateValid() {
	suite.cacher.On("Load", "test-hash", suite.request).Return(suite.response, true)
	suite.request.Header.Set("Cache-Control", "proxy-revalidate")
	suite.response.On("Validate", suite.request).Return(true, http.StatusNotModified)
	suite.response.On("StatusCode").Return(http.StatusOK)
	_, responded, _ := RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().Equal("HIT", suite.writer.Header().Get("X-Honey-Cache"), "RespondFromCache should set X-Honey-Cache: HIT")
	suite.Assert().True(responded, "RespondFromCache should return true when cache validates")
}

func (suite *ResponderTestSuite) TestRespondFromCacheProxyRevalidateNoCache() {
	suite.cacher.On("Load", "test-hash", suite.request).Return(suite.response, false)
	suite.request.Header.Set("Cache-Control", "proxy-revalidate")
	suite.response.On("StatusCode").Return(http.StatusOK)
	RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().True(suite.response.AssertNotCalled(suite.T(), "Validate", suite.request))
}

func (suite *ResponderTestSuite) TestRespondFromCacheProxyRevalidateInvalid() {
	suite.cacher.On("Load", "test-hash", suite.request).Return(suite.response, true)
	suite.request.Header.Set("Cache-Control", "proxy-revalidate")
	suite.response.On("Validate", suite.request).Return(false, 0)
	_, responded, _ := RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().Equal("", suite.writer.Header().Get("X-Honey-Cache"), "RespondFromCache should not set X-Honey-Cache when cache doesn't validate")
	suite.Assert().False(responded, "RespondFromCache should return false when cache doesn't validate")
}

func (suite *ResponderTestSuite) TestRespondFromCacheEtagMatch() {
	suite.cacher.On("Load", "test-hash", suite.request).Return(suite.response, true)
	suite.request.Header.Set("If-None-Match", `"abc123"`)
	suite.response.Header().Set("Etag", `"abc123"`)
	_, responded, _ := RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().Equal(http.StatusNotModified, suite.writer.Code, "RespondFromCache should return 304 Not Modified if Etags match")
	suite.Assert().True(responded, "RespondFromCache should write the response if etags match")
}

func (suite *ResponderTestSuite) TestRespondFromCacheCopiesHeaders() {
	suite.cacher.On("Load", "test-hash", suite.request).Return(suite.response, true)
	suite.response.Header().Set("X-Fake-Header", "test")
	suite.response.On("StatusCode").Return(http.StatusOK)
	RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().Equal("test", suite.writer.Header().Get("X-Fake-Header"), "RespondFromCache should copy headers from remote to response")
}

func (suite *ResponderTestSuite) TestRespondFromCacheCopiesStatusCode() {
	suite.cacher.On("Load", "test-hash", suite.request).Return(suite.response, true)
	suite.response.On("StatusCode").Return(http.StatusVariantAlsoNegotiates)
	RespondFromCache(suite.cacher, suite.writer, suite.request)
	suite.Assert().Equal(http.StatusVariantAlsoNegotiates, suite.writer.Code, "RespondFromCache should copy status code from remote to response")
}

// leadFlight makes suite.httpResponse the response to a request which
// leads a Flight, as it would be when it reaches FlushSingleflight
func (suite *ResponderTestSuite) leadFlight() *singleflight.Flight {
	f, leader := flights.Join("test-hash")
	suite.Require().True(leader)
	suite.T().Cleanup(func() { flights.Finish("test-hash", f, singleflight.Result{Retry: true}) })
	suite.httpResponse.Request = forBackend(suite.request, "test-hash", f)
	return f
}

func (suite *ResponderTestSuite) TestFlushSingleflightMiss() {
	f := suite.leadFlight()
	suite.cacher.On("Standardize", suite.httpResponse).Return(suite.response)
	suite.cacher.On("Cache", "test-hash", suite.response)
	suite.response.On("Validate", mock.Anything).Return(false, 0)
	suite.response.On("StatusCode").Return(http.StatusOK)
	var done = make(chan bool)
	FlushSingleflight(suite.cacher, done)(suite.httpResponse)
	<-done
	suite.Assert().Equal("MISS", suite.httpResponse.Header.Get("X-Honey-Cache"))
	suite.cacher.AssertCalled(suite.T(), "Cache", "test-hash", suite.response)
	<-f.Done()
	suite.Assert().Equal(singleflight.Result{Response: suite.response}, f.Result(), "the response should be shared with waiting requests")
	suite.Assert().False(flights.InFlight("test-hash"))
	suite.Assert().Equal(http.StatusOK, suite.httpResponse.StatusCode)
}

func (suite *ResponderTestSuite) TestFlushSingleflightMissButValidates() {
	suite.request.Header.Set("Cache-Control", "must-revalidate")
	f := suite.leadFlight()
	suite.cacher.On("Standardize", suite.httpResponse).Return(suite.response)
	suite.cacher.On("Cache", "test-hash", suite.response)
	suite.response.On("Validate", mock.Anything).Return(true, http.StatusNotModified)
	suite.response.On("StatusCode").Return(http.StatusOK)
	var done = make(chan bool)
	FlushSingleflight(suite.cacher, done)(suite.httpResponse)
	<-done
	suite.Assert().Equal("MISS", suite.httpResponse.Header.Get("X-Honey-Cache"))
	suite.cacher.AssertCalled(suite.T(), "Cache", "test-hash", suite.response)
	<-f.Done()
	suite.Assert().Equal(http.StatusNotModified, suite.httpResponse.StatusCode)
}

func (suite *ResponderTestSuite) TestFlushSingleflightHit() {
	suite.request.Header.Set("If-None-Match", `"abc123"`)
	suite.response.Header().Set("Etag", `"abc123"`)
	f := suite.leadFlight()
	suite.cacher.On("Standardize", suite.httpResponse).Return(suite.response)
	suite.cacher.On("Cache", "test-hash", suite.response)
	suite.response.On("StatusCode").Return(http.StatusOK)
	var done = make(chan bool)
	FlushSingleflight(suite.cacher, done)(suite.httpResponse)
	<-done
	suite.Assert().Equal("MISS", suite.httpResponse.Header.Get("X-Honey-Cache"))
	suite.cacher.AssertCalled(suite.T(), "Cache", "test-hash", suite.response)
	<-f.Done()
	suite.Assert().Equal(http.StatusNotModified, suite.httpResponse.StatusCode)
}

func (suite *ResponderTestSuite) TestFlushSingleflightIgnoresRequestsWithoutFlight() {
	suite.httpResponse.Request = suite.request
	suite.Assert().NoError(FlushSingleflight(suite.cacher, nil)(suite.httpResponse))
	suite.cacher.AssertNotCalled(suite.T(), "Standardize", suite.httpResponse)
}
