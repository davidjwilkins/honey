package utilities

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
)

type CookieGetter interface {
	Cookie(name string) (*http.Cookie, error)
}

func GetVaryHeadersHash(headers http.Header, getCookie CookieGetter, allowedCookieNames []string, vary string) (hash string) {
	if vary == "" {
		return ""
	}
	var buffer bytes.Buffer
	varies := strings.Split(vary, ",")
	for _, header := range varies {
		if header != "cookie" {
			buffer.WriteString("::")
			buffer.WriteString(headers.Get(header))
		} else {
			for _, cookieName := range allowedCookieNames {
				if cookie, err := getCookie.Cookie(cookieName); err == nil {
					buffer.WriteString(fmt.Sprintf(
						" :: %s :: %v",
						strings.Replace(cookie.Name, "::", "::::", -1),
						strings.Replace(cookie.Value, "::", "::::", -1),
					))
				} else {
					buffer.WriteString(fmt.Sprintf(
						" :: %s :: ",
						strings.Replace(cookieName, "::", "::::", -1),
					))
				}
			}
		}
	}

	return buffer.String()
}
