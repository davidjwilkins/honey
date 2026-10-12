package fetch

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/davidjwilkins/honey/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const esiTestPage = `<html><head><title>A post</title></head><body><!--esi <p>ESI on</p> -->` +
	`<esi:remove><p>No ESI</p></esi:remove><article>ARTICLE</article>` +
	`<esi:include src="/post/?honey-esi=comments"/><footer>FOOTER</footer>` +
	`<esi:include src="/post/?honey-esi=empty"/><esi:include src="/post/?honey-esi=cart"/></body></html>`

func testPage() (*esiPage, [][]byte) {
	body := strings.ReplaceAll(strings.ReplaceAll(esiTestPage, "ARTICLE", strings.Repeat("Welcome to WordPress. ", 3000)),
		"FOOTER", strings.Repeat("A footer. ", 500))
	page := parseESIPage([]byte(body))
	contents := [][]byte{
		[]byte(strings.Repeat("<li>A comment, for Jane.</li>", 200)),
		nil, // a failed include
		[]byte("<p>3 items</p>"),
	}
	return page, contents
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(b))
	require.NoError(t, err)
	plain, err := io.ReadAll(r)
	require.NoError(t, err, "a valid gzip stream, with the right checksum and length")
	return plain
}

func TestESIPageParts(t *testing.T) {
	page, contents := testPage()
	require.Len(t, page.includes, 3)
	assert.Equal(t, "/post/?honey-esi=comments", page.includes[0]["src"])
	plain := string(page.plain(contents))
	assert.Contains(t, plain, "<p>ESI on</p>")
	assert.NotContains(t, plain, "No ESI")
	assert.NotContains(t, plain, "esi:")
	assert.Contains(t, plain, "</article><li>A comment, for Jane.</li>")
	assert.Contains(t, plain, "<footer>A footer. ")
	assert.True(t, strings.HasSuffix(plain, "<p>3 items</p></body></html>"))
	assert.Equal(t, len(plain), page.size(contents))
}

func TestESIPageGzip(t *testing.T) {
	page, contents := testPage()
	plain := page.plain(contents)
	gz := page.gzip(contents)
	assert.Equal(t, plain, gunzip(t, gz))
	assert.Less(t, len(gz), len(plain)/10, "compressed")
	assert.Equal(t, plain, gunzip(t, page.gzip(contents)), "again, with the parts compressed before")

	// For another visitor
	other := [][]byte{[]byte("<li>Bob's comment</li>"), []byte("x"), nil}
	assert.Equal(t, page.plain(other), gunzip(t, page.gzip(other)))
	// With no includes at all
	none := parseESIPage([]byte(strings.Repeat("no includes ", 1000)))
	assert.Equal(t, none.plain(nil), gunzip(t, none.gzip(nil)))

	// Other decoders agree: gzip(1), and zlib (as browsers use)
	if _, err := exec.LookPath("gzip"); err == nil {
		f := t.TempDir() + "/page.gz"
		require.NoError(t, os.WriteFile(f, gz, 0o600))
		out, err := exec.Command("gzip", "-dc", f).Output()
		require.NoError(t, err)
		assert.Equal(t, plain, out)
	}
	if _, err := exec.LookPath("python3"); err == nil {
		cmd := exec.Command("python3", "-c", "import sys, zlib; d = zlib.decompressobj(16 + zlib.MAX_WBITS); sys.stdout.buffer.write(d.decompress(sys.stdin.buffer.read())); assert d.eof")
		cmd.Stdin = bytes.NewReader(gz)
		out, err := cmd.Output()
		require.NoError(t, err)
		assert.Equal(t, plain, out)
	}
}

func BenchmarkESIPage(b *testing.B) {
	page, contents := testPage()
	b.Run("gzip-stitched", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			page.gzip(contents)
		}
	})
	b.Run("brotli-whole", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			cache.Compress("br", page.plain(contents))
		}
	})
}
