package optimize

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	goimage "image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/davidjwilkins/honey/events"
	avif "github.com/gen2brain/gav1d/avif"
)

// Converting images: AVIF and WebP images are a fraction of the size of
// JPEGs and PNGs - AVIF often half the size of a JPEG, or less - which
// shows a page's images, and its Largest Contentful Paint, sooner.  So a
// JPEG or PNG requested by a browser which accepts AVIF or WebP (as its
// Accept header says) is converted in the background, from the response
// which passes through Honey, and later requests for it get the converted
// image, at the same URL (with Vary: Accept, so that caches keep them
// apart).  JPEGs become AVIF, for browsers which accept it, or else WebP;
// PNGs become lossless WebP (which keeps them exactly as they are, sharp
// edges and all).
//
// Images whose colours or orientation would change are left alone: JPEGs
// rotated by their EXIF orientation, with colour profiles other than sRGB
// (e.g. an iPhone's Display P3) or in CMYK, and animated PNGs.  So are
// those which aren't smaller once converted, and those which are tiny,
// huge, or (in a request with a logged in user's cookies) perhaps private.
// A converted image is checked against the original daily, and forgotten
// when its URL is purged.

// ConvertedHeader says which format an image was converted to
const ConvertedHeader = "X-Honey-Image"

const (
	// minConvertBytes and maxConvertBytes bound the size of images which
	// are converted
	minConvertBytes = 2 << 10
	maxConvertBytes = 16 << 20
	// maxConvertPixels and maxLosslessPixels bound their dimensions (as
	// converting takes memory with them: lossless WebP, about 60 bytes a
	// pixel)
	maxConvertPixels  = 40_000_000
	maxLosslessPixels = 2_500_000
	// convertQueue is how many images may wait to be converted
	convertQueue = 256
	// maxImageEntries bounds how many images are remembered, converted or
	// not
	maxImageEntries = 50_000
	// convertRecheck is how long a converted image is served before it's
	// checked against the original
	convertRecheck = 24 * time.Hour
	// DefaultConvertedBytes is how much memory converted images take, by
	// default
	DefaultConvertedBytes = 256 << 20
	// The quality images are converted at: AVIF's 70 and WebP's 82 look
	// as good as the JPEG they come from
	avifQuality   = 70
	avifSpeed     = 8
	webpQuality   = 82
	webpEffort    = 4
	losslessLevel = 2
	// minSaving is how much smaller a converted image must be to be served
	// rather than the original
	minSaving = 0.1
)

// imageFormats are the formats images are converted to, in the order
// they're preferred
var imageFormats = []struct{ name, contentType string }{
	{"avif", "image/avif"},
	{"webp", "image/webp"},
}

// imageConverter converts images, and keeps them converted
type imageConverter struct {
	opts *Options
	jobs chan convertJob
	max  int

	mu     sync.Mutex
	images map[string]*imageEntry
	bytes  int
	// converted, before and after count the images converted, and their
	// sizes before and after, for the admin page's events
	converted, before, after int
}

// imageEntry is what's known of an image: the copies converted, and those
// which couldn't be
type imageEntry struct {
	// validator identifies the original (a hash of it), and original is
	// its size
	validator uint64
	original  int
	// checked is when the original was last seen, and checking whether
	// a request is fetching it to check it again
	checked  time.Time
	checking bool
	// header is the original's headers to send with converted copies
	header http.Header
	// formats are the converted copies, pending those being converted,
	// and none those which can't be (and why)
	formats map[string]*convertedImage
	pending map[string]bool
	none    map[string]string
}

type convertedImage struct {
	body        []byte
	contentType string
	etag        string
	modified    time.Time
}

// size is how much memory e's converted copies take
func (e *imageEntry) size() int {
	n := 0
	for _, c := range e.formats {
		n += len(c.body)
	}
	return n
}

type convertJob struct {
	key, format, kind string
	body              []byte
	validator         uint64
}

func newImageConverter(opts *Options) *imageConverter {
	c := &imageConverter{opts: opts, jobs: make(chan convertJob, convertQueue), max: opts.ConvertedImagesBytes, images: map[string]*imageEntry{}}
	if c.max <= 0 {
		c.max = DefaultConvertedBytes
	}
	// Converting takes a core each: a quarter of them, so that serving
	// isn't held up
	workers := runtime.NumCPU() / 4
	if workers < 1 {
		workers = 1
	}
	if workers > 4 {
		workers = 4
	}
	for i := 0; i < workers; i++ {
		go c.work()
	}
	return c
}

// imageKind returns "jpeg" or "png" for a path to a JPEG or PNG, or ""
func imageKind(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".jpg", ".jpeg", ".jpe":
		return "jpeg"
	case ".png":
		return "png"
	}
	return ""
}

// wanted returns the formats a request accepts which an image of kind can
// be converted to, preferred first
func wanted(kind, accept string) []string {
	accept = strings.ToLower(accept)
	var formats []string
	for _, f := range imageFormats {
		if kind == "png" && f.name != "webp" {
			// PNGs are converted to lossless WebP only (AVIF's lossless
			// mode makes bigger files than PNG)
			continue
		}
		if strings.Contains(accept, f.contentType) {
			formats = append(formats, f.name)
		}
	}
	return formats
}

// serveImage serves a converted copy of the JPEG or PNG r asks for, if
// there is one; or passes r on to next, converting the image it gets if
// it can.  It returns false if r isn't a request for an image.
func (c *imageConverter) serveImage(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
	kind := imageKind(r.URL.Path)
	if kind == "" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return false
	}
	// Caches have to keep the converted and original images apart
	w.Header().Add("Vary", "Accept")
	formats := wanted(kind, r.Header.Get("Accept"))
	if len(formats) == 0 || r.Header.Get("Range") != "" || r.URL.RawQuery != "" {
		// (Images with query strings aren't converted, so that there can't
		// be any number of copies of an image)
		next.ServeHTTP(w, r)
		return true
	}
	key := strings.ToLower(r.Host) + r.URL.Path
	// Whether this request may fetch the original for converting: one at
	// a time, and not a logged in user's (who may get what others mayn't)
	mayFetch := r.Method == http.MethodGet && !private(r, c.opts.PrivateCookies)
	now := c.opts.now()

	c.mu.Lock()
	e := c.images[key]
	var serve *convertedImage
	var header http.Header
	convert := ""
	fetch, stale := false, false
	if e == nil {
		if mayFetch {
			if len(c.images) >= maxImageEntries {
				c.evictEntries()
			}
			e = &imageEntry{formats: map[string]*convertedImage{}, pending: map[string]bool{}, none: map[string]string{}, checking: true}
			c.images[key] = e
			convert, fetch = formats[0], true
		}
	} else {
		for _, f := range formats {
			if img := e.formats[f]; img != nil {
				serve, header = img, e.header
				break
			}
		}
		stale = now.Sub(e.checked) >= convertRecheck
		if mayFetch && !e.checking {
			for _, f := range formats {
				if e.formats[f] == nil && e.none[f] == "" && !e.pending[f] {
					convert = f
					break
				}
			}
			if stale || (serve == nil && convert != "") {
				// This request fetches the original: to check it hasn't
				// changed, and to convert it to what's missing
				e.checking, fetch = true, true
			}
		}
	}
	c.mu.Unlock()

	switch {
	case serve != nil && !(fetch && stale):
		serveConverted(w, r, serve, header)
		return true
	case !fetch:
		next.ServeHTTP(w, r)
		return true
	}
	// The whole image is wanted, even if the browser has it already (it's
	// sent it anyway)
	r = r.Clone(r.Context())
	for _, name := range []string{"If-None-Match", "If-Modified-Since", "If-Match", "If-Unmodified-Since", "If-Range"} {
		r.Header.Del(name)
	}
	cw := &captureWriter{ResponseWriter: w}
	next.ServeHTTP(cw, r)
	body, ok := cw.captured(kind)
	c.captured(key, kind, convert, body, ok, cw.status, cw.Header())
	return true
}

// private returns whether r has the cookies of a visitor (e.g. a logged in
// user) who may be shown what others aren't, or credentials
func private(r *http.Request, cookies []string) bool {
	if r.Header.Get("Authorization") != "" {
		return true
	}
	for _, cookie := range r.Cookies() {
		for _, prefix := range cookies {
			if strings.HasPrefix(cookie.Name, prefix) {
				return true
			}
		}
	}
	return false
}

// captured records that the original image key (of kind) has been fetched,
// with status: body, if ok, with header; and converts it to format, if
// that's set
func (c *imageConverter) captured(key, kind, format string, body []byte, ok bool, status int, header http.Header) {
	now := c.opts.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.images[key]
	if e == nil {
		// Forgotten meanwhile
		return
	}
	e.checking = false
	if !ok {
		switch {
		case status == http.StatusOK:
			// It can't be converted (e.g. it's private, or too big): it
			// isn't tried again until it's checked again
			c.bytes -= e.size()
			e.formats, e.validator, e.checked = map[string]*convertedImage{}, 0, now
			for _, f := range imageFormats {
				e.none[f.name] = "the original can't be converted"
			}
		case status == http.StatusNotFound || status == http.StatusGone || e.checked.IsZero():
			// It's gone, or it's new and couldn't be fetched (it's tried
			// again next time)
			c.bytes -= e.size()
			delete(c.images, key)
		}
		return
	}
	validator := hashBytes(body)
	if e.validator != validator {
		// New, or changed: what was converted from it is forgotten
		c.bytes -= e.size()
		e.validator, e.formats, e.pending, e.none = validator, map[string]*convertedImage{}, map[string]bool{}, map[string]string{}
	}
	e.original = len(body)
	e.checked = now
	e.header = imageHeader(header)
	if format == "" || e.formats[format] != nil || e.pending[format] || e.none[format] != "" {
		return
	}
	select {
	case c.jobs <- convertJob{key: key, format: format, kind: kind, body: bytes.Clone(body), validator: validator}:
		e.pending[format] = true
	default:
		// Too many waiting: it's converted another time
	}
}

// evictEntries forgets images, those not converted first, to make room for
// another
func (c *imageConverter) evictEntries() {
	n := 0
	for k, e := range c.images {
		if len(e.formats) == 0 && len(e.pending) == 0 && !e.checking {
			delete(c.images, k)
			if n++; n >= maxImageEntries/10 {
				return
			}
		}
	}
	for k, e := range c.images {
		if len(e.pending) == 0 && !e.checking {
			c.bytes -= e.size()
			delete(c.images, k)
			if n++; n >= maxImageEntries/10 {
				return
			}
		}
	}
}

// imageHeader returns the headers of an original image to send with
// converted copies of it
func imageHeader(h http.Header) http.Header {
	out := http.Header{}
	for _, name := range []string{"Cache-Control", "Expires", "Last-Modified", "Access-Control-Allow-Origin", "Timing-Allow-Origin"} {
		if v := h.Values(name); len(v) > 0 {
			out[name] = append([]string(nil), v...)
		}
	}
	return out
}

// serveConverted responds with img, a copy of an image whose original had
// header
func serveConverted(w http.ResponseWriter, r *http.Request, img *convertedImage, header http.Header) {
	h := w.Header()
	for name, values := range header {
		h[name] = values
	}
	h.Set("Content-Type", img.contentType)
	h.Set("ETag", img.etag)
	h.Set(ConvertedHeader, img.contentType[len("image/"):])
	h.Set("X-Content-Type-Options", "nosniff")
	h.Del("Last-Modified")
	http.ServeContent(w, r, "", img.modified, bytes.NewReader(img.body))
}

// work converts images
func (c *imageConverter) work() {
	for job := range c.jobs {
		img, err := convertImage(job.kind, job.format, job.body)
		c.done(job, img, err)
	}
}

// done records a converted image (or why it couldn't be)
func (c *imageConverter) done(job convertJob, body []byte, err error) {
	c.mu.Lock()
	e := c.images[job.key]
	if e == nil || e.validator != job.validator {
		// Forgotten, or changed, meanwhile
		c.mu.Unlock()
		return
	}
	delete(e.pending, job.format)
	if err != nil {
		e.none[job.format] = err.Error()
		c.mu.Unlock()
		return
	}
	modified := c.opts.now()
	if t, err := http.ParseTime(e.header.Get("Last-Modified")); err == nil {
		modified = t
	}
	var contentType string
	for _, f := range imageFormats {
		if f.name == job.format {
			contentType = f.contentType
		}
	}
	e.formats[job.format] = &convertedImage{body: body, contentType: contentType, modified: modified,
		etag: fmt.Sprintf(`"honey-%s-%x"`, job.format, hashBytes(body))}
	c.bytes += len(body)
	c.converted++
	c.before += len(job.body)
	c.after += len(body)
	converted, before, after := c.converted, c.before, c.after
	if c.bytes > c.max {
		c.evict(job.key)
	}
	c.mu.Unlock()

	c.opts.Events.Add(events.Info, "convert-images", 10*time.Minute, events.Text(fmt.Sprintf(
		"converted %d %s to AVIF or WebP so far: %s to %s (%.0f%% smaller)", converted, plural(converted, "image", "images"),
		kilobytes(before), kilobytes(after), 100*(1-float64(after)/float64(before)))))
}

// evict forgets images until those kept are within c.max, other than keep
func (c *imageConverter) evict(keep string) {
	for k, e := range c.images {
		if c.bytes <= c.max {
			return
		}
		if k == keep || len(e.pending) > 0 {
			continue
		}
		c.bytes -= e.size()
		delete(c.images, k)
	}
}

// forget forgets the converted copies of the image at path (on any host),
// or of every image under it if prefix
func (c *imageConverter) forget(p string, prefix bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.images {
		uri := k[strings.IndexByte(k, '/'):]
		if uri == p || (prefix && strings.HasPrefix(uri, p)) {
			c.bytes -= e.size()
			delete(c.images, k)
		}
	}
}

// convertImage converts body, a JPEG or PNG, to format
func convertImage(kind, format string, body []byte) ([]byte, error) {
	var decode func(io.Reader) (goimage.Image, error)
	var decodeConfig func(io.Reader) (goimage.Config, error)
	switch kind {
	case "jpeg":
		if err := checkJPEG(body); err != nil {
			return nil, err
		}
		decode, decodeConfig = jpeg.Decode, jpeg.DecodeConfig
	case "png":
		if err := checkPNG(body); err != nil {
			return nil, err
		}
		decode, decodeConfig = png.Decode, png.DecodeConfig
	default:
		return nil, errors.New("not a JPEG or PNG")
	}
	config, err := decodeConfig(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	pixels := config.Width * config.Height
	if pixels > maxConvertPixels || (kind == "png" && pixels > maxLosslessPixels) {
		return nil, fmt.Errorf("it's too big (%dx%d)", config.Width, config.Height)
	}
	img, err := decode(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if kind == "jpeg" {
		switch img.(type) {
		case *goimage.YCbCr, *goimage.Gray:
		default:
			return nil, errors.New("it isn't RGB")
		}
	}
	var out bytes.Buffer
	switch {
	case format == "avif":
		err = avif.Encode(&out, img, avif.EncodeOptions{Quality: avifQuality, Speed: avifSpeed})
	case kind == "png":
		err = encodeWebP(&out, img, true, 0, losslessLevel)
	default:
		err = encodeWebP(&out, img, false, webpQuality, webpEffort)
	}
	if err != nil {
		return nil, fmt.Errorf("converting it: %w", err)
	}
	if float64(out.Len()) > float64(len(body))*(1-minSaving) {
		return nil, errors.New("it isn't smaller converted")
	}
	// It must come back as it went in
	var back goimage.Config
	if format == "avif" {
		back, err = avif.DecodeConfig(bytes.NewReader(out.Bytes()))
	} else {
		back, err = webpConfig(bytes.NewReader(out.Bytes()))
	}
	if err != nil || back.Width != config.Width || back.Height != config.Height {
		return nil, fmt.Errorf("it didn't convert properly: %v", err)
	}
	return out.Bytes(), nil
}

// wideGamut finds colour profiles other than sRGB, whose colours would
// change in a converted image without them
var wideGamut = [][]byte{[]byte("P3"), []byte("Adobe RGB"), []byte("AdobeRGB"), []byte("ProPhoto"), []byte("ROMM"),
	[]byte("2020"), []byte("Wide Gamut"), []byte("WideGamut")}

// checkProfile returns why an ICC colour profile would make an image look
// different converted (as the converted image wouldn't have it), if it would
func checkProfile(profile []byte) error {
	if len(profile) >= 20 {
		if space := string(profile[16:20]); space != "RGB " && space != "GRAY" {
			return fmt.Errorf("its colour profile is for %s", strings.TrimSpace(space))
		}
	}
	for _, name := range wideGamut {
		if bytes.Contains(profile, name) {
			return fmt.Errorf("its colour profile isn't sRGB (%s)", name)
		}
	}
	return nil
}

// checkJPEG returns why a JPEG would look different converted, if it
// would: it's rotated (by its EXIF orientation), or has a colour profile
// other than sRGB
func checkJPEG(b []byte) error {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return errors.New("it isn't a JPEG")
	}
	var profile []byte
	for i := 2; i+4 <= len(b); {
		if b[i] != 0xFF {
			return errors.New("it isn't a JPEG")
		}
		marker := b[i+1]
		if marker == 0xD8 || (marker >= 0xD0 && marker <= 0xD7) || marker == 0x01 || marker == 0xFF {
			i += 2
			continue
		}
		if marker == 0xDA || marker == 0xD9 {
			// The image itself: the markers before it are all there are
			break
		}
		length := int(binary.BigEndian.Uint16(b[i+2:]))
		if length < 2 || i+2+length > len(b) {
			return errors.New("it isn't a JPEG")
		}
		segment := b[i+4 : i+2+length]
		switch {
		case marker == 0xE1 && bytes.HasPrefix(segment, []byte("Exif\x00\x00")):
			if o := exifOrientation(segment[6:]); o > 1 {
				return fmt.Errorf("it's rotated (EXIF orientation %d)", o)
			}
		case marker == 0xE2 && bytes.HasPrefix(segment, []byte("ICC_PROFILE\x00")) && len(segment) > 14:
			profile = append(profile, segment[14:]...)
		}
		i += 2 + length
	}
	if profile != nil {
		return checkProfile(profile)
	}
	return nil
}

// exifOrientation returns the orientation in TIFF data (from EXIF), or 0
func exifOrientation(t []byte) int {
	if len(t) < 8 {
		return 0
	}
	var order binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 0
	}
	ifd := int(order.Uint32(t[4:]))
	if ifd < 8 || ifd+2 > len(t) {
		return 0
	}
	n := int(order.Uint16(t[ifd:]))
	for i := 0; i < n; i++ {
		entry := ifd + 2 + i*12
		if entry+12 > len(t) {
			return 0
		}
		if order.Uint16(t[entry:]) == 0x0112 {
			return int(order.Uint16(t[entry+8:]))
		}
	}
	return 0
}

// checkPNG returns why a PNG would look different converted, if it would:
// it's animated, or has a colour profile other than sRGB
func checkPNG(b []byte) error {
	if !bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")) {
		return errors.New("it isn't a PNG")
	}
	for i := 8; i+8 <= len(b); {
		length := int(binary.BigEndian.Uint32(b[i:]))
		kind := string(b[i+4 : i+8])
		if length < 0 || i+12+length > len(b) {
			return errors.New("it isn't a PNG")
		}
		data := b[i+8 : i+8+length]
		switch kind {
		case "acTL":
			return errors.New("it's animated")
		case "iCCP":
			// A name, a compression method, then the zlib compressed profile
			nul := bytes.IndexByte(data, 0)
			if nul < 0 || nul+2 > len(data) {
				return errors.New("its colour profile is broken")
			}
			z, err := zlib.NewReader(bytes.NewReader(data[nul+2:]))
			if err != nil {
				return errors.New("its colour profile is broken")
			}
			profile, err := io.ReadAll(io.LimitReader(z, 4<<20))
			if err != nil {
				return errors.New("its colour profile is broken")
			}
			if err := checkProfile(profile); err != nil {
				return err
			}
		case "IDAT", "IEND":
			return nil
		}
		i += 12 + length
	}
	return nil
}

// captureWriter passes a response on, and keeps a copy of its body, if
// it's an image which can be converted
type captureWriter struct {
	http.ResponseWriter
	status  int
	body    bytes.Buffer
	tooBig  bool
	started bool
}

func (w *captureWriter) WriteHeader(status int) {
	if !w.started {
		w.started, w.status = true, status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *captureWriter) Write(b []byte) (int, error) {
	if !w.started {
		w.started, w.status = true, http.StatusOK
	}
	if !w.tooBig {
		if w.body.Len()+len(b) > maxConvertBytes {
			w.tooBig = true
			w.body = bytes.Buffer{}
		} else {
			w.body.Write(b)
		}
	}
	return w.ResponseWriter.Write(b)
}

func (w *captureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// captured returns the response's body, and whether it's a public image of
// kind which may be converted
func (w *captureWriter) captured(kind string) ([]byte, bool) {
	h := w.Header()
	if w.status != http.StatusOK || w.tooBig || w.body.Len() < minConvertBytes {
		return nil, false
	}
	if ct := strings.ToLower(h.Get("Content-Type")); ct != "image/"+kind && !strings.HasPrefix(ct, "image/"+kind+";") {
		return nil, false
	}
	if enc := h.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		return nil, false
	}
	if h.Get("Set-Cookie") != "" {
		return nil, false
	}
	cc := strings.ToLower(strings.Join(h.Values("Cache-Control"), ","))
	if strings.Contains(cc, "private") || strings.Contains(cc, "no-store") {
		return nil, false
	}
	for _, v := range h.Values("Vary") {
		for _, field := range strings.Split(v, ",") {
			if f := strings.ToLower(strings.TrimSpace(field)); f == "cookie" || f == "*" || f == "authorization" {
				return nil, false
			}
		}
	}
	if n := h.Get("Content-Length"); n != "" && n != strconv.Itoa(w.body.Len()) {
		return nil, false
	}
	return w.body.Bytes(), true
}

func hashBytes(b []byte) uint64 {
	h := fnv.New64a()
	h.Write(b)
	return h.Sum64()
}

func kilobytes(n int) string {
	if n >= 10<<20 {
		return fmt.Sprintf("%.0fMB", float64(n)/(1<<20))
	}
	if n >= 1<<20 {
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.0fKB", float64(n)/(1<<10))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
