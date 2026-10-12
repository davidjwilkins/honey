package optimize

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReport(t *testing.T) {
	b := &backend{sheets: map[string]string{"/wp-content/themes/t/style.css": themeCSS}}
	o := &origin{files: map[string][]byte{"/wp-content/uploads/featured-1024.jpg": jpegOf(t, photo(500, 300, 50))}}
	opt := New(Options{DelayScripts: true, PreloadMainImage: true, RemoveUnusedCSS: true, ConvertImages: true, Fetch: b.fetch})
	h := opt.Middleware(o)

	// The page as it's first cached: its main image isn't known
	page := opt.Rewrite(response("/post/", wpPage))
	r := opt.Report("www.example.com", "/post/", page)
	require.NotNil(t, r)
	assert.Equal(t, []MainImageReport{{Screens: "phones", State: "asked"}, {Screens: "desktops", State: "asked"}}, r.MainImages)
	assert.Contains(t, r.DelayedScripts, "https://www.googletagmanager.com/gtag/js?id=G-ABC")
	assert.Contains(t, r.DelayedScripts, "inline (#fb-pixel)")
	require.Len(t, r.UsedCSS, 1)
	assert.Equal(t, "/wp-content/themes/t/style.css", r.UsedCSS[0].Href)
	assert.Equal(t, len(themeCSS), r.UsedCSS[0].Whole)
	assert.Less(t, r.UsedCSS[0].Used, r.UsedCSS[0].Whole)
	images := map[string]ImageReport{}
	for _, i := range r.Images {
		images[i.URL] = i
	}
	assert.Equal(t, "not seen", images["/wp-content/uploads/featured-1024.jpg"].State)
	assert.Equal(t, "not seen", images["/logo.png"].State)

	// Once browsers have reported its main image, and asked for its image
	for _, viewport := range []string{"desktop", "mobile"} {
		report(t, h, "/post/", viewport, "http://www.example.com/wp-content/uploads/featured-1024.jpg")
	}
	fetchImage(h, "/wp-content/uploads/featured-1024.jpg", chromeAccept)
	converted(t, h, "/wp-content/uploads/featured-1024.jpg", chromeAccept, "image/avif")
	r = opt.Report("www.example.com", "/post/", opt.Rewrite(response("/post/", wpPage)))
	assert.Equal(t, MainImageReport{Screens: "phones", State: "learned", URL: "/wp-content/uploads/featured-1024.jpg"}, r.MainImages[0])
	for _, i := range r.Images {
		images[i.URL] = i
	}
	featured := images["/wp-content/uploads/featured-1024.jpg"]
	assert.Equal(t, "converted", featured.State)
	assert.Equal(t, len(o.files["/wp-content/uploads/featured-1024.jpg"]), featured.Original)
	require.Len(t, featured.Converted, 1)
	assert.Equal(t, "avif", featured.Converted[0].Format)
	assert.Less(t, featured.Converted[0].Bytes, featured.Original)

	var none *Optimizer
	assert.Nil(t, none.Report("www.example.com", "/", page))
}
