//go:build 386 || arm || mips || mipsle

package optimize

import (
	"errors"
	goimage "image"
	"io"
)

// The WebP encoder (webp-go-pure) needs 64 bits: on 32 bit systems, images
// are only converted to AVIF

var errNoWebP = errors.New("WebP isn't supported on 32 bit systems")

func encodeWebP(w io.Writer, img goimage.Image, lossless bool, quality, effort int) error {
	return errNoWebP
}

func webpConfig(r io.Reader) (goimage.Config, error) {
	return goimage.Config{}, errNoWebP
}
