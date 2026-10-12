// Command hero writes a photo-like JPEG (noisy, so that browsers treat it
// as a page's main image), for the page speed comparison: see ../run.sh.
package main

import (
	"image"
	"image/color"
	"image/jpeg"
	"log"
	"math/rand/v2"
	"os"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: hero file.jpg")
	}
	const width, height = 1600, 900
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	r := rand.New(rand.NewPCG(1, 2))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{uint8(x/7 + r.IntN(50)), uint8(y/4 + r.IntN(50)), uint8(120 + r.IntN(100)), 255})
		}
	}
	f, err := os.Create(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 82}); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}
}
