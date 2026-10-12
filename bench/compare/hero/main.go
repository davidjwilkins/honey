// Command hero writes a photo-like JPEG, for the page speed comparison (see
// ../run.sh): a sky and hills in soft light, with fine grain, so that it
// compresses as a photo does, and browsers treat it as the page's main
// image.
package main

import (
	"image"
	"image/color"
	"image/jpeg"
	"log"
	"math"
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
	clamp := func(v float64) uint8 { return uint8(math.Max(0, math.Min(255, v))) }
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			fx, fy := float64(x)/width, float64(y)/height
			// The sun, and the light around it
			sun := math.Max(0, 1-math.Hypot(fx-0.72, fy-0.28)*2.5)
			red, green, blue := 70+110*fy+170*sun*sun, 120+60*fy+140*sun*sun, 210-90*fy+40*sun
			// Two ridges of hills
			if fy > 0.55+0.08*math.Sin(fx*9)+0.03*math.Sin(fx*31) {
				red, green, blue = 60+40*fx, 110-30*fy+20*math.Sin(fx*50+fy*20), 50
			}
			if fy > 0.75+0.05*math.Sin(fx*5+1) {
				red, green, blue = 40, 80+15*math.Sin(fx*80)*math.Sin(fy*60), 35
			}
			grain := float64(r.IntN(13) - 6)
			img.Set(x, y, color.RGBA{clamp(red + grain), clamp(green + grain), clamp(blue + grain), 255})
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
