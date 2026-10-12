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
	cached(t, anonymous, "/product/honey-jar/")

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

		// The shopper's pages are the cached ones, with their own cart
		// (see TestShoppersGetCachedPages)
		page := get(t, shopper, "/product/honey-jar/")
		assert.Equal(t, "HIT", page.Cache)
		assert.Contains(t, page.Body, `"items_count":1`)
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
	notContains(t, page.Body, `"items_count":1`)
}

// shopperInclude is how the plugin ends a page which shows the shopper's
// cart and store notices
const shopperInclude = `<!--esi <esi:include src="/?honey-esi=woocommerce.cart.notices"/> -->`

func TestShoppersGetCachedPages(t *testing.T) {
	id := woocommerce(t)
	anonymous := client(nil)
	for _, path := range []string{"/shop/", "/hello-world/", "/cart/"} {
		get(t, anonymous, path)
	}
	shop := cached(t, anonymous, "/shop/")
	assert.Contains(t, shop.Body, shopperInclude, "the shop shows the cart and notices")
	assert.Contains(t, shop.Body, `"items_count":0`, "everyone else gets it as it is")
	assert.NotEmpty(t, shop.Header.Get("Etag"), "like any other page")

	jar := newJar(t)
	shopper := client(jar)
	// Adding to the cart without JavaScript leaves a notice for the next
	// page which shows them
	get(t, shopper, "/hello-world/?add-to-cart="+id)
	require.True(t, hasCookie(jar, "wp_woocommerce_session_"), "%v", cookieNames(jar))

	page := get(t, shopper, "/shop/")
	assert.Equal(t, "HIT", page.Cache, "the shopper gets the cached shop")
	assert.Equal(t, "private, no-cache", page.Header.Get("Cache-Control"))
	notContains(t, page.Body, "<esi:")
	assert.Contains(t, page.Body, `"items_count":1`, "with their cart")
	assert.Contains(t, page.Body, "has been added to your cart", "and their notice")
	page = get(t, shopper, "/shop/")
	assert.Equal(t, "HIT", page.Cache)
	assert.Contains(t, page.Body, `"items_count":1`)
	notContains(t, page.Body, "has been added to your cart", "which they only see once")

	page = get(t, shopper, "/hello-world/")
	assert.Equal(t, "HIT", page.Cache, "pages which don't show the cart are the same for shoppers")
	notContains(t, page.Body, "honey-esi=woocommerce")

	assert.NotEqual(t, "HIT", get(t, shopper, "/cart/").Cache, "the cart is the shopper's own")

	shop = get(t, anonymous, "/shop/")
	assert.Equal(t, "HIT", shop.Cache)
	notContains(t, shop.Body, `"items_count":1`, "and nobody else gets their cart")

	// The include is only for shoppers
	resp := get(t, anonymous, "/?honey-esi=woocommerce.cart.notices")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, resp.Body)
	assert.Equal(t, http.StatusNotFound, get(t, anonymous, "/?honey-esi=woocommerce.other").StatusCode)

	// Commenters' cookies aren't the shop's
	u, _ := url.Parse(site)
	jar.SetCookies(u, []*http.Cookie{{Name: "comment_author_e2e", Value: "Jane", Path: "/"}})
	assert.Equal(t, "NO-CACHE", get(t, shopper, "/shop/").Cache)
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
