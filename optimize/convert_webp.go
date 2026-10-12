//go:build !386 && !arm && !mips && !mipsle

package optimize

import (
	goimage "image"
	"io"

	webp "github.com/SeriousBug/webp-go-pure/std"
)

// encodeWebP writes img as WebP: lossless, or at quality and effort
func encodeWebP(w io.Writer, img goimage.Image, lossless bool, quality, effort int) error {
	return webp.Encode(w, img, &webp.Options{Lossless: lossless, Quality: quality, Effort: effort})
}

// webpConfig returns a WebP image's dimensions
func webpConfig(r io.Reader) (goimage.Config, error) {
	return webp.DecodeConfig(r)
}
