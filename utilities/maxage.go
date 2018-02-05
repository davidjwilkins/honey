package utilities

import (
	"regexp"
	"strconv"
	"strings"
)

var smaxAgeFinder = regexp.MustCompile(`s-maxage=(?:\")?(\d+)(?:\")?(?:,|$)`)
var maxAgeFinder = regexp.MustCompile(`max-age=(?:\")?(\d+)(?:\")?(?:,|$)`)

func GetMaxAge(cacheControl string) (maxage int, exists bool) {
	var age string
	var err error
	if strings.Contains(cacheControl, "s-maxage") {
		// https://tools.ietf.org/html/rfc7234#section-5.2.2.8
		tmp := smaxAgeFinder.FindStringSubmatch(cacheControl)
		if len(tmp) == 2 {
			age = tmp[1]
			exists = true
		}
	} else if strings.Contains(cacheControl, "max-age") {
		// https://tools.ietf.org/html/rfc7234#section-5.2.2.9
		tmp := maxAgeFinder.FindStringSubmatch(cacheControl)
		if len(tmp) == 2 {
			age = tmp[1]
			exists = true
		}
	}
	if age != "" {
		maxage, err = strconv.Atoi(age)
		if err != nil {
			maxage = 0
		}
	}
	return
}
