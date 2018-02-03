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
	valid := suite.response.Validate(suite.request)
	suite.Assert().True(valid, "A fresh response should be considered valid")
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
	valid := suite.response.Validate(suite.request)
	suite.Assert().False(valid, "A stale response should be considered valid")
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
	valid := suite.response.Validate(suite.request)
	suite.Assert().True(valid, "A fresh response should be considered valid")
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
	valid := suite.response.Validate(suite.request)
	suite.Assert().False(valid, "A stale response should be considered valid")
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
	valid := suite.response.Validate(suite.request)
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
	valid := suite.response.Validate(suite.request)
	suite.Assert().True(valid, "Future expires should be valid")
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
	valid := suite.response.Validate(suite.request)
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
	valid := suite.response.Validate(suite.request)
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
	valid := suite.response.Validate(suite.request)
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
	valid := suite.response.Validate(suite.request)
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
	valid := suite.response.Validate(suite.request)
	suite.Assert().True(valid, "Future expires should be valid")
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
	valid := suite.response.Validate(suite.request)
	suite.Assert().True(valid, "Future expires should be valid")
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
	valid := suite.response.Validate(suite.request)
	suite.Assert().True(valid, "Future expires should be valid")
}
