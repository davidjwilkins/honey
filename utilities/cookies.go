package utilities

import (
	"net/http"
	"strings"
)

// Cookies returns the name and value of each cookie in header's Cookie
// lines, as they were sent.  They are parsed by hand rather than with
// http.Request.Cookies, which skips cookies with values it considers
// invalid (but which a backend may still read).
func Cookies(header http.Header) [][2]string {
	var cookies [][2]string
	for _, line := range header.Values("Cookie") {
		for _, pair := range strings.Split(line, ";") {
			name, value, _ := strings.Cut(pair, "=")
			if name = strings.TrimSpace(name); name != "" {
				cookies = append(cookies, [2]string{name, strings.TrimSpace(value)})
			}
		}
	}
	return cookies
}

// KeepCookies removes the cookies which keep returns false for from
// header's Cookie lines, and returns how many it removed.
func KeepCookies(header http.Header, keep func(name string) bool) (removed int) {
	cookies := Cookies(header)
	var kept []string
	for _, cookie := range cookies {
		if keep(cookie[0]) {
			kept = append(kept, cookie[0]+"="+cookie[1])
		}
	}
	removed = len(cookies) - len(kept)
	if removed == 0 {
		return 0
	}
	if len(kept) == 0 {
		header.Del("Cookie")
	} else {
		header.Set("Cookie", strings.Join(kept, "; "))
	}
	return removed
}
