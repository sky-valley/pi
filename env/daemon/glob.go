package daemon

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Globs as the globset crate (0.4.20) parses gitignore lines for the ignore
// crate: literal_separator, backslash_escape and allow_unclosed_class on,
// case sensitive. globset compiles each glob to a regular expression over
// bytes ((?-u)); Go's regexp works on runes, so the expression is built the
// same way and both it and the candidate path are carried byte for rune
// (Latin-1), which makes every rune stand for one byte.

type globToken struct {
	kind       int
	literal    rune
	negated    bool
	ranges     [][2]rune
	alternates [][]globToken
}

const (
	tokenLiteral = iota
	tokenAny
	tokenZeroOrMore
	tokenRecursivePrefix
	tokenRecursiveSuffix
	tokenRecursiveZeroOrMore
	tokenClass
	tokenAlternates
)

var (
	errUnclosedAlternates = errors.New("unclosed alternate group; missing '}' (maybe escape '{' with '[{]'?)")
	errUnopenedAlternates = errors.New("unopened alternate group; missing '{' (maybe escape '}' with '[}]'?)")
	errDanglingEscape     = errors.New("dangling '\\'")
)

type globParser struct {
	chars        []rune
	pos          int
	prev, cur    rune
	hasPrev      bool
	hasCur       bool
	stack        []int
	branches     [][]globToken
	unclosedSeen bool
}

func (p *globParser) bump() (rune, bool) {
	p.prev, p.hasPrev = p.cur, p.hasCur
	if p.pos >= len(p.chars) {
		p.hasCur = false
		return 0, false
	}
	p.cur, p.hasCur = p.chars[p.pos], true
	p.pos++
	return p.cur, true
}

func (p *globParser) peek() (rune, bool) {
	if p.pos >= len(p.chars) {
		return 0, false
	}
	return p.chars[p.pos], true
}

func (p *globParser) push(t globToken) error {
	if len(p.branches) == 0 {
		return errUnopenedAlternates
	}
	last := len(p.branches) - 1
	p.branches[last] = append(p.branches[last], t)
	return nil
}

func (p *globParser) pop() (globToken, error) {
	if len(p.branches) == 0 {
		return globToken{}, errUnopenedAlternates
	}
	last := len(p.branches) - 1
	tokens := p.branches[last]
	t := tokens[len(tokens)-1]
	p.branches[last] = tokens[:len(tokens)-1]
	return t, nil
}

func (p *globParser) haveTokens() (bool, error) {
	if len(p.branches) == 0 {
		return false, errUnopenedAlternates
	}
	return len(p.branches[len(p.branches)-1]) > 0, nil
}

func isSeparator(c rune) bool { return c == '/' }

func (p *globParser) parse() error {
	for {
		c, ok := p.bump()
		if !ok {
			return nil
		}
		var err error
		switch {
		case c == '?':
			err = p.push(globToken{kind: tokenAny})
		case c == '*':
			err = p.parseStar()
		case c == '[' && !p.unclosedSeen:
			err = p.parseClass()
		case c == '{':
			p.stack = append(p.stack, len(p.branches))
			p.branches = append(p.branches, nil)
		case c == '}':
			if len(p.stack) == 0 {
				return errUnopenedAlternates
			}
			start := p.stack[len(p.stack)-1]
			p.stack = p.stack[:len(p.stack)-1]
			alternates := append([][]globToken(nil), p.branches[start:]...)
			p.branches = p.branches[:start]
			err = p.push(globToken{kind: tokenAlternates, alternates: alternates})
		case c == ',':
			if len(p.stack) == 0 {
				err = p.push(globToken{kind: tokenLiteral, literal: ','})
			} else {
				p.branches = append(p.branches, nil)
			}
		case c == '\\':
			next, ok := p.bump()
			if !ok {
				return errDanglingEscape
			}
			err = p.push(globToken{kind: tokenLiteral, literal: next})
		default:
			err = p.push(globToken{kind: tokenLiteral, literal: c})
		}
		if err != nil {
			return err
		}
	}
}

func (p *globParser) parseStar() error {
	prev, hasPrev := p.prev, p.hasPrev
	if next, ok := p.peek(); !ok || next != '*' {
		return p.push(globToken{kind: tokenZeroOrMore})
	}
	p.bump()
	have, err := p.haveTokens()
	if err != nil {
		return err
	}
	if !have {
		if next, ok := p.peek(); ok && !isSeparator(next) {
			if err := p.push(globToken{kind: tokenZeroOrMore}); err != nil {
				return err
			}
			return p.push(globToken{kind: tokenZeroOrMore})
		}
		if err := p.push(globToken{kind: tokenRecursivePrefix}); err != nil {
			return err
		}
		p.bump()
		return nil
	}
	if !(hasPrev && isSeparator(prev)) {
		if len(p.branches) <= 1 || (prev != ',' && prev != '{') {
			if err := p.push(globToken{kind: tokenZeroOrMore}); err != nil {
				return err
			}
			return p.push(globToken{kind: tokenZeroOrMore})
		}
	}
	var suffix bool
	next, ok := p.peek()
	switch {
	case !ok:
		p.bump()
		suffix = true
	case (next == ',' || next == '}') && len(p.branches) >= 2:
		suffix = true
	case isSeparator(next):
		p.bump()
		suffix = false
	default:
		if err := p.push(globToken{kind: tokenZeroOrMore}); err != nil {
			return err
		}
		return p.push(globToken{kind: tokenZeroOrMore})
	}
	last, err := p.pop()
	if err != nil {
		return err
	}
	switch {
	case last.kind == tokenRecursivePrefix || last.kind == tokenRecursiveSuffix:
		return p.push(last)
	case suffix:
		return p.push(globToken{kind: tokenRecursiveSuffix})
	default:
		return p.push(globToken{kind: tokenRecursiveZeroOrMore})
	}
}

func (p *globParser) parseClass() error {
	savedPos, savedPrev, savedHasPrev, savedCur, savedHasCur := p.pos, p.prev, p.hasPrev, p.cur, p.hasCur
	var ranges [][2]rune
	negated := false
	if next, ok := p.peek(); ok && (next == '!' || next == '^') {
		p.bump()
		negated = true
	}
	first, inRange := true, false
	extend := func(c rune) error {
		r := &ranges[len(ranges)-1]
		r[1] = c
		if r[1] < r[0] {
			return fmt.Errorf("invalid range; '%c' > '%c'", r[0], r[1])
		}
		return nil
	}
	for {
		c, ok := p.bump()
		if !ok {
			// allow_unclosed_class: the '[' is a literal after all.
			p.pos, p.prev, p.hasPrev, p.cur, p.hasCur = savedPos, savedPrev, savedHasPrev, savedCur, savedHasCur
			p.unclosedSeen = true
			return p.push(globToken{kind: tokenLiteral, literal: '['})
		}
		switch {
		case c == ']':
			if !first {
				if inRange {
					ranges = append(ranges, [2]rune{'-', '-'})
				}
				return p.push(globToken{kind: tokenClass, negated: negated, ranges: ranges})
			}
			ranges = append(ranges, [2]rune{']', ']'})
		case c == '-':
			switch {
			case first:
				ranges = append(ranges, [2]rune{'-', '-'})
			case inRange:
				if err := extend('-'); err != nil {
					return err
				}
				inRange = false
			default:
				inRange = true
			}
		default:
			if inRange {
				if err := extend(c); err != nil {
					return err
				}
			} else {
				ranges = append(ranges, [2]rune{c, c})
			}
			inRange = false
		}
		first = false
	}
}

// globRegexp is the regular expression globset builds for tokens, with every
// non-ASCII byte written as \xNN (a rune of that value here).
func globRegexp(tokens []globToken) string {
	var re strings.Builder
	re.WriteString("^")
	if len(tokens) == 1 && tokens[0].kind == tokenRecursivePrefix {
		re.WriteString(".*$")
		return re.String()
	}
	tokensRegexp(&re, tokens)
	re.WriteString("$")
	return re.String()
}

func tokensRegexp(re *strings.Builder, tokens []globToken) {
	for _, t := range tokens {
		switch t.kind {
		case tokenLiteral:
			re.WriteString(escapedLiteral(t.literal))
		case tokenAny:
			re.WriteString("[^/]")
		case tokenZeroOrMore:
			re.WriteString("[^/]*")
		case tokenRecursivePrefix:
			re.WriteString("(?:/?|.*/)")
		case tokenRecursiveSuffix:
			re.WriteString("/.*")
		case tokenRecursiveZeroOrMore:
			re.WriteString("(?:/|/.*/)")
		case tokenClass:
			re.WriteString("[")
			if t.negated {
				re.WriteString("^")
			}
			for _, r := range t.ranges {
				re.WriteString(escapedLiteral(r[0]))
				if r[0] != r[1] {
					re.WriteString("-")
					re.WriteString(escapedLiteral(r[1]))
				}
			}
			re.WriteString("]")
		case tokenAlternates:
			var parts []string
			for _, alternate := range t.alternates {
				var part strings.Builder
				tokensRegexp(&part, alternate)
				if part.Len() > 0 {
					parts = append(parts, part.String())
				}
			}
			if len(parts) > 0 {
				re.WriteString("(?:" + strings.Join(parts, "|") + ")")
			}
		}
	}
}

// escapedLiteral is globset's char_to_escaped_literal: the character's UTF-8
// bytes, ASCII ones escaped as regex_syntax escapes them, others as \xNN.
func escapedLiteral(c rune) string {
	var buf [utf8.UTFMax]byte
	n := utf8.EncodeRune(buf[:], c)
	var out strings.Builder
	for _, b := range buf[:n] {
		if b <= 0x7f {
			if strings.IndexByte(`\.+*?()|[]{}^$#&-~`, b) >= 0 {
				out.WriteByte('\\')
			}
			out.WriteByte(b)
		} else {
			fmt.Fprintf(&out, `\x{%02x}`, b)
		}
	}
	return out.String()
}

// compileGlob parses glob and compiles its expression; an error refuses the
// line, as GitignoreBuilder::add_line does.
func compileGlob(glob string) (*regexp.Regexp, error) {
	p := &globParser{chars: []rune(glob), branches: [][]globToken{nil}}
	if err := p.parse(); err != nil {
		return nil, err
	}
	if len(p.branches) > 1 {
		return nil, errUnclosedAlternates
	}
	return regexp.Compile(globRegexp(p.branches[0]))
}

// latin1 carries each byte of s as the rune of the same value, the form
// compiled globs match.
func latin1(s string) string {
	runes := make([]rune, len(s))
	for i := 0; i < len(s); i++ {
		runes[i] = rune(s[i])
	}
	return string(runes)
}
