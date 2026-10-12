package cache

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

const wordPressHead = `<!DOCTYPE html>
<html lang="en-US">
<head>
<meta charset="UTF-8" />
<link rel='preconnect' href='https://fonts.gstatic.com' crossorigin />
<link rel="preload" href="/wp-content/themes/twentytwentyfive/assets/fonts/manrope/Manrope-VariableFont_wght.woff2" as="font" type="font/woff2" crossorigin>
<link rel='stylesheet' id='wp-block-library-css' href='/wp-includes/css/dist/block-library/style.min.css?ver=7.0&#038;x=1' media='all' />
<link rel="stylesheet" href="/print.css" media="print">
<link rel="alternate stylesheet" href="/dark.css">
<link rel="stylesheet" href="theme.css">
<!-- <link rel="stylesheet" href="/commented-out.css"> -->
<link rel="alternate" type="application/rss+xml" href="/feed/">
<script src="/wp-includes/js/jquery/jquery.min.js?ver=3.7.1" id="jquery-core-js"></script>
<script src="/deferred.js" defer></script>
<script async src="/async.js"></script>
<script type="module" src="/module.js"></script>
<script type="application/ld+json">{"@context":"https://schema.org"}</script>
<link rel="modulepreload" href="/wp-includes/js/dist/script-modules/interactivity/index.min.js">
<link rel="stylesheet" href="/evil.css>; rel=preload; as=script">
<link rel="stylesheet" href="javascript:alert(1)">
</head>
<body>
<link rel="stylesheet" href="/in-the-body.css">
</body>
</html>`

func TestParseHints(t *testing.T) {
	assert.Equal(t, []string{
		"<https://fonts.gstatic.com>; rel=preconnect; crossorigin",
		"</wp-content/themes/twentytwentyfive/assets/fonts/manrope/Manrope-VariableFont_wght.woff2>; rel=preload; as=font; type=font/woff2; crossorigin",
		"</wp-includes/css/dist/block-library/style.min.css?ver=7.0&x=1>; rel=preload; as=style",
		"</wp-includes/js/jquery/jquery.min.js?ver=3.7.1>; rel=preload; as=script",
		"</wp-includes/js/dist/script-modules/interactivity/index.min.js>; rel=modulepreload",
	}, parseHints([]byte(wordPressHead)))
}

func TestParseHintsLimits(t *testing.T) {
	assert.Nil(t, parseHints([]byte(`<head><link rel="stylesheet" href="/a.css">`)), "without the end of the head, it can't be told what's in it")
	assert.Nil(t, parseHints([]byte(`<head></head><body>`)))

	var head strings.Builder
	head.WriteString("<head>")
	for i := 0; i < 50; i++ {
		head.WriteString(`<link rel="stylesheet" href="/style-` + strings.Repeat("x", i) + `.css">`)
	}
	head.WriteString("</head>")
	assert.Len(t, parseHints([]byte(head.String())), maxHints)

	long := `<head><link rel="stylesheet" href="/` + strings.Repeat("x", maxHintBytes) + `.css"><link rel="stylesheet" href="/b.css"></head>`
	assert.Empty(t, parseHints([]byte(long)), "the hints stop before they get too long")

	late := `<head>` + strings.Repeat(" ", maxHeadBytes) + `<link rel="stylesheet" href="/a.css"></head>`
	assert.Nil(t, parseHints([]byte(late)), "the head is only looked for so far into the page")
}

func cachedPage(c *defaultCacher, uri, contentType, body string) *responseImpl {
	request := newValidRequest(uri)
	resp := c.Standardize(&http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {contentType}},
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Request:    request,
	}).(*responseImpl)
	c.Cache(c.Hash(request), resp)
	return resp
}

func TestEarlyHints(t *testing.T) {
	c := NewCacher(Options{})
	assert.NotEmpty(t, EarlyHints(cachedPage(c, "https://www.example.com/", "text/html; charset=UTF-8", wordPressHead)))
	assert.Nil(t, EarlyHints(cachedPage(c, "https://www.example.com/a.xml", "application/xml", wordPressHead)), "only HTML pages")
}

func TestCommonEarlyHints(t *testing.T) {
	c := NewCacher(Options{})
	page := func(path string, styles ...string) {
		body := "<head>"
		for _, s := range styles {
			body += `<link rel="stylesheet" href="/` + s + `.css">`
		}
		cachedPage(c, "https://www.example.com"+path, "text/html", body+"</head>")
	}
	page("/", "theme", "home", "blocks")
	page("/a/", "theme", "blocks", "a")
	assert.Nil(t, c.CommonEarlyHints("www.example.com"), "not until it has seen enough pages")
	page("/b/", "blocks", "b", "theme")
	assert.Equal(t, []string{"</blocks.css>; rel=preload; as=style", "</theme.css>; rel=preload; as=style"}, c.CommonEarlyHints("WWW.example.com"),
		"those every page has, in the order the latest has them")
	assert.Nil(t, c.CommonEarlyHints("other.example.com"), "each host's are its own")

	page("/c/", "c")
	assert.Empty(t, c.CommonEarlyHints("www.example.com"))
	for i := 0; i < hintPages; i++ {
		page("/"+strings.Repeat("d", i+1)+"/", "theme")
	}
	assert.Equal(t, []string{"</theme.css>; rel=preload; as=style"}, c.CommonEarlyHints("www.example.com"), "only recent pages count")
}
