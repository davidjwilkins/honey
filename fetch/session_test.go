package fetch

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/cache"
	"github.com/stretchr/testify/assert"
)

// personalOrigin greets logged in users by name, without saying that the
// page is personal (as WordPress themes and plugins may not)
func personalOrigin(cacheControl string) *origin {
	return newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		w.Header().Set("Cache-Control", cacheControl)
		if c, err := r.Cookie("wordpress_logged_in_0123abcd"); err == nil {
			io.WriteString(w, "Howdy, "+c.Value)
			return
		}
		io.WriteString(w, "Hello, visitor")
	})
}

func TestLoggedInPagesAreNotCached(t *testing.T) {
	o := personalOrigin("max-age=600")
	proxy := newProxyWith(t, o, cache.Options{BypassCookies: cache.WordPressBypassCookies})
	loggedIn := []string{"Cookie", "wordpress_logged_in_0123abcd=admin"}

	resp, body := get(t, proxy.URL+"/", loggedIn...)
	assert.Equal(t, "Howdy, admin", body)
	assert.Equal(t, "NO-CACHE", resp.Header.Get("X-Honey-Cache"))
	resp, body = get(t, proxy.URL+"/")
	assert.Equal(t, "Hello, visitor", body, "a logged in user's page mustn't be served to anyone else")
	assert.Equal(t, "MISS", resp.Header.Get("X-Honey-Cache"))

	resp, body = get(t, proxy.URL+"/", loggedIn...)
	assert.Equal(t, "Howdy, admin", body, "nor the cached page to logged in users")
	assert.Equal(t, "NO-CACHE", resp.Header.Get("X-Honey-Cache"))
	resp, _ = get(t, proxy.URL+"/")
	assert.Equal(t, "HIT", resp.Header.Get("X-Honey-Cache"))
}

func TestPrivateResponsesAreNotShared(t *testing.T) {
	o := personalOrigin("private, max-age=600")
	// Without bypass cookies, only private stops it being shared
	proxy := newProxy(t, o)

	_, body := get(t, proxy.URL+"/", "Cookie", "wordpress_logged_in_0123abcd=admin")
	assert.Equal(t, "Howdy, admin", body)
	resp, body := get(t, proxy.URL+"/")
	assert.Equal(t, "Hello, visitor", body)
	assert.Equal(t, "MISS", resp.Header.Get("X-Honey-Cache"))
	assert.Equal(t, 2, o.Hits())
}

func TestResponsesStartingASessionAreNotCached(t *testing.T) {
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		w.Header().Set("Cache-Control", "max-age=600")
		if r.URL.Query().Get("add-to-cart") != "" {
			http.SetCookie(w, &http.Cookie{Name: "woocommerce_items_in_cart", Value: "1", Path: "/"})
			http.SetCookie(w, &http.Cookie{Name: "wp_woocommerce_session_0123abcd", Value: "abc", Path: "/"})
			io.WriteString(w, "Added to your cart")
			return
		}
		io.WriteString(w, "Shop")
	})
	proxy := newProxyWith(t, o, cache.Options{BypassCookies: cache.WordPressBypassCookies})

	for i := 0; i < 2; i++ {
		resp, body := get(t, proxy.URL+"/shop/?add-to-cart=12")
		assert.Equal(t, "Added to your cart", body)
		assert.Equal(t, "NO-CACHE", resp.Header.Get("X-Honey-Cache"))
		assert.Len(t, resp.Header.Values("Set-Cookie"), 2, "the shopper's session cookies must reach them")
	}
	assert.Equal(t, 2, o.Hits(), "everyone adding to their cart must reach the backend")
}

func TestWaitersFetchTheirOwnSession(t *testing.T) {
	release := make(chan struct{})
	o := newOrigin(func(w http.ResponseWriter, r *http.Request, hit int) {
		<-release
		http.SetCookie(w, &http.Cookie{Name: "wp_woocommerce_session_0123abcd", Value: strings.Repeat("x", hit), Path: "/"})
		io.WriteString(w, "Added to your cart")
	})
	proxy := newProxyWith(t, o, cache.Options{BypassCookies: cache.WordPressBypassCookies})

	var wg sync.WaitGroup
	cookies := make(chan string, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _ := get(t, proxy.URL+"/shop/?add-to-cart=12")
			cookies <- resp.Header.Get("Set-Cookie")
		}()
	}
	// for the first to reach the backend, and the others to wait for it
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
	close(cookies)
	seen := map[string]bool{}
	for cookie := range cookies {
		seen[cookie] = true
	}
	assert.Len(t, seen, 3, "each shopper gets their own session")
}
