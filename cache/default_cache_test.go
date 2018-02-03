package cache

import (
	"bytes"
	"io/ioutil"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
)

func newValidRequest(uri string) *http.Request {
	url, err := url.Parse(uri)
	if err != nil {
		panic(err)
	}
	return &http.Request{
		Method: http.MethodGet,
		URL:    url,
		Header: http.Header{},
	}
}
func validRequest() *http.Request {
	return newValidRequest("https://www.insomniac.com")
}
func TestDefaultCacheCanOnlyCacheGetAndHeadRequests(t *testing.T) {
	var cache defaultCacher
	invalidMethods := []string{
		http.MethodConnect,
		http.MethodDelete,
		http.MethodOptions,
		http.MethodPatch,
		http.MethodPost,
		http.MethodPut,
		http.MethodTrace,
		"Fake method",
	}
	var request = validRequest()
	for _, method := range invalidMethods {
		request.Method = method
		if cache.CanCache(request) {
			t.Error("Cacher should only be able to cache GET and HEAD requests")
		}
	}
	validMethods := []string{
		http.MethodGet,
		http.MethodHead,
	}
	for _, method := range validMethods {
		request.Method = method
		if !cache.CanCache(request) {
			t.Error("Cacher should be able to cache GET and HEAD requests")
		}
	}
}

func TestDefaultCacheCanCachePage(t *testing.T) {
	var request = validRequest()
	var cache defaultCacher
	if !cache.CanCache(request) {
		t.Error("Default cacher should be able to cache valid pages")
	}
}

func TestDefaultCacheCannotCacheStaticFiles(t *testing.T) {
	var request = newValidRequest("https://www.example.com/images/test.jpg")
	var cache defaultCacher
	if cache.CanCache(request) {
		t.Error("Default cacher should not be able to cache static files: ", request.URL.Path)
	}
}
func TestDefaultCacheCannotCacheIfAuthorizationHeader(t *testing.T) {
	request := validRequest()
	request.Header.Add("Authorization", "P@ssw0rd")
	var cache defaultCacher
	if cache.CanCache(request) {
		t.Error("Default cacher should not be able to cache if authorization header")
	}
}

func TestDefaultCacheCannotCacheIfPreviewQueryString(t *testing.T) {
	request := newValidRequest("https://www.insomniac.com/test/page?preview=false")
	var cache = NewDefaultCacher()
	if !cache.CanCache(request) {
		t.Error("Default cacher should cache if preview is set but not true")
	}
	request = newValidRequest("https://www.insomniac.com/test/page?preview=true")
	if cache.CanCache(request) {
		t.Error("Default cacher should not cache if preview is set to true")
	}
}

func TestDefaultCacheDoesNotCacheLoginPage(t *testing.T) {
	request := newValidRequest("https://www.insomniac.com/wp-login")
	var cache = NewDefaultCacher()
	if cache.CanCache(request) {
		t.Error("Default cacher should not cache WP login page")
	}
}

func TestDefaultCacheDoesNotCacheAdminPage(t *testing.T) {
	request := newValidRequest("https://www.insomniac.com/wp-admin")
	var cache = NewDefaultCacher()
	if cache.CanCache(request) {
		t.Error("Default cacher should not cache WP admin page")
	}
}

func TestDefaultCacheDoesNotCacheWpRssFeed(t *testing.T) {
	request := newValidRequest("https://www.insomniac.com/feed")
	var cache = NewDefaultCacher()
	if cache.CanCache(request) {
		t.Error("Default cacher should not cache WP RSS feed")
	}
}

func TestDefaultCacheHashesAreDifferentIfDomainsAreDifferent(t *testing.T) {
	requestA := newValidRequest("https://www.insomniac.com/feed")
	requestB := newValidRequest("https://www.nightowls.com/feed")
	var cache = NewDefaultCacher()
	if cache.Hash(requestA) == cache.Hash(requestB) {
		t.Error("Default cacher should hash different domains differently")
	}
}

func TestDefaultCacheHashesAreDifferentIfPathsAreDifferent(t *testing.T) {
	requestA := newValidRequest("https://www.insomniac.com/feed")
	requestB := newValidRequest("https://www.insomniac.com/home")
	var cache = NewDefaultCacher()
	if cache.Hash(requestA) == cache.Hash(requestB) {
		t.Error("Default cacher should hash different paths differently")
	}
}

func TestDefaultCacheHashDoesNotIncludeCookiesUnlessAllowed(t *testing.T) {
	requestA := newValidRequest("https://www.insomniac.com/feed")
	requestB := newValidRequest("https://www.insomniac.com/feed")
	requestB.AddCookie(&http.Cookie{
		Name:  "Not Allowed Cookie",
		Value: "This Doesn't matter",
	})
	var cache = NewDefaultCacher()
	hash := cache.Hash(requestA)
	cache.vary.Store(hash, "cookie")
	defer cache.vary.Delete(hash)
	assert.Equal(t, cache.Hash(requestA), cache.Hash(requestB), "Hash should be equal if cookie not in allowed list")

}

func TestDefaultCacheHashDoesIncludeCookiesIfAllowed(t *testing.T) {
	requestA := newValidRequest("https://www.insomniac.com/feed")
	requestB := newValidRequest("https://www.insomniac.com/feed")
	requestB.AddCookie(&http.Cookie{
		Name:  "site_lang_id",
		Value: "1",
	})
	var cache = NewDefaultCacher()
	cache.AddAllowedCookie("site_lang_id")
	hash := cache.Hash(requestA)
	cache.vary.Store(hash, "cookie")
	cache.vary.Store(hash, "cookie")
	defer cache.vary.Delete(hash)
	assert.NotEqual(t, cache.Hash(requestA), cache.Hash(requestB), "Hash should not be equal if allowed cookies are different")
}

func TestDefaultCacheRemovesCookiesIfNotAllowed(t *testing.T) {
	response := http.Response{
		Header: http.Header(map[string][]string{
			"Set-Cookie": []string{
				"site_lang_id=1; HttpOnly; Path=/",
				"remove_me=1; HttpOnly; Path=/",
			},
		}),

		Body: ioutil.NopCloser(bytes.NewBuffer([]byte("test"))),
	}
	var cache = NewDefaultCacher()
	cache.AddAllowedCookie("site_lang_id")
	cache.Standardize(&response)
	cookies := response.Cookies()
	if len(cookies) >= 2 {
		t.Error("Default cacher should remove cookies not added to the allowed list from the response")
	} else if len(cookies) == 0 {
		t.Error("Default cacher should not remove cookies added to the allowed list from the response")
	}
}

func TestDefaultCacheAddsAndRetrievesItemsFromCache(t *testing.T) {
	response := http.Response{
		Header: http.Header{},
		Body:   ioutil.NopCloser(bytes.NewBuffer([]byte("test"))),
	}
	var cache = NewDefaultCacher()
	r := cache.Standardize(&response)
	request := newValidRequest("https://www.insomniac.com")

	hash := cache.Hash(request)
	cache.Cache(hash, r)
	stored, ok := cache.Load(hash, request)
	if !ok {
		t.Error("Cacher should store cached responses")
	}
	if stored != r {
		t.Error("Cacher should return the appropriate stored response")
	}
}

func TestDefaultCacheDoesntFindIfCookiesDontMatch(t *testing.T) {
	response := http.Response{
		Header: http.Header{},
		Body:   ioutil.NopCloser(bytes.NewBuffer([]byte("test"))),
	}
	response.Header.Set("Vary", "cookie")
	var cache = NewDefaultCacher()
	cache.AddAllowedCookie("site_land_id")
	r := cache.Standardize(&response)
	requestA := newValidRequest("https://www.insomniac.com")
	requestB := newValidRequest("https://www.insomniac.com")
	requestA.AddCookie(&http.Cookie{Name: "site_lang_id", Value: "1", HttpOnly: false})
	hash := cache.Hash(requestA)
	cache.Cache(hash, r)
	_, ok := cache.Load(hash, requestB)
	if ok {
		t.Error("Cacher should not match if cookies don't match")
	}
}
