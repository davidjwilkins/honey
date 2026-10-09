package utilities

import (
	"mime"
	"strconv"
	"strings"
)

// PreferredEncoding returns which of the available content codings
// (e.g. "br", "gzip") a request with the given Accept-Encoding header
// prefers, or "" if it prefers the unencoded response
// (https://www.rfc-editor.org/rfc/rfc9110#section-12.5.3).  Codings are
// chosen by their q-value; ties go to the earliest available coding.  A
// coding the client lists always beats the unencoded response unless the
// client lists "identity" too, as clients list the codings they want.
func PreferredEncoding(acceptEncoding string, available ...string) string {
	qualities := map[string]float64{}
	for _, part := range strings.Split(acceptEncoding, ",") {
		coding, params, _ := strings.Cut(part, ";")
		coding = strings.ToLower(strings.TrimSpace(coding))
		if coding == "" {
			continue
		}
		q := 1.0
		for _, param := range strings.Split(params, ";") {
			name, value, _ := strings.Cut(strings.TrimSpace(param), "=")
			if strings.EqualFold(name, "q") {
				if parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
					q = parsed
				}
			}
		}
		qualities[coding] = q
	}
	quality := func(coding string) float64 {
		if q, found := qualities[coding]; found {
			return q
		}
		if q, found := qualities["*"]; found {
			return q
		}
		return 0
	}
	best, bestQ := "", 0.0
	if q, found := qualities["identity"]; found {
		bestQ = q
	}
	for _, coding := range available {
		if q := quality(coding); q > 0 && q > bestQ {
			best, bestQ = coding, q
		}
	}
	return best
}

// CacheVary returns the Vary header value to key cached responses on.  It
// leaves out Accept-Encoding: the cache stores responses unencoded, and
// encodes them for each client itself.
func CacheVary(vary string) string {
	var kept []string
	for _, header := range strings.Split(vary, ",") {
		if header = strings.TrimSpace(header); header != "" && !strings.EqualFold(header, "Accept-Encoding") {
			kept = append(kept, header)
		}
	}
	return strings.Join(kept, ",")
}

var compressibleTypes = map[string]bool{
	"application/javascript":        true,
	"application/x-javascript":      true,
	"application/ecmascript":        true,
	"application/json":              true,
	"application/xml":               true,
	"application/wasm":              true,
	"application/vnd.ms-fontobject": true,
	"image/svg+xml":                 true,
	"image/x-icon":                  true,
	"image/vnd.microsoft.icon":      true,
	"font/ttf":                      true,
	"font/otf":                      true,
}

// IsCompressible returns whether content with the given Content-Type is
// worth compressing - text, scripts, data and uncompressed fonts, but not
// images, video or other formats which are already compressed.
func IsCompressible(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return strings.HasPrefix(mediaType, "text/") ||
		strings.HasSuffix(mediaType, "+json") ||
		strings.HasSuffix(mediaType, "+xml") ||
		compressibleTypes[mediaType]
}
