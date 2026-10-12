package fetch

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/cache"
	"github.com/stretchr/testify/assert"
)

func TestBackendGate(t *testing.T) {
	var closed atomic.Bool
	gate := func(r *http.Request) (time.Duration, string) {
		if closed.Load() {
			return 1500 * time.Millisecond, "security.rateLimit"
		}
		return 0, ""
	}
	o := noHeadersOrigin()
	proxy := routeProxy(t, o, cache.Options{BypassCookies: cache.WordPressBypassCookies, DefaultTTL: time.Hour}, Options{BackendGate: gate}).URL
	get(t, proxy+"/cached/")

	closed.Store(true)
	resp, _ := get(t, proxy+"/cached/")
	assert.Equal(t, http.StatusOK, resp.StatusCode, "cache hits are never limited")
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"))

	for _, tc := range []struct{ path, cookie string }{
		{"/not-cached/", ""},
		{"/cached/", "wordpress_logged_in_abc=admin"},
	} {
		resp, body := get(t, proxy+tc.path, "Cookie", tc.cookie)
		assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode, tc)
		assert.Equal(t, "2", resp.Header.Get("Retry-After"), tc)
		assert.Equal(t, "BLOCKED", resp.Header.Get("X-Honey-Cache"), tc)
		assert.Contains(t, body, "security.rateLimit", tc)
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&o.hits), "only the first request reached the backend")

	closed.Store(false)
	resp, _ = get(t, proxy+"/not-cached/")
	assert.Equal(t, "MISS", resp.Header.Get("X-Honey-Cache"), "a refused request isn't left in flight")
}

func TestHideUsersHeader(t *testing.T) {
	for _, hide := range []bool{false, true} {
		var got atomic.Value
		o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
			got.Store(r.Header.Get(HideUsersHeader))
		})
		proxy := routeProxy(t, o, cache.Options{}, Options{HideUsers: hide}).URL
		get(t, proxy+"/?x=1", HideUsersHeader, "forged")
		if hide {
			assert.Equal(t, "1", got.Load(), "Honey asks WordPress to hide its users")
		} else {
			assert.Equal(t, "", got.Load(), "visitors can't ask")
		}
	}
}
