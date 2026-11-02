// Adapted from TypeBox 1.3.27 (MIT, Copyright (c) 2017-2026 Haydn Paterson);
// see LICENSES/typebox-LICENSE.

package formats

import "unicode"

// Regex reports whether value is an ECMAScript regular expression pattern
// that `new RegExp(value, "u")` accepts. Ports typebox/regex.ts, which asks
// the JavaScript engine; this answers with a validator of the /u pattern
// grammar as V8 12.4 (Node v22) parses it:
//
//   - no Annex B leniency: identity escapes are syntax characters and "/"
//     only, lone "{", "}" and "]" are errors, and lookarounds take no
//     quantifier;
//   - \1…\N must name an existing group, \k<name> a defined name, and group
//     names are unique (V8 12.4 predates duplicate named groups and (?i:)
//     modifiers, which are errors);
//   - \p{…} takes the property names and values V8 knows (tables_v8.go);
//   - {n,m} bounds clamp at 2^31-1 before the order check, and more than
//     32767 capturing groups is an error, as in V8.
//
// Two V8 quirks are kept: an escaped ">" (>) after the first code point
// of a group name ends the name, and the order check compares clamped bounds.
func Regex(value string) bool {
	p := &patternParser{src: []rune(value)}
	return p.parse()
}

const (
	maxCaptures     = 32767     // V8: more is "Too many captures"
	quantifierLimit = 1<<31 - 1 // V8's RegExpTree::kInfinity
)

type groupKind uint8

const (
	groupCapture groupKind = iota
	groupPlain
	groupLookaround
)

type patternParser struct {
	src        []rune
	pos        int
	captures   int
	names      map[string]bool
	nameRefs   []string
	maxBackref int
}

func (p *patternParser) eof() bool { return p.pos >= len(p.src) }

// peek returns the code point at pos+off, or -1 past the end.
func (p *patternParser) peek(off int) rune {
	if p.pos+off < len(p.src) {
		return p.src[p.pos+off]
	}
	return -1
}

// parse walks the pattern once; groups only need a stack of their kinds,
// since what makes a pattern invalid is local except for the backreference
// and group-name checks done at the end.
func (p *patternParser) parse() bool {
	var groups []groupKind
	for !p.eof() {
		c := p.src[p.pos]
		switch c {
		case '|':
			p.pos++
			continue
		case '(':
			kind, ok := p.groupOpen()
			if !ok {
				return false
			}
			groups = append(groups, kind)
			continue
		case ')':
			if len(groups) == 0 {
				return false
			}
			kind := groups[len(groups)-1]
			groups = groups[:len(groups)-1]
			p.pos++
			if kind == groupLookaround {
				if p.atQuantifier() {
					return false
				}
				continue
			}
		case '^', '$':
			p.pos++
			if p.atQuantifier() {
				return false
			}
			continue
		case '\\':
			if next := p.peek(1); next == 'b' || next == 'B' {
				p.pos += 2
				if p.atQuantifier() {
					return false
				}
				continue
			}
			p.pos++
			if !p.atomEscape() {
				return false
			}
		case '[':
			if !p.class() {
				return false
			}
		case '*', '+', '?', '{', '}', ']':
			return false // nothing to repeat, or a lone quantifier bracket
		default:
			p.pos++ // a pattern character or "."
		}
		if !p.quantifier() {
			return false
		}
	}
	if len(groups) > 0 || p.captures > maxCaptures || p.maxBackref > p.captures {
		return false
	}
	for _, name := range p.nameRefs {
		if !p.names[name] {
			return false
		}
	}
	return true
}

func (p *patternParser) atQuantifier() bool {
	switch p.peek(0) {
	case '*', '+', '?', '{':
		return true
	}
	return false
}

// groupOpen consumes "(", "(?:", "(?=", "(?!", "(?<=", "(?<!" or "(?<name>".
func (p *patternParser) groupOpen() (groupKind, bool) {
	p.pos++
	if p.peek(0) != '?' {
		p.captures++
		return groupCapture, true
	}
	switch p.peek(1) {
	case ':':
		p.pos += 2
		return groupPlain, true
	case '=', '!':
		p.pos += 2
		return groupLookaround, true
	case '<':
		if next := p.peek(2); next == '=' || next == '!' {
			p.pos += 3
			return groupLookaround, true
		}
		p.pos += 2
		name, ok := p.groupName()
		if !ok || p.names[name] {
			return 0, false
		}
		if p.names == nil {
			p.names = map[string]bool{}
		}
		p.names[name] = true
		p.captures++
		return groupCapture, true
	}
	return 0, false
}

// groupName reads a RegExpIdentifierName and its closing ">", as V8's
// ParseCaptureGroupName does.
func (p *patternParser) groupName() (string, bool) {
	var name []rune
	for {
		if p.eof() {
			return "", false
		}
		c := p.src[p.pos]
		p.pos++
		if c == '\\' {
			if p.peek(0) != 'u' {
				return "", false
			}
			p.pos++
			var ok bool
			if c, ok = p.unicodeEscape(); !ok {
				return "", false
			}
		}
		if len(name) == 0 {
			if !unicode.Is(jsGroupNameStart, c) {
				return "", false
			}
		} else if c == '>' {
			return string(name), true
		} else if !unicode.Is(jsGroupNameContinue, c) {
			return "", false
		}
		name = append(name, c)
	}
}

// unicodeEscape reads what follows "\u": {hex…} up to U+10FFFF, or four hex
// digits, joined with a following \u trail surrogate when they make a lead.
func (p *patternParser) unicodeEscape() (rune, bool) {
	if p.peek(0) == '{' {
		p.pos++
		var v rune
		digits := 0
		for {
			d := hexValue(p.peek(0))
			if d < 0 {
				break
			}
			v = v*16 + d
			if v > unicode.MaxRune {
				return 0, false
			}
			p.pos++
			digits++
		}
		if digits == 0 || p.peek(0) != '}' {
			return 0, false
		}
		p.pos++
		return v, true
	}
	v, ok := p.hex4(0)
	if !ok {
		return 0, false
	}
	p.pos += 4
	if v >= 0xd800 && v <= 0xdbff && p.peek(0) == '\\' && p.peek(1) == 'u' {
		if lo, ok := p.hex4(2); ok && lo >= 0xdc00 && lo <= 0xdfff {
			p.pos += 6
			return (v-0xd800)<<10 + (lo - 0xdc00) + 0x10000, true
		}
	}
	return v, true
}

func (p *patternParser) hex4(off int) (rune, bool) {
	var v rune
	for i := range 4 {
		d := hexValue(p.peek(off + i))
		if d < 0 {
			return 0, false
		}
		v = v*16 + d
	}
	return v, true
}

func hexValue(c rune) rune {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	}
	return -1
}

func isDecimal(c rune) bool { return c >= '0' && c <= '9' }

func isSyntaxChar(c rune) bool {
	switch c {
	case '^', '$', '\\', '.', '*', '+', '?', '(', ')', '[', ']', '{', '}', '|', '/':
		return true
	}
	return false
}

// atomEscape reads an AtomEscape after "\" (\b and \B are assertions,
// handled by the caller).
func (p *patternParser) atomEscape() bool {
	c := p.peek(0)
	switch {
	case c >= '1' && c <= '9':
		n := 0
		for isDecimal(p.peek(0)) {
			if n <= maxCaptures {
				n = n*10 + int(p.peek(0)-'0')
			}
			p.pos++
		}
		p.maxBackref = max(p.maxBackref, n)
		return true
	case c == 'k':
		p.pos++
		if p.peek(0) != '<' {
			return false
		}
		p.pos++
		name, ok := p.groupName()
		if !ok {
			return false
		}
		p.nameRefs = append(p.nameRefs, name)
		return true
	}
	_, ok := p.characterEscape(false)
	return ok
}

// escapeClass marks an escape that stands for a set (\d, \p{…}, …) rather
// than one code point.
const escapeClass rune = -2

// characterEscape reads the escapes atoms and class atoms share, plus "\-"
// and "\b" inside a class. It returns the code point, or escapeClass.
func (p *patternParser) characterEscape(inClass bool) (rune, bool) {
	if p.eof() {
		return 0, false
	}
	c := p.src[p.pos]
	p.pos++
	switch c {
	case 'd', 'D', 's', 'S', 'w', 'W':
		return escapeClass, true
	case 'p', 'P':
		return escapeClass, p.property()
	case 'f':
		return '\f', true
	case 'n':
		return '\n', true
	case 'r':
		return '\r', true
	case 't':
		return '\t', true
	case 'v':
		return '\v', true
	case 'c':
		l := p.peek(0)
		if !(l >= 'a' && l <= 'z' || l >= 'A' && l <= 'Z') {
			return 0, false
		}
		p.pos++
		return l % 32, true
	case '0':
		return 0, !isDecimal(p.peek(0))
	case 'x':
		hi, lo := hexValue(p.peek(0)), hexValue(p.peek(1))
		if hi < 0 || lo < 0 {
			return 0, false
		}
		p.pos += 2
		return hi*16 + lo, true
	case 'u':
		return p.unicodeEscape()
	case 'b':
		return '\b', inClass
	case '-':
		return '-', inClass
	}
	return c, isSyntaxChar(c)
}

// property reads {Name} or {Name=Value} after \p or \P.
func (p *patternParser) property() bool {
	if p.peek(0) != '{' {
		return false
	}
	p.pos++
	var name, value []rune
	sawEquals := false
	for {
		c := p.peek(0)
		switch {
		case c == '}':
			p.pos++
			if !sawEquals {
				return jsLoneProperties[string(name)]
			}
			key, val := string(name), string(value)
			return jsGeneralCategoryKeys[key] && jsGeneralCategoryValues[val] ||
				jsScriptKeys[key] && jsScriptValues[val]
		case c == '=' && !sawEquals:
			sawEquals = true
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
			if sawEquals {
				value = append(value, c)
			} else {
				name = append(name, c)
			}
		default:
			return false
		}
		p.pos++
	}
}

// class reads a CharacterClass from its "[".
func (p *patternParser) class() bool {
	p.pos++
	if p.peek(0) == '^' {
		p.pos++
	}
	for {
		if p.eof() {
			return false
		}
		if p.src[p.pos] == ']' {
			p.pos++
			return true
		}
		from, ok := p.classAtom()
		if !ok {
			return false
		}
		if p.peek(0) != '-' || p.peek(1) == ']' || p.peek(1) == -1 {
			continue
		}
		p.pos++ // the range's "-"
		to, ok := p.classAtom()
		if !ok || from == escapeClass || to == escapeClass || from > to {
			return false
		}
	}
}

func (p *patternParser) classAtom() (rune, bool) {
	if p.eof() {
		return 0, false
	}
	c := p.src[p.pos]
	p.pos++
	if c != '\\' {
		return c, true
	}
	return p.characterEscape(true)
}

// quantifier reads an optional quantifier and its lazy "?". In /u mode a
// "{" that does not open a well-formed {n}, {n,} or {n,m} is an error.
func (p *patternParser) quantifier() bool {
	switch p.peek(0) {
	case '*', '+', '?':
		p.pos++
	case '{':
		p.pos++
		lo, ok := p.quantifierBound()
		if !ok {
			return false
		}
		hi := lo
		if p.peek(0) == ',' {
			p.pos++
			hi = quantifierLimit
			if p.peek(0) != '}' {
				if hi, ok = p.quantifierBound(); !ok {
					return false
				}
			}
		}
		if p.peek(0) != '}' || hi < lo {
			return false
		}
		p.pos++
	default:
		return true
	}
	if p.peek(0) == '?' {
		p.pos++
	}
	return true
}

// quantifierBound reads decimal digits, clamping at 2^31-1 as V8 does.
func (p *patternParser) quantifierBound() (int, bool) {
	if !isDecimal(p.peek(0)) {
		return 0, false
	}
	n := 0
	for isDecimal(p.peek(0)) {
		d := int(p.peek(0) - '0')
		if n > (quantifierLimit-d)/10 {
			n = quantifierLimit
			for isDecimal(p.peek(0)) {
				p.pos++
			}
			break
		}
		n = n*10 + d
		p.pos++
	}
	return n, true
}
