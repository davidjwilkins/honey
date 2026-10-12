package optimize

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOptimizerStateSurvivesARestart(t *testing.T) {
	g := newGoogleFonts(t)
	o := &origin{files: map[string][]byte{"/wp-content/uploads/featured-1024.jpg": jpegOf(t, photo(400, 300, 40))}}
	opts := Options{PreloadMainImage: true, SelfHostFonts: true, ConvertImages: true, googleCSS: g.URL + "/css/", googleFonts: g.URL + "/fonts/"}
	before := New(opts)
	h := before.Middleware(o)

	// A main image learned, a font fetched, and an image converted
	before.Rewrite(response("/post/", wpPage))
	for _, viewport := range []string{"desktop", "mobile"} {
		require.Equal(t, http.StatusNoContent, report(t, h, "/post/", viewport, "http://www.example.com/wp-content/uploads/featured-1024.jpg"))
	}
	before.Rewrite(response("/", fontsPage))
	require.Equal(t, http.StatusOK, get(h, "/.well-known/honey/fonts/css2?family=Inter:wght@400;700&display=swap").Code)
	fetchImage(h, "/wp-content/uploads/featured-1024.jpg", chromeAccept)
	converted(t, h, "/wp-content/uploads/featured-1024.jpg", chromeAccept, "image/avif")

	path := filepath.Join(t.TempDir(), "cache.snapshot.optimize")
	saved, err := before.SaveTo(path)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, saved, 3)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "only Honey may read it")

	// Honey restarts
	after := New(opts)
	loaded, err := after.LoadFrom(path)
	require.NoError(t, err)
	assert.Equal(t, saved, loaded)
	h = after.Middleware(o)
	page := string(after.Rewrite(response("/post/", wpPage)))
	assert.Contains(t, page, `<img fetchpriority="high" src="/wp-content/uploads/featured-1024.jpg"`, "the main image is still known")
	assert.NotContains(t, page, `<script id="honey-main-image">`, "so browsers aren't asked again")
	fonts, requests := g.requests.Load(), o.requests.Load()
	assert.Equal(t, http.StatusOK, get(h, "/.well-known/honey/fonts/css2?family=Inter:wght@400;700&display=swap").Code)
	assert.Equal(t, fonts, g.requests.Load(), "the font isn't fetched from Google again")
	w := fetchImage(h, "/wp-content/uploads/featured-1024.jpg", chromeAccept)
	assert.Equal(t, "image/avif", w.Header().Get("Content-Type"), "and the image isn't converted again")
	assert.Equal(t, requests, o.requests.Load())

	// An optimizer with less on loads less
	fewer := New(Options{ConvertImages: true})
	loaded, err = fewer.LoadFrom(path)
	require.NoError(t, err)
	assert.Equal(t, 1, loaded)
}

func TestLoadingOptimizerSnapshots(t *testing.T) {
	o := New(Options{ConvertImages: true})
	loaded, err := o.LoadFrom(filepath.Join(t.TempDir(), "missing"))
	assert.NoError(t, err, "there's nothing to load the first time")
	assert.Zero(t, loaded)

	bad := filepath.Join(t.TempDir(), "bad")
	require.NoError(t, os.WriteFile(bad, []byte("not a snapshot"), 0o600))
	_, err = o.LoadFrom(bad)
	assert.Error(t, err)

	var none *Optimizer
	saved, err := none.SaveTo(filepath.Join(t.TempDir(), "x"))
	assert.NoError(t, err)
	assert.Zero(t, saved)
}
