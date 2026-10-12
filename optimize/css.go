package optimize

import (
	"strings"
)

// A stylesheet, as far as removing unused CSS needs to understand it: a
// list of style rules (selectors and declarations) and at-rules (@media and
// the like, which may hold more rules), with comments left out.

type cssNode struct {
	// For a style rule, selectors and body (the declarations, without
	// braces)
	selectors string
	// For an at-rule, its name (lower case, without "@") and prelude (e.g.
	// "media" and "screen and (min-width: 600px)"), and either the rules
	// in its block (children) or, for one whose block holds declarations
	// (@font-face, @page) or keyframes, the block's text (body)
	atRule   string
	prelude  string
	children []cssNode
	body     string
	// block is whether it has a block (rather than ending with ";", like
	// @import)
	block bool
}

// nestedAtRules are at-rules whose blocks hold rules, which are looked into
var nestedAtRules = map[string]bool{"media": true, "supports": true, "layer": true, "container": true, "document": true, "-moz-document": true, "scope": true, "starting-style": true}

// parseCSS parses a stylesheet into its rules.  It's forgiving, like a
// browser: what it can't make sense of, it skips.
func parseCSS(css string) []cssNode {
	p := &cssParser{s: stripComments(css)}
	return p.rules(false)
}

type cssParser struct {
	s   string
	pos int
}

// rules parses rules until the end, or (if nested) the "}" which closes the
// block they're in
func (p *cssParser) rules(nested bool) []cssNode {
	var nodes []cssNode
	for {
		p.skipSpace()
		if p.pos >= len(p.s) {
			return nodes
		}
		if p.s[p.pos] == '}' {
			p.pos++
			if nested {
				return nodes
			}
			continue
		}
		if p.s[p.pos] == '@' {
			nodes = append(nodes, p.atRule())
			continue
		}
		prelude, ok := p.until('{')
		if !ok {
			continue
		}
		p.pos--
		body := p.block()
		nodes = append(nodes, cssNode{selectors: strings.TrimSpace(prelude), body: body, block: true})
	}
}

func (p *cssParser) atRule() cssNode {
	start := p.pos + 1
	end := start
	for end < len(p.s) && (isNameByte(p.s[end])) {
		end++
	}
	n := cssNode{atRule: strings.ToLower(p.s[start:end])}
	p.pos = end
	// The prelude ends at "{" or ";"
	i := p.pos
	for i < len(p.s) {
		c := p.s[i]
		switch {
		case c == '"' || c == '\'':
			i = skipString(p.s, i)
			continue
		case c == '(':
			i = skipParens(p.s, i)
			continue
		case c == '{' || c == ';' || c == '}':
		default:
			i++
			continue
		}
		break
	}
	n.prelude = strings.TrimSpace(p.s[p.pos:i])
	p.pos = i
	if i >= len(p.s) || p.s[i] != '{' {
		if i < len(p.s) && p.s[i] == ';' {
			p.pos++
		}
		return n
	}
	n.block = true
	p.pos++
	if nestedAtRules[n.atRule] {
		n.children = p.rules(true)
		return n
	}
	p.pos--
	n.body = p.block()
	return n
}

// until returns the text up to c (outside strings and brackets), and moves
// past c.  If it comes to a stray ";" or "}" first, it moves past the ";"
// or to the "}" (which may close the block the rule is in), skipping what
// came before, and returns false; so it does at the end.
func (p *cssParser) until(c byte) (string, bool) {
	i := p.pos
	for i < len(p.s) {
		switch p.s[i] {
		case '"', '\'':
			i = skipString(p.s, i)
			continue
		case '(':
			i = skipParens(p.s, i)
			continue
		case '[':
			// Attribute selectors' values may be quoted, and hold anything
			for i++; i < len(p.s) && p.s[i] != ']' && p.s[i] != '{' && p.s[i] != '}'; i++ {
				if p.s[i] == '"' || p.s[i] == '\'' {
					i = skipString(p.s, i) - 1
				}
			}
			continue
		case c:
			text := p.s[p.pos:i]
			p.pos = i + 1
			return text, true
		case ';':
			p.pos = i + 1
			return "", false
		case '}':
			p.pos = i
			return "", false
		}
		i++
	}
	p.pos = len(p.s)
	return "", false
}

// block returns the text of the block starting at "{" (or just after it),
// without its braces, and moves past its "}"
func (p *cssParser) block() string {
	if p.pos < len(p.s) && p.s[p.pos] == '{' {
		p.pos++
	}
	start, depth := p.pos, 0
	for p.pos < len(p.s) {
		switch p.s[p.pos] {
		case '"', '\'':
			p.pos = skipString(p.s, p.pos)
			continue
		case '{':
			depth++
		case '}':
			if depth == 0 {
				body := p.s[start:p.pos]
				p.pos++
				return body
			}
			depth--
		}
		p.pos++
	}
	return p.s[start:]
}

func (p *cssParser) skipSpace() {
	for p.pos < len(p.s) && (p.s[p.pos] == ' ' || p.s[p.pos] == '\n' || p.s[p.pos] == '\t' || p.s[p.pos] == '\r' || p.s[p.pos] == '\f' || p.s[p.pos] == ';') {
		p.pos++
	}
	// Old-style HTML comment markers, which CSS ignores
	if strings.HasPrefix(p.s[p.pos:], "<!--") {
		p.pos += 4
		p.skipSpace()
	} else if strings.HasPrefix(p.s[p.pos:], "-->") {
		p.pos += 3
		p.skipSpace()
	}
}

func isNameByte(c byte) bool {
	return c == '-' || c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}

// skipString returns the index just after the string starting at s[i]
func skipString(s string, i int) int {
	quote := s[i]
	for i++; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case quote, '\n':
			return i + 1
		}
	}
	return len(s)
}

// skipParens returns the index just after the parentheses starting at s[i]
func skipParens(s string, i int) int {
	depth := 0
	for i < len(s) {
		switch s[i] {
		case '"', '\'':
			i = skipString(s, i)
			continue
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1
			}
		case '{', '}', ';':
			// Unbalanced: let the caller deal with it
			return i
		}
		i++
	}
	return len(s)
}

// stripComments returns css without its comments (outside strings)
func stripComments(css string) string {
	if !strings.Contains(css, "/*") {
		return css
	}
	var b strings.Builder
	b.Grow(len(css))
	for i := 0; i < len(css); {
		switch {
		case css[i] == '"' || css[i] == '\'':
			end := skipString(css, i)
			b.WriteString(css[i:end])
			i = end
		case strings.HasPrefix(css[i:], "/*"):
			end := strings.Index(css[i+2:], "*/")
			if end < 0 {
				return b.String()
			}
			i += end + 4
			b.WriteByte(' ')
		default:
			b.WriteByte(css[i])
			i++
		}
	}
	return b.String()
}

// splitSelectors splits a selector list at its top level commas
func splitSelectors(list string) []string {
	var parts []string
	depth, start := 0, 0
	for i := 0; i < len(list); i++ {
		switch list[i] {
		case '"', '\'':
			i = skipString(list, i) - 1
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(list[start:i]))
				start = i + 1
			}
		}
	}
	return append(parts, strings.TrimSpace(list[start:]))
}

// serialize writes nodes as CSS
func serialize(b *strings.Builder, nodes []cssNode) {
	for _, n := range nodes {
		switch {
		case n.atRule == "":
			b.WriteString(n.selectors)
			b.WriteByte('{')
			b.WriteString(strings.TrimSpace(n.body))
			b.WriteByte('}')
		case !n.block:
			b.WriteString("@" + n.atRule + " " + n.prelude + ";")
		case n.children != nil || nestedAtRules[n.atRule]:
			b.WriteString("@" + n.atRule)
			if n.prelude != "" {
				b.WriteString(" " + n.prelude)
			}
			b.WriteByte('{')
			serialize(b, n.children)
			b.WriteByte('}')
		default:
			b.WriteString("@" + n.atRule)
			if n.prelude != "" {
				b.WriteString(" " + n.prelude)
			}
			b.WriteByte('{')
			b.WriteString(strings.TrimSpace(n.body))
			b.WriteByte('}')
		}
	}
}
