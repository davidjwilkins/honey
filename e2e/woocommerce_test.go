//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// woocommerce skips the test unless WooCommerce was installed (see
// run.sh), and returns the Honey jar product's ID
func woocommerce(t *testing.T) string {
	t.Helper()
	if os.Getenv("HONEY_E2E_WOOCOMMERCE") == "" {
		t.Skip("set HONEY_E2E_WOOCOMMERCE=1 to install WooCommerce and test it")
	}
	id := wp(t, "post", "list", "--post_type=product", "--name=honey-jar", "--field=ID")
	require.NotEmpty(t, id, "the Honey jar product")
	return id
}

func cookieNames(jar http.CookieJar) []string {
	u, _ := url.Parse(site)
	var names []string
	for _, c := range jar.Cookies(u) {
		names = append(names, c.Name)
	}
	return names
}

func hasCookie(jar http.CookieJar, prefix string) bool {
	for _, name := range cookieNames(jar) {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func TestShopPagesAreCached(t *testing.T) {
	woocommerce(t)
	c := client(nil)
	for _, path := range []string{"/product/honey-jar/", "/shop/"} {
		// The shop may have been cached (by the warmer) before the jar was
		// added, and soft purged since
		eventually(t, c, path, "Honey jar")
		assert.Equal(t, "HIT", get(t, c, path).Cache, path)
	}
}

func TestAddingToCartStartsASession(t *testing.T) {
	id := woocommerce(t)
	anonymous := client(nil)
	get(t, anonymous, "/product/honey-jar/")
	require.Equal(t, "HIT", get(t, anonymous, "/product/honey-jar/").Cache)

	var sessions []string
	for i := 0; i < 2; i++ {
		jar := newJar(t)
		shopper := client(jar)
		resp := do(t, shopper, http.MethodGet, "/product/honey-jar/?add-to-cart="+id, nil)
		assert.NotEqual(t, "HIT", resp.Cache, "adding to the cart is never served from the cache")
		require.True(t, hasCookie(jar, "wp_woocommerce_session_"), "the shopper gets a session: %v", cookieNames(jar))
		require.True(t, hasCookie(jar, "woocommerce_items_in_cart"), "%v", cookieNames(jar))
		u, _ := url.Parse(site)
		for _, c := range jar.Cookies(u) {
			if strings.HasPrefix(c.Name, "wp_woocommerce_session_") {
				sessions = append(sessions, c.Value)
			}
		}

		// The shopper's pages are their own
		page := get(t, shopper, "/product/honey-jar/")
		assert.Equal(t, "NO-CACHE", page.Cache)
		// The cart page is filled in by JavaScript, from the Store API
		cart, _ := getStoreCart(t, shopper, "")
		assert.Equal(t, 1, cart.ItemsCount, "the shopper's cart has their jar")
	}
	require.Len(t, sessions, 2)
	assert.NotEqual(t, sessions[0], sessions[1], "each shopper has their own session")

	// and everyone else still gets the cached page, without anyone's cart
	page := get(t, anonymous, "/product/honey-jar/")
	assert.Equal(t, "HIT", page.Cache)
	notContains(t, page.Body, "has been added to your cart")
}

// The cart page isn't here: WooCommerce's cart block fills it in with
// JavaScript, from the Store API, so the page itself is the same for
// everyone, and WooCommerce lets it be cached.
func TestCheckoutAndAccountAreNeverCached(t *testing.T) {
	woocommerce(t)
	c := client(nil)
	for _, path := range []string{"/checkout/", "/my-account/"} {
		get(t, c, path)
		assert.NotEqual(t, "HIT", get(t, c, path).Cache, path)
	}
}

// storeCart is the Store API's cart
type storeCart struct {
	ItemsCount int `json:"items_count"`
}

func getStoreCart(t *testing.T, c *http.Client, token string) (storeCart, string) {
	t.Helper()
	var headers []string
	if token != "" {
		headers = []string{"Cart-Token", token}
	}
	resp := get(t, c, "/wp-json/wc/store/v1/cart", headers...)
	require.Equal(t, http.StatusOK, resp.StatusCode, resp.Body)
	var cart storeCart
	require.NoError(t, json.Unmarshal([]byte(resp.Body), &cart), resp.Body)
	return cart, resp.Header.Get("Cart-Token")
}

func TestStoreAPICartsArentShared(t *testing.T) {
	id := woocommerce(t)
	c := client(nil)

	// A shopper using the Store API (e.g. a headless shop) has a Cart-Token
	// rather than cookies
	cart, token := getStoreCart(t, c, "")
	require.NotEmpty(t, token, "the Store API gives a Cart-Token")
	assert.Zero(t, cart.ItemsCount)
	resp := do(t, c, http.MethodPost, "/wp-json/wc/store/v1/cart/add-item", url.Values{"id": {id}, "quantity": {"1"}},
		"Cart-Token", token)
	require.Equal(t, http.StatusCreated, resp.StatusCode, resp.Body)

	cart, _ = getStoreCart(t, c, token)
	assert.Equal(t, 1, cart.ItemsCount, "their cart has the jar")
	cart, _ = getStoreCart(t, c, "")
	assert.Zero(t, cart.ItemsCount, "someone else's cart doesn't")
	cart, _ = getStoreCart(t, c, token)
	assert.Equal(t, 1, cart.ItemsCount)
}
