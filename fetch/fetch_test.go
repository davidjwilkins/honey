package fetch

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
)

type FetchTestSuite struct {
	suite.Suite
	backend     *url.URL
	cacher      *testCacher
	handler     *testHandler
	request     *http.Request
	response    *testResponse
	writer      *httptest.ResponseRecorder
	multiplexer *testMultiplexer
}

type testHandler struct {
	mock.Mock
}

func (t *testHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t.Called(w, r)
}

func (suite *FetchTestSuite) SetupTest() {
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
	suite.backend, _ = url.Parse("https://www.insomniac.com")
	suite.handler = &testHandler{}
	suite.handler.On("ServeHTTP", suite.writer, suite.request)
}

// In order for 'go test' to run this suite, we need to create
// a normal test function and pass our suite to suite.Run
func TestFetchTestSuite(t *testing.T) {
	suite.Run(t, new(FetchTestSuite))
}

func (suite *FetchTestSuite) TestFetchUncacheableViaCache() {
	suite.cacher.On("CanCache", suite.request).Return(false)
	Fetch(suite.cacher, suite.handler, suite.backend)(suite.writer, suite.request)
	suite.Assert().Equal("NO-CACHE", suite.writer.Header().Get("X-Honey-Cache"), "X-Honey-Cache: NO-CACHE should be set for uncacheable requests")
	suite.handler.AssertCalled(suite.T(), "ServeHTTP", suite.writer, suite.request)
	suite.cacher.AssertNotCalled(suite.T(), "Hash", suite.request)
	_, exists := multiplexers.Load("test-hash")
	suite.Assert().False(exists, "Multiplexer should not be created for uncacheable requests")
}

func (suite *FetchTestSuite) TestFetchUncacheableViaHeader() {
	suite.cacher.On("CanCache", suite.request).Return(true)
	suite.request.Header.Set("Cache-Control", "no-cache")
	Fetch(suite.cacher, suite.handler, suite.backend)(suite.writer, suite.request)
	suite.handler.AssertCalled(suite.T(), "ServeHTTP", suite.writer, suite.request)
}

func (suite *FetchTestSuite) TestFetchUncacheableOnlyIfCached() {
	suite.cacher.On("CanCache", suite.request).Return(true)
	suite.request.Header.Set("Cache-Control", "no-cache,only-if-cached")
	Fetch(suite.cacher, suite.handler, suite.backend)(suite.writer, suite.request)
	suite.handler.AssertNotCalled(suite.T(), "ServeHTTP", suite.writer, suite.request)
	suite.Assert().Equal(http.StatusGatewayTimeout, suite.writer.Code)
}

func (suite *FetchTestSuite) TestFetchUncacheableMultiplexed() {
	suite.cacher.On("CanCache", suite.request).Return(true)
	multiplexers.Load(suite.multiplexer)
	suite.multiplexer.On("AddWriter", suite.writer, suite.request)
	suite.multiplexer.On("Wait")
	suite.request.Header.Set("Cache-Control", "no-cache,only-if-cached")
	Fetch(suite.cacher, suite.handler, suite.backend)(suite.writer, suite.request)
	suite.handler.AssertNotCalled(suite.T(), "ServeHTTP", suite.writer, suite.request)
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

func TestSwitchBackendUrlIsChanged(t *testing.T) {
	backend, _ := url.Parse("http://www.backend.com")
	r := newTestValidRequest()
	SwitchBackend(r, backend)
	if r.URL.Scheme != "http" {
		t.Error("SwitchBackend should switch requests scheme to the correct one")
	}
	if r.URL.Host != "www.backend.com" {
		t.Error("SwitchBackend should switch requests host to the correct one")
	}
	if r.Host != "www.insomniac.com" {
		t.Errorf("Expected host www.insomniac.com, got %s", r.Host)
	}
}

func TestSwitchBackendXForwarderProtoIsAdded(t *testing.T) {
	backend, _ := url.Parse("http://www.backend.com")
	r := newTestValidRequest()
	SwitchBackend(r, backend)
	if r.Header.Get("X-Forwarded-Proto") != "https" {
		t.Error("SwitchBackend should set X-Forwarded-Proto header")
	}
}
