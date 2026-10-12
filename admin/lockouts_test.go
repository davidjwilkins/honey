package admin

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/events"
	"github.com/davidjwilkins/honey/guard"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLockouts(t *testing.T) {
	locked := map[string]time.Time{"203.0.113.9": time.Now().Add(10 * time.Minute)}
	h := Handler(Options{
		Events: events.New(0),
		Lockouts: func() []guard.Lockout {
			var l []guard.Lockout
			for address, until := range locked {
				l = append(l, guard.Lockout{Address: address, Until: until, From: guard.BannedFromBackend})
			}
			return l
		},
		Unblock: func(address string) bool {
			_, ok := locked[address]
			delete(locked, address)
			return ok
		},
	})
	page := do(h, http.MethodGet, "/", "127.0.0.1:9090", nil).Body.String()
	assert.Contains(t, page, "Blocked addresses")
	assert.Contains(t, page, "<td>WordPress (for probing)</td>")
	assert.Contains(t, page, `<th scope="row" class="mono">203.0.113.9</th>`)
	assert.Contains(t, page, `name="address" value="203.0.113.9"`)

	resp := post(h, "/unblock", url.Values{"address": {"203.0.113.9"}})
	require.Equal(t, http.StatusSeeOther, resp.Code)
	page = do(h, http.MethodGet, resp.Header.Get("Location"), "127.0.0.1:9090", nil).Body.String()
	assert.Contains(t, page, "Unblocked <span class=\"mono\">203.0.113.9</span>")
	assert.NotContains(t, page, "Blocked addresses", "the list is gone with the last lockout")

	resp = post(h, "/unblock", url.Values{"address": {"203.0.113.9"}})
	page = do(h, http.MethodGet, resp.Header.Get("Location"), "127.0.0.1:9090", nil).Body.String()
	assert.Contains(t, page, "203.0.113.9 isn&#39;t blocked.")

	w := do(h, http.MethodPost, "/unblock", "127.0.0.1:9090", url.Values{"address": {"203.0.113.9"}}, "Origin", "https://evil.example")
	assert.Equal(t, http.StatusForbidden, w.Code, "other sites can't post to it")
}
