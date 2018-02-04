package cache

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/davidjwilkins/honey/utilities"
	blake2b "github.com/minio/blake2b-simd"
)

type defaultCacher struct {
	skipUrls           map[string]bool
	allowedCookies     map[string]bool
	allowedCookieNames []string
	skipRegex          []*regexp.Regexp
	entries            sync.Map
	vary               sync.Map
}

// NewDefaultCacher returns a cacher optimized
// for Wordpress - it will not cache the WP RSS
// feed or the wp-admin or wp-login pages.
func NewDefaultCacher() *defaultCacher {
	cacher := &defaultCacher{
		skipUrls: make(map[string]bool),
		skipRegex: []*regexp.Regexp{
			regexp.MustCompile("/(feed|wp-admin|wp-login)"),
		},
		allowedCookies:     make(map[string]bool),
		allowedCookieNames: []string{},
		entries:            sync.Map{},
		vary:               sync.Map{},
	}

	return cacher
}

// CanCache will return true if the method is a GET or
// HEAD request, does not have a static file extension,
// does not have an Authorization header, does not
// have preview=true in the query string, and is not
// for a url containing /wp-admin, /wp-login, or /feed
func (c *defaultCacher) CanCache(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if utilities.IsStaticFile(r.URL.Path) {
		return false
	}
	if r.Header.Get("Authorization") != "" {
		return false
	}
	if r.URL.Query().Get("preview") == "true" {
		return false
	}
	if skip, ok := c.skipUrls[r.URL.Path]; skip && ok {
		return false
	}
	for _, regex := range c.skipRegex {
		if regex.MatchString(r.URL.Path) {
			return false
		}
	}
	return true
}

// Hash creates a unique string for a request.  It includes
// the method, the url, and any allowed cookies.  It also
// includes the X-Honey-Vary header - which is used internally
// on multiplexed requests.
func (c *defaultCacher) Hash(r *http.Request) string {
	var buffer bytes.Buffer
	buffer.WriteString(fmt.Sprintf("%s :: %s", r.Method, r.URL.String()))

	v, found := c.vary.Load(buffer.String())
	if found {
		vary := v.(string)
		buffer.WriteString(utilities.GetVaryHeadersHash(r.Header, r, c.allowedCookieNames, vary))
	}
	if vary := r.Header.Get("X-Honey-Vary"); vary != "" {
		buffer.WriteString(vary)
	}
	return buffer.String()
}

// AddAllowedCookie adds a name to the list of cookies which
// are allowed through the cache.
func (c *defaultCacher) AddAllowedCookie(name string) {
	c.allowedCookies[name] = true
	c.allowedCookieNames = append(c.allowedCookieNames, name)
}

var replacer = strings.NewReplacer(`no-cache="set-cookie"`, "", ",,", "", "public", "")

func ccReplacer(cc string) string {
	return strings.Trim(replacer.Replace(cc), ",")
}

// Standardize removes set-cookie headers unless they are listed
// in the allowed cookies, reads the response, and saves it to
// a Response interface.
func (c *defaultCacher) Standardize(r *http.Response) Response {
	for i := 0; i < len(r.Header["Set-Cookie"]); i++ {
		line := r.Header["Set-Cookie"][i]
		parts := strings.Split(strings.TrimSpace(line), ";")
		if len(parts) == 1 && parts[0] == "" {
			continue
		}
		parts[0] = strings.TrimSpace(parts[0])
		j := strings.Index(parts[0], "=")
		if j < 0 {
			continue
		}
		name := parts[0][:j]
		if allowed, ok := c.allowedCookies[name]; !allowed || !ok {
			r.Header["Set-Cookie"] = append(r.Header["Set-Cookie"][:i], r.Header["Set-Cookie"][i+1:]...)
			i--
		}
	}
	resp := responseImpl{
		now:      time.Now(),
		headers:  http.Header{},
		response: r,
	}
	copyHeader(resp.headers, r.Header)

	cc := resp.headers.Get("Cache-Control")

	if strings.Contains(cc, `no-cache="set-cookie"`) {
		resp.headers.Del("Set-Cookie")
		resp.headers.Set("Cache-Control", ccReplacer(cc))
	}

	if !strings.Contains(r.Header.Get("Cache-Control"), "private") {
		cc = ccReplacer(cc + ",public")
		resp.headers.Set("Cache-Control", cc)
	}

	if !strings.Contains(cc, "no-cache") && !strings.Contains(cc, "max-age") {
		cc = ccReplacer(cc + ",max-age=300")
		resp.headers.Set("Cache-Control", cc)
	}

	if r.Header.Get("Last-Modified") == "" {
		r.Header.Set("Last-Modified", time.Now().Format(time.RFC1123))
	}

	if r.Header.Get("Expires") == "" {
		r.Header.Set("Expires", time.Now().Add(time.Hour*1).Format(time.RFC1123))
	}

	resp.response = r
	resp.body, _ = ioutil.ReadAll(r.Body)
	r.Body.Close()
	r.Body = ioutil.NopCloser(bytes.NewReader(resp.body))
	if !strings.Contains(cc, "no-store") {
		hasher := blake2b.New256()
		hasher.Write(resp.body)
		etag := base64.StdEncoding.EncodeToString(hasher.Sum(nil))
		resp.Header().Set("Etag", etag)
		r.Header.Set("Etag", etag)
	}
	resp.cookies = make(map[string]*http.Cookie)
	for _, cookie := range resp.response.Cookies() {
		resp.cookies[cookie.Name] = cookie
	}
	if r.Request != nil {
		resp.requestHeaders = r.Request.Header
	} else {
		resp.requestHeaders = http.Header{}
	}

	return &resp
}

// Cache will store the Response in the cache for later retrieval
func (c *defaultCacher) Cache(hash string, r Response) {
	vary := r.Header().Get("Vary")
	if vary != "" {
		c.vary.Store(hash, vary)
	}
	hash += utilities.GetVaryHeadersHash(r.RequestHeaders(), r, c.allowedCookieNames, vary)
	c.entries.Store(hash, r)
}

// Load returns a Response from the cache.  It returns the Response, if found, and
// a boolean indicating whether or not it was found (and matched the Vary header, if
// present)
func (c *defaultCacher) Load(hash string, request *http.Request) (Response, bool) {
	var r Response
	r_tmp, ok := c.entries.Load(hash)
	if ok {
		r = r_tmp.(Response)
	}
	return r, ok
}

func (c *defaultCacher) AllowedCookies() []string {
	return c.allowedCookieNames
}
