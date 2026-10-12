package optimize

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func reserialize(css string) string {
	var b strings.Builder
	serialize(&b, parseCSS(css))
	return b.String()
}

func TestParseCSS(t *testing.T) {
	nodes := parseCSS(`/* theme */
@charset "utf-8";
@import url("a.css") screen;
body, .x > a[href*="{"] { color: red; content: "}" }
@media screen and (min-width: 600px) {
  .wide { display: block }
  @supports (display: grid) { .grid { display: grid } }
}
@font-face { font-family: "Inter"; src: url(a.woff2) }
@keyframes spin { from { transform: rotate(0) } to { transform: rotate(1turn) } }
`)
	assert.Len(t, nodes, 6)
	assert.Equal(t, cssNode{atRule: "charset", prelude: `"utf-8"`}, nodes[0])
	assert.Equal(t, cssNode{atRule: "import", prelude: `url("a.css") screen`}, nodes[1])
	assert.Equal(t, `body, .x > a[href*="{"]`, nodes[2].selectors)
	assert.Equal(t, ` color: red; content: "}" `, nodes[2].body)
	assert.Equal(t, "media", nodes[3].atRule)
	assert.Equal(t, "screen and (min-width: 600px)", nodes[3].prelude)
	assert.Len(t, nodes[3].children, 2)
	assert.Equal(t, ".wide", nodes[3].children[0].selectors)
	assert.Equal(t, "supports", nodes[3].children[1].atRule)
	assert.Equal(t, ".grid", nodes[3].children[1].children[0].selectors)
	assert.Equal(t, "font-face", nodes[4].atRule)
	assert.Contains(t, nodes[4].body, `font-family: "Inter"`)
	assert.Equal(t, "keyframes", nodes[5].atRule)
	assert.Equal(t, "spin", nodes[5].prelude)
	assert.Contains(t, nodes[5].body, "to { transform: rotate(1turn) }")
}

func TestSerializeCSS(t *testing.T) {
	assert.Equal(t, `@import url(a.css);a,b{color:red}@media (min-width:1px){.c{x:y}}@font-face{font-family:F}`,
		reserialize("@import url(a.css);\na,b { color:red }\n@media (min-width:1px) { .c { x:y } }\n@font-face { font-family:F }"))
}

func TestParseMalformedCSS(t *testing.T) {
	for css, want := range map[string]string{
		"a{color:red}} b{color:blue}":          "a{color:red}b{color:blue}",
		"a{color:red}; b{color:blue}":          "a{color:red}b{color:blue}",
		"junk; a{color:red}":                   "a{color:red}",
		"@media screen { a{x:y} junk } b{x:y}": "@media screen{a{x:y}}b{x:y}",
		"a{color:red":                          "a{color:red}",
		"a{color:red} b":                       "a{color:red}",
		"<!-- a{x:y} -->":                      "a{x:y}",
		"a{x:y} /* unclosed":                   "a{x:y}",
		"a:not(.b, .c){x:y}":                   "a:not(.b, .c){x:y}",
		`a{background:url("x;}.png")} b{x:y}`:  `a{background:url("x;}.png")}b{x:y}`,
		"@media screen { a{x:y}":               "@media screen{a{x:y}}",
	} {
		assert.Equal(t, want, reserialize(css), css)
	}
}

func TestSplitSelectors(t *testing.T) {
	assert.Equal(t, []string{"a", ".b:is(.c, .d)", `[title="x,y"]`}, splitSelectors(`a, .b:is(.c, .d) ,[title="x,y"]`))
}
