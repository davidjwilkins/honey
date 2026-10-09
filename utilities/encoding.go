package utilities

import (
	"mime"
	"strconv"
	"strings"
)

// AcceptsBrotli returns whether a request with the given Accept-Encoding
// header accepts a brotli ("br") encoded response
// (https://www.rfc-editor.org/rfc/rfc9110#section-12.5.3).
func AcceptsBrotli(acceptEncoding string) bool {
	star := false
	for _, part := range strings.Split(acceptEncoding, ",") {
		coding, params, _ := strings.Cut(part, ";")
		coding = strings.ToLower(strings.TrimSpace(coding))
		q := 1.0
		for _, param := range strings.Split(params, ";") {
			name, value, _ := strings.Cut(strings.TrimSpace(param), "=")
			if strings.EqualFold(name, "q") {
				if parsed, err := strconv.ParseFloat(value, 64); err == nil {
					q = parsed
				}
			}
		}
		switch coding {
		case "br":
			return q > 0
		case "*":
			star = q > 0
		}
	}
	return star
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
