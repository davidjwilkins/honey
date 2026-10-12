package utilities

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCookies(t *testing.T) {
	header := http.Header{"Cookie": {"a=1; b = 2 ;; c", `d="x y"`}}
	assert.Equal(t, [][2]string{{"a", "1"}, {"b", "2"}, {"c", ""}, {"d", `"x y"`}}, Cookies(header))
	assert.Empty(t, Cookies(http.Header{}))
}

func TestKeepCookies(t *testing.T) {
	keepLang := func(name string) bool { return name == "lang" }

	header := http.Header{"Cookie": {"theme=evil; lang=fr", "_ga=GA1.2.3"}}
	assert.Equal(t, 2, KeepCookies(header, keepLang))
	assert.Equal(t, []string{"lang=fr"}, header.Values("Cookie"))

	header = http.Header{"Cookie": {"theme=evil"}}
	assert.Equal(t, 1, KeepCookies(header, keepLang))
	assert.NotContains(t, header, "Cookie")

	header = http.Header{"Cookie": {"lang=fr"}}
	assert.Equal(t, 0, KeepCookies(header, keepLang))
	assert.Equal(t, []string{"lang=fr"}, header.Values("Cookie"), "left as it was")
}
