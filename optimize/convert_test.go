package optimize

import (
	"bytes"
	"encoding/binary"
	goimage "image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	webp "github.com/SeriousBug/webp-go-pure/std"
	avif "github.com/gen2brain/gav1d/avif"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// photo returns a photo-like picture: smooth gradients and shapes, with a
// little grain
func photo(w, h int, seed uint64) *goimage.NRGBA {
	img := goimage.NewNRGBA(goimage.Rect(0, 0, w, h))
	r := rand.New(rand.NewPCG(seed, 1))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			fx, fy := float64(x)/float64(w), float64(y)/float64(h)
			sun := math.Max(0, 1-math.Hypot(fx-0.7, fy-0.3)*3)
			c := color.NRGBA{
				R: uint8(math.Min(255, 60+120*fy+150*sun+float64(r.IntN(8)))),
				G: uint8(math.Min(255, 90+80*fx+120*sun+float64(r.IntN(8)))),
				B: uint8(math.Min(255, 180-100*fy+40*sun+float64(r.IntN(8)))),
				A: 255,
			}
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

// screenshot returns a picture like a screenshot: lines of text, and a
// sidebar
func screenshot(w, h int) *goimage.NRGBA {
	img := goimage.NewNRGBA(goimage.Rect(0, 0, w, h))
	r := rand.New(rand.NewPCG(3, 4))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.NRGBA{255, 255, 255, 255}
			switch {
			case x < 120:
				c = color.NRGBA{40, 60, 120, 255}
			case y%24 >= 6 && y%24 < 18 && x%9 < 7 && r.IntN(3) == 0:
				c = color.NRGBA{30, 30, 40, 255}
			}
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

func jpegOf(t *testing.T, img goimage.Image) []byte {
	var b bytes.Buffer
	require.NoError(t, jpeg.Encode(&b, img, &jpeg.Options{Quality: 90}))
	return b.Bytes()
}

func pngOf(t *testing.T, img goimage.Image) []byte {
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, img))
	return b.Bytes()
}

// withSegment returns a JPEG with an APPn segment added after its start
func withSegment(j []byte, marker byte, data []byte) []byte {
	seg := []byte{0xFF, marker, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(data)+2))
	return append(append(append([]byte{}, j[:2]...), append(seg, data...)...), j[2:]...)
}

// rotated is EXIF saying an image is rotated 90 degrees
func rotated() []byte {
	tiff := []byte("MM\x00\x2a\x00\x00\x00\x08\x00\x01\x01\x12\x00\x03\x00\x00\x00\x01\x00\x06\x00\x00\x00\x00\x00\x00")
	return append([]byte("Exif\x00\x00"), tiff...)
}

// iccProfile is an ICC profile (enough of one) described as desc
func iccProfile(desc string) []byte {
	p := make([]byte, 128)
	copy(p[16:], "RGB ")
	copy(p[36:], "acsp")
	return append(p, []byte("desc"+desc)...)
}

// origin serves images, and counts the requests for them
type origin struct {
	files    map[string][]byte
	headers  map[string]http.Header
	requests atomic.Int32
}

func (o *origin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.requests.Add(1)
	body, ok := o.files[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	for name, values := range o.headers[r.URL.Path] {
		w.Header()[name] = values
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "image/"+imageKind(r.URL.Path))
	}
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "public, max-age=31536000")
	}
	w.Header().Set("ETag", `"original"`)
	http.ServeContent(w, r, "", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), bytes.NewReader(body))
}

func converter(t *testing.T, o *origin, clock *clock) (*Optimizer, http.Handler) {
	opts := Options{ConvertImages: true, PrivateCookies: []string{"wordpress_logged_in_"}}
	if clock != nil {
		opts.now = clock.now
	}
	opt := New(opts)
	require.NotNil(t, opt)
	return opt, opt.Middleware(o)
}

func fetchImage(h http.Handler, path, accept string, headers ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "http://www.example.com"+path, nil)
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const (
	chromeAccept = "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8"
	webpAccept   = "image/webp,*/*"
)

// converted waits for path to be served as contentType
func converted(t *testing.T, h http.Handler, path, accept, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	var w *httptest.ResponseRecorder
	require.Eventually(t, func() bool {
		w = fetchImage(h, path, accept)
		return w.Header().Get("Content-Type") == contentType
	}, 10*time.Second, 10*time.Millisecond, "%s was never served as %s", path, contentType)
	return w
}

func TestConvertJPEG(t *testing.T) {
	src := photo(600, 400, 1)
	o := &origin{files: map[string][]byte{"/wp-content/uploads/hero.jpg": jpegOf(t, src)}}
	_, h := converter(t, o, nil)
	original := o.files["/wp-content/uploads/hero.jpg"]

	first := fetchImage(h, "/wp-content/uploads/hero.jpg", chromeAccept)
	assert.Equal(t, "image/jpeg", first.Header().Get("Content-Type"), "the first browser gets the original, as it's converted")
	assert.Equal(t, original, first.Body.Bytes())
	assert.Equal(t, "Accept", first.Header().Get("Vary"))

	w := converted(t, h, "/wp-content/uploads/hero.jpg", chromeAccept, "image/avif")
	assert.Equal(t, "Accept", w.Header().Get("Vary"))
	assert.Equal(t, "avif", w.Header().Get(ConvertedHeader))
	assert.Equal(t, "public, max-age=31536000", w.Header().Get("Cache-Control"), "with the original's caching")
	assert.Less(t, w.Body.Len(), len(original)*9/10)
	img, err := avif.Decode(bytes.NewReader(w.Body.Bytes()))
	require.NoError(t, err)
	assert.Equal(t, src.Bounds(), img.Bounds())
	etag := w.Header().Get("ETag")
	assert.Contains(t, etag, "honey-avif-")

	// Again, conditionally, and for its headers alone
	assert.Equal(t, http.StatusNotModified, fetchImage(h, "/wp-content/uploads/hero.jpg", chromeAccept, "If-None-Match", etag).Code)
	head := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "http://www.example.com/wp-content/uploads/hero.jpg", nil)
	req.Header.Set("Accept", chromeAccept)
	h.ServeHTTP(head, req)
	assert.Equal(t, "image/avif", head.Header().Get("Content-Type"))
	assert.Zero(t, head.Body.Len())

	// Browsers which take WebP but not AVIF get WebP, once it's converted
	assert.Equal(t, "image/jpeg", fetchImage(h, "/wp-content/uploads/hero.jpg", webpAccept).Header().Get("Content-Type"))
	w = converted(t, h, "/wp-content/uploads/hero.jpg", webpAccept, "image/webp")
	_, err = webp.Decode(bytes.NewReader(w.Body.Bytes()))
	require.NoError(t, err)

	// and those which take neither, the original
	plain := fetchImage(h, "/wp-content/uploads/hero.jpg", "image/png,image/*")
	assert.Equal(t, "image/jpeg", plain.Header().Get("Content-Type"))
	assert.Equal(t, "Accept", plain.Header().Get("Vary"))

	requests := o.requests.Load()
	fetchImage(h, "/wp-content/uploads/hero.jpg", chromeAccept)
	assert.Equal(t, requests, o.requests.Load(), "converted images are served by Honey")
}

func TestConvertPNGLosslessly(t *testing.T) {
	src := screenshot(600, 400)
	o := &origin{files: map[string][]byte{"/logo.png": pngOf(t, src)}}
	_, h := converter(t, o, nil)
	fetchImage(h, "/logo.png", chromeAccept)
	w := converted(t, h, "/logo.png", chromeAccept, "image/webp")
	img, err := webp.Decode(bytes.NewReader(w.Body.Bytes()))
	require.NoError(t, err)
	for _, p := range []goimage.Point{{0, 0}, {100, 50}, {300, 200}, {599, 399}} {
		assert.Equal(t, src.At(p.X, p.Y), color.NRGBAModel.Convert(img.At(p.X, p.Y)), "it's exactly the same")
	}
}

func TestConvertingAConditionalRequest(t *testing.T) {
	o := &origin{files: map[string][]byte{"/a.jpg": jpegOf(t, photo(400, 300, 3))}}
	_, h := converter(t, o, nil)
	// A browser which has the original cached already
	w := fetchImage(h, "/a.jpg", chromeAccept, "If-None-Match", `"original"`)
	assert.Equal(t, http.StatusOK, w.Code, "it gets it again, so that it can be converted")
	converted(t, h, "/a.jpg", chromeAccept, "image/avif")
}

func TestImagesWhichArentConverted(t *testing.T) {
	plain := jpegOf(t, photo(400, 300, 4))
	animated := pngOf(t, photo(200, 100, 5))
	// An acTL chunk, after the IHDR (8 + 25 bytes in)
	actl := []byte{0, 0, 0, 8, 'a', 'c', 'T', 'L', 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0}
	animated = append(append(append([]byte{}, animated[:33]...), actl...), animated[33:]...)
	o := &origin{
		files: map[string][]byte{
			"/rotated.jpg":     withSegment(plain, 0xE1, rotated()),
			"/p3.jpg":          withSegment(plain, 0xE2, append([]byte("ICC_PROFILE\x00\x01\x01"), iccProfile("Display P3")...)),
			"/srgb.jpg":        withSegment(plain, 0xE2, append([]byte("ICC_PROFILE\x00\x01\x01"), iccProfile("sRGB IEC61966-2.1")...)),
			"/animated.png":    animated,
			"/grainy.png":      pngOf(t, photo(300, 200, 1)),
			"/tiny.jpg":        jpegOf(t, photo(8, 8, 6)),
			"/cookie.jpg":      plain,
			"/private.jpg":     plain,
			"/varies.jpg":      plain,
			"/logged-in.jpg":   plain,
			"/range.jpg":       plain,
			"/not-a-jpeg.jpg":  []byte(strings.Repeat("<html>", 1000)),
			"/as-html.jpg":     plain,
			"/compressed.jpeg": plain,
			"/query.jpg":       plain,
		},
		headers: map[string]http.Header{
			"/cookie.jpg":      {"Set-Cookie": {"a=b"}},
			"/private.jpg":     {"Cache-Control": {"private, max-age=60"}},
			"/varies.jpg":      {"Vary": {"Cookie"}},
			"/as-html.jpg":     {"Content-Type": {"text/html"}},
			"/compressed.jpeg": {"Content-Encoding": {"gzip"}},
		},
	}
	opt, h := converter(t, o, nil)
	url := func(path string) string {
		if path == "/query.jpg" {
			return path + "?ver=2"
		}
		return path
	}
	headers := func(path string) []string {
		switch path {
		case "/logged-in.jpg":
			return []string{"Cookie", "wordpress_logged_in_abc=admin"}
		case "/range.jpg":
			return []string{"Range", "bytes=0-99"}
		}
		return nil
	}
	for path := range o.files {
		fetchImage(h, url(path), chromeAccept, headers(path)...)
	}
	// Once whatever could be converted has been
	converted(t, h, "/srgb.jpg", chromeAccept, "image/avif")
	require.Eventually(t, func() bool {
		opt.convert.mu.Lock()
		defer opt.convert.mu.Unlock()
		for _, e := range opt.convert.images {
			if len(e.pending) > 0 {
				return false
			}
		}
		return true
	}, 10*time.Second, 10*time.Millisecond)
	for path := range o.files {
		if path == "/srgb.jpg" {
			continue
		}
		w := fetchImage(h, url(path), chromeAccept, headers(path)...)
		assert.Empty(t, w.Header().Get(ConvertedHeader), path)
	}
	opt.convert.mu.Lock()
	defer opt.convert.mu.Unlock()
	reasons := map[string]string{}
	for k, e := range opt.convert.images {
		reasons[k[strings.IndexByte(k, '/'):]] = e.none["avif"] + e.none["webp"]
	}
	assert.Contains(t, reasons["/rotated.jpg"], "rotated")
	assert.Contains(t, reasons["/p3.jpg"], "isn't sRGB")
	assert.Contains(t, reasons["/animated.png"], "animated")
	assert.Contains(t, reasons["/not-a-jpeg.jpg"], "isn't a JPEG")
	assert.Contains(t, reasons["/grainy.png"], "isn't smaller")
	// Responses which can't be converted aren't fetched to be again (until
	// they're checked again)
	for _, path := range []string{"/tiny.jpg", "/cookie.jpg", "/private.jpg", "/varies.jpg", "/as-html.jpg", "/compressed.jpeg"} {
		assert.Contains(t, reasons[path], "can't be converted", path)
	}
	// and requests which can't be converted from aren't even remembered
	for _, path := range []string{"/logged-in.jpg", "/range.jpg", "/query.jpg"} {
		_, kept := reasons[path]
		assert.False(t, kept, "%s isn't remembered", path)
	}
}

func TestConvertedImagesAreCheckedAgainstTheOriginal(t *testing.T) {
	clock := &clock{t: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)}
	o := &origin{files: map[string][]byte{"/a.jpg": jpegOf(t, photo(400, 300, 7))}}
	_, h := converter(t, o, clock)
	fetchImage(h, "/a.jpg", chromeAccept)
	before := converted(t, h, "/a.jpg", chromeAccept, "image/avif").Body.Bytes()

	// A day later, the image has changed
	o.files["/a.jpg"] = jpegOf(t, photo(400, 300, 8))
	clock.t = clock.t.Add(25 * time.Hour)
	w := fetchImage(h, "/a.jpg", chromeAccept)
	assert.Equal(t, "image/jpeg", w.Header().Get("Content-Type"), "one request gets the original, to check it")
	after := converted(t, h, "/a.jpg", chromeAccept, "image/avif").Body.Bytes()
	assert.NotEqual(t, before, after, "and the new one is converted")

	// and then it's gone
	clock.t = clock.t.Add(25 * time.Hour)
	delete(o.files, "/a.jpg")
	assert.Equal(t, http.StatusNotFound, fetchImage(h, "/a.jpg", chromeAccept).Code)
	assert.Equal(t, http.StatusNotFound, fetchImage(h, "/a.jpg", chromeAccept).Code)
}

func TestConvertedImagesAreForgottenWhenPurged(t *testing.T) {
	o := &origin{files: map[string][]byte{"/wp-content/uploads/a.jpg": jpegOf(t, photo(400, 300, 9)), "/b.jpg": jpegOf(t, photo(400, 300, 10))}}
	opt, h := converter(t, o, nil)
	for _, p := range []string{"/wp-content/uploads/a.jpg", "/b.jpg"} {
		fetchImage(h, p, chromeAccept)
		converted(t, h, p, chromeAccept, "image/avif")
	}
	opt.Forget("/wp-content/uploads/", true)
	assert.Equal(t, "image/jpeg", fetchImage(h, "/wp-content/uploads/a.jpg", chromeAccept).Header().Get("Content-Type"))
	assert.Equal(t, "image/avif", fetchImage(h, "/b.jpg", chromeAccept).Header().Get("Content-Type"))
	opt.Forget("/b.jpg", false)
	assert.Equal(t, "image/jpeg", fetchImage(h, "/b.jpg", chromeAccept).Header().Get("Content-Type"))
}

func TestConvertedImagesAreBounded(t *testing.T) {
	o := &origin{files: map[string][]byte{}}
	for i := 0; i < 6; i++ {
		o.files["/"+string(rune('a'+i))+".jpg"] = jpegOf(t, photo(300, 200, uint64(20+i)))
	}
	opt := New(Options{ConvertImages: true, ConvertedImagesBytes: 1})
	h := opt.Middleware(o)
	for p := range o.files {
		fetchImage(h, p, chromeAccept)
	}
	// Once they've all been converted, only the last is kept (those being
	// converted aren't forgotten)
	require.Eventually(t, func() bool {
		opt.convert.mu.Lock()
		defer opt.convert.mu.Unlock()
		return opt.convert.converted == len(o.files) && len(opt.convert.images) == 1
	}, 20*time.Second, 10*time.Millisecond)
	opt.convert.mu.Lock()
	defer opt.convert.mu.Unlock()
	for _, e := range opt.convert.images {
		assert.Equal(t, opt.convert.bytes, e.size())
	}
}

func TestCheckJPEGAndPNG(t *testing.T) {
	plain := jpegOf(t, photo(40, 30, 1))
	assert.NoError(t, checkJPEG(plain))
	assert.ErrorContains(t, checkJPEG(withSegment(plain, 0xE1, rotated())), "rotated")
	assert.NoError(t, checkJPEG(withSegment(plain, 0xE1, append([]byte("Exif\x00\x00"), "II\x2a\x00\x08\x00\x00\x00\x01\x00\x12\x01\x03\x00\x01\x00\x00\x00\x01\x00\x00\x00"...))), "orientation 1, little endian")
	assert.ErrorContains(t, checkJPEG(withSegment(plain, 0xE2, append([]byte("ICC_PROFILE\x00\x01\x01"), iccProfile("Adobe RGB (1998)")...))), "isn't sRGB")
	cmyk := iccProfile("U.S. Web Coated")
	copy(cmyk[16:], "CMYK")
	assert.ErrorContains(t, checkJPEG(withSegment(plain, 0xE2, append([]byte("ICC_PROFILE\x00\x01\x01"), cmyk...))), "CMYK")
	assert.Error(t, checkJPEG([]byte("not a jpeg")))
	assert.NoError(t, checkPNG(pngOf(t, photo(20, 20, 1))))
	assert.Error(t, checkPNG([]byte("not a png")))
}

func TestConcurrentRequestsForAnImage(t *testing.T) {
	o := &origin{files: map[string][]byte{"/a.jpg": jpegOf(t, photo(400, 300, 30))}}
	opt, h := converter(t, o, nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := fetchImage(h, "/a.jpg", chromeAccept)
			assert.Equal(t, http.StatusOK, w.Code)
		}()
	}
	wg.Wait()
	converted(t, h, "/a.jpg", chromeAccept, "image/avif")
	opt.convert.mu.Lock()
	defer opt.convert.mu.Unlock()
	assert.Equal(t, 1, opt.convert.converted, "it's converted once")
}
