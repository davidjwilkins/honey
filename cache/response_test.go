package cache

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
)

type ResponseTestSuite struct {
	suite.Suite
	request  *http.Request
	response Response
}

func (suite *ResponseTestSuite) SetupTest() {
	suite.request = validRequest()
}

func TestResponseTestSuite(t *testing.T) {
	suite.Run(t, new(ResponseTestSuite))
}

func (suite *ResponseTestSuite) TestResponseRevalidateSMaxAgeValid() {
	suite.response = &responseImpl{
		body:    []byte("Test Response Body"),
		headers: http.Header{},
		once:    sync.Once{},
		now:     time.Now().Add(time.Second * -99),
	}
	suite.request.Header.Set("Cache-Control", "must-revalidate")
	suite.response.Header().Set("Cache-Control", `s-maxage="100"`)
	valid, code := suite.response.Validate(suite.request)
	suite.Assert().True(valid, "A fresh response should be considered valid")
	suite.Assert().Equal(http.StatusNotModified, code, "A valid response should be 304 Not Modified")
}

func (suite *ResponseTestSuite) TestResponseRevalidateSMaxAgeInvalid() {
	suite.request.Header.Add("Cache-Control", "must-revalidate")
	suite.response = &responseImpl{
		body:    []byte("Test Response Body"),
		headers: http.Header{},
		once:    sync.Once{},
		now:     time.Now().Add(time.Second * -101),
	}
	suite.response.Header().Set("Cache-Control", `s-maxage="100"`)
	valid, _ := suite.response.Validate(suite.request)
	suite.Assert().False(valid, "A stale response should be considered invalid")
}

func (suite *ResponseTestSuite) TestResponseRevalidateMaxAgeValid() {
	suite.response = &responseImpl{
		body:    []byte("Test Response Body"),
		headers: http.Header{},
		once:    sync.Once{},
		now:     time.Now().Add(time.Second * -99),
	}
	suite.request.Header.Set("Cache-Control", "must-revalidate")
	suite.response.Header().Set("Cache-Control", `max-age="100"`)
	valid, code := suite.response.Validate(suite.request)
	suite.Assert().True(valid, "A fresh response should be considered valid")
	suite.Assert().Equal(http.StatusNotModified, code, "A valid response should be 304 Not Modified")
}

func (suite *ResponseTestSuite) TestResponseRevalidateMaxAgeInvalid() {
	suite.request.Header.Set("Cache-Control", "must-revalidate")
	suite.response = &responseImpl{
		body:    []byte("Test Response Body"),
		headers: http.Header{},
		once:    sync.Once{},
		now:     time.Now().Add(time.Second * -101),
	}
	suite.response.Header().Set("Cache-Control", `max-age="100"`)
	valid, _ := suite.response.Validate(suite.request)
	suite.Assert().False(valid, "A stale response should be considered invalid")
}

func (suite *ResponseTestSuite) TestResponseRevalidateSMaxAgeOverridesMaxAge() {
	suite.response = &responseImpl{
		body:    []byte("Test Response Body"),
		headers: http.Header{},
		once:    sync.Once{},
		now:     time.Now().Add(time.Second * -50),
	}
	suite.request.Header.Set("Cache-Control", "must-revalidate")
	suite.response.Header().Set("Cache-Control", `max-age="100",s-maxage="10"`)
	valid, _ := suite.response.Validate(suite.request)
	suite.Assert().False(valid, "s-maxage should take precedence over max-age")
}

func (suite *ResponseTestSuite) TestResponseRevalidateExpiresValid() {
	suite.response = &responseImpl{
		body:    []byte("Test Response Body"),
		headers: http.Header{},
		once:    sync.Once{},
		now:     time.Now(),
	}
	suite.request.Header.Set("Cache-Control", "must-revalidate")
	suite.response.Header().Set("Expires", time.Now().Add(time.Second).Format(time.RFC1123))
	valid, code := suite.response.Validate(suite.request)
	suite.Assert().True(valid, "Future expires should be valid")
	suite.Assert().Equal(http.StatusNotModified, code, "A valid response should be 304 Not Modified")
}

func (suite *ResponseTestSuite) TestResponseRevalidateExpiresInvalid() {
	suite.response = &responseImpl{
		body:    []byte("Test Response Body"),
		headers: http.Header{},
		once:    sync.Once{},
		now:     time.Now(),
	}
	suite.request.Header.Set("Cache-Control", "must-revalidate")
	suite.response.Header().Set("Expires", time.Now().Add(time.Second*-1).Format(time.RFC1123))
	valid, _ := suite.response.Validate(suite.request)
	suite.Assert().False(valid, "Past expires should be invalid")
}

func (suite *ResponseTestSuite) TestResponseRevalidateExpiresRFC850Invalid() {
	suite.response = &responseImpl{
		body:    []byte("Test Response Body"),
		headers: http.Header{},
		once:    sync.Once{},
		now:     time.Now(),
	}
	suite.request.Header.Set("Cache-Control", "must-revalidate")
	suite.response.Header().Set("Expires", time.Now().Add(time.Second*-1).Format(time.RFC850))
	valid, _ := suite.response.Validate(suite.request)
	suite.Assert().False(valid, "Past expires should be invalid")
}

func (suite *ResponseTestSuite) TestResponseRevalidateExpiresANSICInvalid() {
	return
	suite.response = &responseImpl{
		body:    []byte("Test Response Body"),
		headers: http.Header{},
		once:    sync.Once{},
		now:     time.Now(),
	}
	suite.request.Header.Set("Cache-Control", "must-revalidate")
	suite.response.Header().Set("Expires", time.Now().Add(time.Second*-1).Format(time.ANSIC))
	valid, _ := suite.response.Validate(suite.request)
	suite.Assert().False(valid, "Past expires should be invalid")
}

func (suite *ResponseTestSuite) TestResponseRevalidateExpiresRFC1123ZInvalid() {
	suite.response = &responseImpl{
		body:    []byte("Test Response Body"),
		headers: http.Header{},
		once:    sync.Once{},
		now:     time.Now(),
	}
	suite.request.Header.Set("Cache-Control", "must-revalidate")
	suite.response.Header().Set("Expires", time.Now().Add(time.Second*-1).Format(time.RFC1123Z))
	valid, _ := suite.response.Validate(suite.request)
	suite.Assert().False(valid, "Past expires should be invalid")
}

func (suite *ResponseTestSuite) TestResponseRevalidateExpiresRFC850Valid() {
	suite.response = &responseImpl{
		body:    []byte("Test Response Body"),
		headers: http.Header{},
		once:    sync.Once{},
		now:     time.Now(),
	}
	suite.request.Header.Set("Cache-Control", "must-revalidate")
	suite.response.Header().Set("Expires", time.Now().Add(time.Second).Format(time.RFC850))
	valid, code := suite.response.Validate(suite.request)
	suite.Assert().True(valid, "Future expires should be valid")
	suite.Assert().Equal(http.StatusNotModified, code, "A valid response should be 304 Not Modified")
}

func (suite *ResponseTestSuite) TestResponseRevalidateExpiresANSICValid() {
	suite.response = &responseImpl{
		body:    []byte("Test Response Body"),
		headers: http.Header{},
		once:    sync.Once{},
		now:     time.Now(),
	}
	suite.request.Header.Set("Cache-Control", "must-revalidate")
	suite.response.Header().Set("Expires", time.Now().Add(time.Second).Format(time.ANSIC))
	valid, code := suite.response.Validate(suite.request)
	suite.Assert().True(valid, "Future expires should be valid")
	suite.Assert().Equal(http.StatusNotModified, code, "A valid response should be 304 Not Modified")
}

func (suite *ResponseTestSuite) TestResponseRevalidateExpiresRFC1123ZValid() {
	suite.response = &responseImpl{
		body:    []byte("Test Response Body"),
		headers: http.Header{},
		once:    sync.Once{},
		now:     time.Now(),
	}
	suite.request.Header.Set("Cache-Control", "must-revalidate")
	suite.response.Header().Set("Expires", time.Now().Add(time.Second).Format(time.RFC1123Z))
	valid, code := suite.response.Validate(suite.request)
	suite.Assert().True(valid, "Future expires should be valid")
	suite.Assert().Equal(http.StatusNotModified, code, "A valid response should be 304 Not Modified")
}

func (suite *ResponseTestSuite) TestResponseIfModifiedSinceNotModified() {
	suite.response = &responseImpl{
		body:    []byte("Test Response Body"),
		headers: http.Header{},
		once:    sync.Once{},
		now:     time.Now(),
	}
	suite.response.Header().Set("Last-Modified", time.Now().Add(time.Hour*-2).Format(time.RFC1123))
	suite.request.Header.Set("If-Modified-Since", time.Now().Format(time.RFC1123))
	valid, code := suite.response.Validate(suite.request)
	suite.Assert().True(valid, "Should return true if not modified since If-Modified-Since")
	suite.Assert().Equal(http.StatusNotModified, code, "A valid response should be 304 Not Modified")
}

func (suite *ResponseTestSuite) TestResponseIfModifiedSinceModified() {
	suite.response = &responseImpl{
		body:    []byte("Test Response Body"),
		headers: http.Header{},
		once:    sync.Once{},
		now:     time.Now(),
	}
	suite.response.Header().Set("Last-Modified", time.Now().Format(time.RFC1123))
	suite.request.Header.Set("If-Modified-Since", time.Now().Add(time.Hour*-2).Format(time.RFC1123))
	valid, _ := suite.response.Validate(suite.request)
	suite.Assert().False(valid, "Should return false if modified after If-Modified-Since")
}

func (suite *ResponseTestSuite) TestResponseIfUnmodifiedSinceNotModified() {
	suite.response = &responseImpl{
		body:    []byte("Test Response Body"),
		headers: http.Header{},
		once:    sync.Once{},
		now:     time.Now(),
	}
	suite.response.Header().Set("Last-Modified", time.Now().Add(time.Hour*-2).Format(time.RFC1123))
	suite.request.Header.Set("If-Unmodified-Since", time.Now().Format(time.RFC1123))
	valid, _ := suite.response.Validate(suite.request)
	suite.Assert().False(valid, "Should return false if not modified since If-Unmodified-Since")
}

func (suite *ResponseTestSuite) TestResponseIfUnmodifiedSinceModified() {
	suite.response = &responseImpl{
		body:    []byte("Test Response Body"),
		headers: http.Header{},
		once:    sync.Once{},
		now:     time.Now(),
	}
	suite.response.Header().Set("Last-Modified", time.Now().Format(time.RFC1123))
	suite.request.Header.Set("If-Unmodified-Since", time.Now().Add(time.Hour*-2).Format(time.RFC1123))
	valid, code := suite.response.Validate(suite.request)
	suite.Assert().True(valid, "Should return false if modified after If-Modified-Since")
	suite.Assert().Equal(http.StatusPreconditionFailed, code, "If-Unmodified-Since should return true, 412 Precondition Failed if modified")
}
