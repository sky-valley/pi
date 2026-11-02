package schema

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf16"
)

// pattern is a compiled `pattern`/`patternProperties` regular expression. A
// nil re is a pattern RE2 cannot express (a lookaround, a backreference, a
// Unicode property RE2 lacks): it matches every string, as the Go port's
// earlier validator treated such patterns.
type pattern struct {
	re *regexp.Regexp
}

// test is RegExp.prototype.test: whether the pattern matches anywhere in s.
func (p *pattern) test(s string) bool {
	return p.re == nil || p.re.MatchString(s)
}

// regexes is shared.ts's cache of regular expressions by source. Upstream
// holds them weakly so an unused one can be collected; a Go regexp is small
// and pattern sources are few, so this cache keeps them, and also keeps a
// source's SyntaxError.
var regexes sync.Map // source → *pattern or *regexSyntaxError

// sharedRegex is shared.ts sharedRegex: the regular expression of a pattern
// source, shared by source. Patterns are ECMAScript regular expressions with
// the `u` flag; a source invalid with it compiles without it. A source
// invalid either way is an error with V8's SyntaxError message.
func sharedRegex(source string) (*pattern, error) {
	if hit, ok := regexes.Load(source); ok {
		if err, ok := hit.(*regexSyntaxError); ok {
			return nil, err
		}
		return hit.(*pattern), nil
	}
	p, err := compileRegex(source)
	if err != nil {
		regexes.Store(source, err)
		return nil, err
	}
	actual, _ := regexes.LoadOrStore(source, p)
	return actual.(*pattern), nil
}

// sharedRegexCount is the number of cached sources (for tests).
func sharedRegexCount() int {
	n := 0
	regexes.Range(func(any, any) bool { n++; return true })
	return n
}

// regexSyntaxError is the SyntaxError `new RegExp(source)` throws.
type regexSyntaxError struct {
	source, reason string
}

func (e *regexSyntaxError) Error() string {
	return fmt.Sprintf("Invalid regular expression: /%s/: %s", e.source, e.reason)
}

func compileRegex(source string) (*pattern, error) {
	out, err := translateRegex(source, true)
	if err != nil && !errors.Is(err, errUnsupportedRegex) {
		out, err = translateRegex(source, false)
	}
	var syntax *regexSyntaxError
	switch {
	case errors.As(err, &syntax):
		syntax.source = source
		return nil, syntax
	case err != nil:
		return &pattern{}, nil
	}
	re, err := regexp.Compile(out)
	if err != nil {
		// What JavaScript accepts and RE2 does not (a repeat count over 1000).
		return &pattern{}, nil
	}
	return &pattern{re: re}, nil
}

// errUnsupportedRegex marks a valid ECMAScript pattern RE2 cannot express.
var errUnsupportedRegex = errors.New("pattern needs a feature RE2 lacks")

// errUnicodeMode marks a pattern the `u` flag rejects; it is read again
// without the flag (Annex B syntax).
var errUnicodeMode = errors.New("invalid with the u flag")

// translateRegex rewrites an ECMAScript pattern as RE2 syntax with the same
// meaning, where the two differ:
//
//   - `.` excludes all four line terminators (\n, \r, U+2028, U+2029);
//   - `\s` and `\S` are ECMAScript's WhiteSpace and LineTerminator set (RE2's
//     is ASCII);
//   - `[]` matches nothing and `[^]` anything; `]`, `[` and `-` in a class
//     are literal where ECMAScript says so;
//   - `\cX`, `\0`, `\uXXXX` (a surrogate pair joined), `\u{X}`, `\xHH`,
//     legacy octal escapes and identity escapes become \x{X};
//   - `\p{…}` takes ECMAScript's General_Category names and aliases and
//     Script names (Script_Extensions, script codes and binary properties
//     are unsupported); `(?<name>` becomes `(?P<name>`.
//
// It also reports what makes V8 throw — an unterminated group or class, an
// unmatched ')', nothing to repeat, `{}` bounds out of order, a range out of
// order, an invalid group, a trailing backslash — and, unless unicode, reads
// Annex B's lenient forms (a lone `{`, `}` or `]`, `\c` without a letter, an
// identity escape of any character). The `u` flag's code-point matching is
// RE2's own; without it, ECMAScript matches UTF-16 code units, which the
// port does not model.
func translateRegex(source string, unicodeMode bool) (string, error) {
	t := regexTranslator{src: []rune(source), unicode: unicodeMode}
	t.countGroups()
	if err := t.run(); err != nil {
		return "", err
	}
	if t.unsupported {
		return "", errUnsupportedRegex
	}
	return t.out.String(), nil
}

type regexTranslator struct {
	src         []rune
	pos         int
	unicode     bool
	out         strings.Builder
	groups      int  // capturing groups in the whole pattern
	named       bool // whether the pattern has a named group
	unsupported bool
}

func syntaxError(reason string) error { return &regexSyntaxError{reason: reason} }

func (t *regexTranslator) countGroups() {
	inClass := false
	for i := 0; i < len(t.src); i++ {
		switch c := t.src[i]; {
		case c == '\\':
			i++
		case inClass:
			inClass = c != ']'
		case c == '[':
			inClass = true
		case c == '(':
			if i+1 < len(t.src) && t.src[i+1] == '?' {
				if i+2 < len(t.src) && t.src[i+2] == '<' && i+3 < len(t.src) && t.src[i+3] != '=' && t.src[i+3] != '!' {
					t.groups++
					t.named = true
				}
				continue
			}
			t.groups++
		}
	}
}

func (t *regexTranslator) peek(n int) rune {
	if t.pos+n < len(t.src) {
		return t.src[t.pos+n]
	}
	return -1
}

func (t *regexTranslator) run() error {
	depth := 0
	canRepeat := false
	for t.pos < len(t.src) {
		c := t.src[t.pos]
		switch c {
		case '\\':
			atom, err := t.escape()
			if err != nil {
				return err
			}
			canRepeat = atom
			continue
		case '^', '$':
			t.out.WriteRune(c)
			canRepeat = false
		case '.':
			t.out.WriteString(`[^\n\r\x{2028}\x{2029}]`)
			canRepeat = true
		case '(':
			if err := t.group(); err != nil {
				return err
			}
			depth++
			canRepeat = false
			continue
		case ')':
			if depth == 0 {
				return syntaxError("Unmatched ')'")
			}
			depth--
			t.out.WriteByte(')')
			canRepeat = true
		case '|':
			t.out.WriteByte('|')
			canRepeat = false
		case '*', '+', '?':
			if !canRepeat {
				return syntaxError("Nothing to repeat")
			}
			t.out.WriteRune(c)
			t.lazy()
			canRepeat = false
		case '{':
			text, lo, hi, ok := t.bounds()
			switch {
			case ok && !canRepeat:
				return syntaxError("Nothing to repeat")
			case ok && hi >= 0 && lo > hi:
				return syntaxError("numbers out of order in {} quantifier")
			case ok:
				t.out.WriteString(text)
				t.pos += len([]rune(text)) - 1
				t.lazy()
				canRepeat = false
			case t.unicode:
				return syntaxError("Lone quantifier brackets")
			default:
				t.literal(c)
				canRepeat = true
			}
		case '}', ']':
			if t.unicode {
				return syntaxError("Lone quantifier brackets")
			}
			t.literal(c)
			canRepeat = true
		case '[':
			if err := t.class(); err != nil {
				return err
			}
			canRepeat = true
			continue
		default:
			t.literal(c)
			canRepeat = true
		}
		t.pos++
	}
	if depth > 0 {
		return syntaxError("Unterminated group")
	}
	return nil
}

// lazy copies the `?` of a lazy quantifier after the quantifier at pos.
func (t *regexTranslator) lazy() {
	if t.peek(1) == '?' {
		t.pos++
		t.out.WriteByte('?')
	}
}

// bounds reads `{n}`, `{n,}` or `{n,m}` at pos: its text and bounds (hi -1
// for none); ok is false when the brace starts no quantifier.
func (t *regexTranslator) bounds() (text string, lo, hi int, ok bool) {
	end := t.pos + 1
	for end < len(t.src) && t.src[end] != '}' {
		end++
	}
	if end >= len(t.src) {
		return "", 0, 0, false
	}
	inner := string(t.src[t.pos+1 : end])
	a, b, comma := strings.Cut(inner, ",")
	if !digitsOnly(a) || (comma && b != "" && !digitsOnly(b)) {
		return "", 0, 0, false
	}
	lo = atoiSaturating(a)
	hi = lo
	if comma {
		hi = -1
		if b != "" {
			hi = atoiSaturating(b)
		}
	}
	return "{" + inner + "}", lo, hi, true
}

func digitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func atoiSaturating(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return int(^uint(0) >> 1)
	}
	return n
}

// group reads the opening of a group at pos.
func (t *regexTranslator) group() error {
	if t.peek(1) != '?' {
		t.pos++
		t.out.WriteByte('(')
		return nil
	}
	switch t.peek(2) {
	case ':':
		t.pos += 3
		t.out.WriteString("(?:")
		return nil
	case '=', '!':
		t.pos += 3
		t.unsupported = true
		t.out.WriteString("(?:")
		return nil
	case '<':
		if t.peek(3) == '=' || t.peek(3) == '!' {
			t.pos += 4
			t.unsupported = true
			t.out.WriteString("(?:")
			return nil
		}
		end := t.pos + 3
		for end < len(t.src) && t.src[end] != '>' {
			if !isIdentifierRune(t.src[end], end == t.pos+3) {
				return syntaxError("Invalid capture group name")
			}
			end++
		}
		if end >= len(t.src) || end == t.pos+3 {
			return syntaxError("Invalid capture group name")
		}
		t.out.WriteString("(?P<" + string(t.src[t.pos+3:end]) + ">")
		t.pos = end + 1
		return nil
	}
	return syntaxError("Invalid group")
}

func isIdentifierRune(r rune, first bool) bool {
	if r == '$' || r == '_' || unicode.IsLetter(r) {
		return true
	}
	return !first && (unicode.IsDigit(r) || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Mc, r) || unicode.Is(unicode.Pc, r))
}

// literal writes one character to match as itself.
func (t *regexTranslator) literal(r rune) {
	if r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
		t.out.WriteRune(r)
		return
	}
	fmt.Fprintf(&t.out, `\x{%X}`, r)
}

// jsWhitespace is ECMAScript's \s: WhiteSpace and LineTerminator, as class
// members.
const jsWhitespace = `\t\n\x{B}\f\r\x{20}\x{A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}`

// jsNonWhitespace is ECMAScript's \S as class members: the complement of
// jsWhitespace, spelled out because RE2 cannot negate a list inside a class.
const jsNonWhitespace = `\x{0}-\x{8}\x{E}-\x{1F}\x{21}-\x{9F}\x{A1}-\x{167F}\x{1681}-\x{1FFF}` +
	`\x{200B}-\x{2027}\x{202A}-\x{202E}\x{2030}-\x{205E}\x{2060}-\x{2FFF}\x{3001}-\x{FEFE}\x{FF00}-\x{10FFFF}`

// classItem is one class atom: a single character (set == "") or a set in
// class-member syntax.
type classItem struct {
	char rune
	set  string
}

// escape reads an escape outside a class at pos; atom is whether a
// quantifier may follow it.
func (t *regexTranslator) escape() (atom bool, err error) {
	next := t.peek(1)
	switch next {
	case -1:
		return false, syntaxError(`\ at end of pattern`)
	case 'b', 'B':
		t.pos += 2
		t.out.WriteString(`\` + string(next))
		return false, nil
	case 's':
		t.pos += 2
		t.out.WriteString("[" + jsWhitespace + "]")
		return true, nil
	case 'S':
		t.pos += 2
		t.out.WriteString("[^" + jsWhitespace + "]")
		return true, nil
	case 'd', 'D', 'w', 'W':
		t.pos += 2
		t.out.WriteString(`\` + string(next))
		return true, nil
	case 'p', 'P':
		if t.unicode {
			set, negated, err := t.property()
			if err != nil {
				return false, err
			}
			if negated {
				t.out.WriteString("[^" + set + "]")
			} else {
				t.out.WriteString("[" + set + "]")
			}
			return true, nil
		}
	case 'k':
		if t.unicode || t.named {
			t.unsupported = true
			end := t.pos + 2
			for end < len(t.src) && t.src[end] != '>' {
				end++
			}
			if t.peek(2) != '<' || end >= len(t.src) {
				return false, syntaxError("Invalid named reference")
			}
			t.pos = end + 1
			return true, nil
		}
	case '1', '2', '3', '4', '5', '6', '7', '8', '9':
		if n := t.decimalEscape(); n <= t.groups {
			t.unsupported = true
			return true, nil
		}
		if t.unicode {
			return false, syntaxError("Invalid escape")
		}
	}
	item, err := t.characterEscape(false)
	if err != nil {
		return false, err
	}
	t.literal(item.char)
	return true, nil
}

// decimalEscape reads the digits of `\N` at pos without moving; it is the
// group number they name.
func (t *regexTranslator) decimalEscape() int {
	end := t.pos + 1
	for end < len(t.src) && t.src[end] >= '0' && t.src[end] <= '9' {
		end++
	}
	if n, err := strconv.Atoi(string(t.src[t.pos+1 : end])); err == nil {
		if n <= t.groups {
			t.pos = end
		}
		return n
	}
	return int(^uint(0) >> 1)
}

// characterEscape reads an escape that stands for one character, at pos.
func (t *regexTranslator) characterEscape(inClass bool) (classItem, error) {
	next := t.peek(1)
	t.pos += 2
	switch next {
	case 't':
		return classItem{char: '\t'}, nil
	case 'n':
		return classItem{char: '\n'}, nil
	case 'v':
		return classItem{char: '\v'}, nil
	case 'f':
		return classItem{char: '\f'}, nil
	case 'r':
		return classItem{char: '\r'}, nil
	case 'c':
		if l := t.peek(0); ('a' <= l && l <= 'z') || ('A' <= l && l <= 'Z') || (inClass && !t.unicode && (('0' <= l && l <= '9') || l == '_')) {
			t.pos++
			return classItem{char: l % 32}, nil
		}
		if t.unicode {
			return classItem{}, syntaxError("Invalid unicode escape")
		}
		t.pos-- // a literal backslash; the 'c' is read next
		return classItem{char: '\\'}, nil
	case '0':
		if d := t.peek(0); d >= '0' && d <= '9' {
			if t.unicode {
				return classItem{}, syntaxError("Invalid decimal escape")
			}
			t.pos--
			return classItem{char: t.octal()}, nil
		}
		return classItem{char: 0}, nil
	case '1', '2', '3', '4', '5', '6', '7':
		if t.unicode {
			return classItem{}, syntaxError("Invalid escape")
		}
		t.pos--
		return classItem{char: t.octal()}, nil
	case 'x':
		if r, ok := t.hex(2); ok {
			return classItem{char: r}, nil
		}
		if t.unicode {
			return classItem{}, syntaxError("Invalid escape")
		}
		return classItem{char: 'x'}, nil
	case 'u':
		return t.unicodeEscape()
	case '-':
		if !inClass && t.unicode {
			return classItem{}, syntaxError("Invalid escape")
		}
		return classItem{char: '-'}, nil
	case '^', '$', '\\', '.', '*', '+', '?', '(', ')', '[', ']', '{', '}', '|', '/':
		return classItem{char: next}, nil
	}
	if t.unicode {
		return classItem{}, syntaxError("Invalid escape")
	}
	return classItem{char: next}, nil
}

// octal reads a legacy octal escape (at most \377) after the backslash at
// pos-1; pos is at its first digit.
func (t *regexTranslator) octal() rune {
	var r rune
	for n := 0; n < 3; n++ {
		d := t.peek(0)
		if d < '0' || d > '7' || r*8+(d-'0') > 0377 {
			break
		}
		r = r*8 + (d - '0')
		t.pos++
	}
	return r
}

// hex reads n hex digits at pos.
func (t *regexTranslator) hex(n int) (rune, bool) {
	if t.pos+n > len(t.src) {
		return 0, false
	}
	v, err := strconv.ParseUint(string(t.src[t.pos:t.pos+n]), 16, 32)
	if err != nil {
		return 0, false
	}
	t.pos += n
	return rune(v), true
}

// unicodeEscape reads the rest of `\u` (pos is after the 'u').
func (t *regexTranslator) unicodeEscape() (classItem, error) {
	if t.unicode && t.peek(0) == '{' {
		end := t.pos + 1
		for end < len(t.src) && t.src[end] != '}' {
			end++
		}
		v, err := strconv.ParseUint(string(t.src[t.pos+1:min(end, len(t.src))]), 16, 32)
		if end >= len(t.src) || err != nil || v > unicode.MaxRune {
			return classItem{}, syntaxError("Invalid Unicode escape")
		}
		t.pos = end + 1
		return t.codePoint(rune(v))
	}
	r, ok := t.hex(4)
	if !ok {
		if t.unicode {
			return classItem{}, syntaxError("Invalid Unicode escape")
		}
		return classItem{char: 'u'}, nil
	}
	if utf16.IsSurrogate(r) && r < 0xDC00 && t.peek(0) == '\\' && t.peek(1) == 'u' {
		save := t.pos
		t.pos += 2
		if lo, ok := t.hex(4); ok && lo >= 0xDC00 && lo <= 0xDFFF {
			return classItem{char: utf16.DecodeRune(r, lo)}, nil
		}
		t.pos = save
	}
	return t.codePoint(r)
}

// codePoint is one escaped character; half a surrogate pair matches a code
// unit, which RE2 cannot.
func (t *regexTranslator) codePoint(r rune) (classItem, error) {
	if utf16.IsSurrogate(r) {
		t.unsupported = true
		return classItem{char: unicode.ReplacementChar}, nil
	}
	return classItem{char: r}, nil
}

// property reads `\p{…}` or `\P{…}` at pos (unicode mode): the class
// members of the property and whether the escape negates them.
func (t *regexTranslator) property() (members string, negated bool, err error) {
	negated = t.peek(1) == 'P'
	if t.peek(2) != '{' {
		return "", false, syntaxError("Invalid property name")
	}
	end := t.pos + 3
	for end < len(t.src) && t.src[end] != '}' {
		end++
	}
	if end >= len(t.src) {
		return "", false, syntaxError("Invalid property name")
	}
	name := string(t.src[t.pos+3 : end])
	t.pos = end + 1
	set, ok := propertySet(name)
	switch {
	case !ok:
		return "", false, syntaxError("Invalid property name")
	case set == "":
		t.unsupported = true
		return `\x{0}`, negated, nil
	}
	return set, negated, nil
}

// propertySet is the class members of a property expression V8 accepts: ok
// false for a name V8 rejects, set "" for one RE2 cannot express.
func propertySet(expr string) (set string, ok bool) {
	name, value, hasValue := strings.Cut(expr, "=")
	if hasValue {
		switch name {
		case "General_Category", "gc":
			return generalCategory(value)
		case "Script", "sc":
			if _, ok := unicode.Scripts[value]; ok {
				return `\p{` + value + `}`, true
			}
			return "", true // a script code, or a script V8 knows and Go does not
		case "Script_Extensions", "scx":
			return "", true
		}
		return "", false
	}
	if set, ok := generalCategory(name); ok {
		return set, true
	}
	switch name {
	case "Any":
		return `\x{0}-\x{10FFFF}`, true
	case "ASCII":
		return `\x{0}-\x{7F}`, true
	}
	if binaryProperties[name] {
		return "", true
	}
	return "", false
}

func generalCategory(name string) (string, bool) {
	short, ok := generalCategories[name]
	switch {
	case !ok:
		return "", false
	case short == "LC":
		return `\p{Lu}\p{Ll}\p{Lt}`, true
	case short == "Cn":
		return "", true
	}
	return `\p{` + short + `}`, true
}

// generalCategories maps ECMAScript's General_Category values and aliases
// (PropertyValueAliases.txt) to the short name.
var generalCategories = func() map[string]string {
	m := map[string]string{}
	for _, aliases := range [][]string{
		{"C", "Other"}, {"Cc", "Control", "cntrl"}, {"Cf", "Format"}, {"Cn", "Unassigned"},
		{"Co", "Private_Use"}, {"Cs", "Surrogate"}, {"L", "Letter"}, {"LC", "Cased_Letter"},
		{"Ll", "Lowercase_Letter"}, {"Lm", "Modifier_Letter"}, {"Lo", "Other_Letter"},
		{"Lt", "Titlecase_Letter"}, {"Lu", "Uppercase_Letter"}, {"M", "Mark", "Combining_Mark"},
		{"Mc", "Spacing_Mark"}, {"Me", "Enclosing_Mark"}, {"Mn", "Nonspacing_Mark"}, {"N", "Number"},
		{"Nd", "Decimal_Number", "digit"}, {"Nl", "Letter_Number"}, {"No", "Other_Number"},
		{"P", "Punctuation", "punct"}, {"Pc", "Connector_Punctuation"}, {"Pd", "Dash_Punctuation"},
		{"Pe", "Close_Punctuation"}, {"Pf", "Final_Punctuation"}, {"Pi", "Initial_Punctuation"},
		{"Po", "Other_Punctuation"}, {"Ps", "Open_Punctuation"}, {"S", "Symbol"}, {"Sm", "Math_Symbol"},
		{"Sc", "Currency_Symbol"}, {"Sk", "Modifier_Symbol"}, {"So", "Other_Symbol"}, {"Z", "Separator"},
		{"Zl", "Line_Separator"}, {"Zp", "Paragraph_Separator"}, {"Zs", "Space_Separator"},
	} {
		for _, a := range aliases {
			m[a] = aliases[0]
		}
	}
	return m
}()

// binaryProperties are ECMAScript's binary Unicode properties (names and
// aliases besides Any and ASCII), which RE2 lacks.
var binaryProperties = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range strings.Fields(`ASCII_Hex_Digit AHex Alphabetic Alpha Assigned Bidi_Control Bidi_C
		Bidi_Mirrored Bidi_M Case_Ignorable CI Cased Changes_When_Casefolded CWCF Changes_When_Casemapped CWCM
		Changes_When_Lowercased CWL Changes_When_NFKC_Casefolded CWKCF Changes_When_Titlecased CWT
		Changes_When_Uppercased CWU Dash Default_Ignorable_Code_Point DI Deprecated Dep Diacritic Dia Emoji
		Emoji_Component EComp Emoji_Modifier EMod Emoji_Modifier_Base EBase Emoji_Presentation EPres
		Extended_Pictographic ExtPict Extender Ext Grapheme_Base Gr_Base Grapheme_Extend Gr_Ext Hex_Digit Hex
		IDS_Binary_Operator IDSB IDS_Trinary_Operator IDST ID_Continue IDC ID_Start IDS Ideographic Ideo
		Join_Control Join_C Logical_Order_Exception LOE Lowercase Lower Math Noncharacter_Code_Point NChar
		Pattern_Syntax Pat_Syn Pattern_White_Space Pat_WS Quotation_Mark QMark Radical Regional_Indicator RI
		Sentence_Terminal STerm Soft_Dotted SD Terminal_Punctuation Term Unified_Ideograph UIdeo Uppercase
		Upper Variation_Selector VS White_Space space XID_Continue XIDC XID_Start XIDS`) {
		m[n] = true
	}
	return m
}()

// class reads a character class at pos.
func (t *regexTranslator) class() error {
	t.pos++ // '['
	negated := false
	if t.peek(0) == '^' {
		negated = true
		t.pos++
	}
	if t.peek(0) == ']' {
		t.pos++
		if negated {
			t.out.WriteString(`[\x{0}-\x{10FFFF}]`)
		} else {
			t.out.WriteString(`[^\x{0}-\x{10FFFF}]`)
		}
		return nil
	}
	var members strings.Builder
	for {
		if t.pos >= len(t.src) {
			return syntaxError("Unterminated character class")
		}
		if t.src[t.pos] == ']' {
			t.pos++
			break
		}
		lo, err := t.classAtom()
		if err != nil {
			return err
		}
		if t.peek(0) == '-' && t.peek(1) != ']' && t.peek(1) != -1 {
			t.pos++
			hi, err := t.classAtom()
			if err != nil {
				return err
			}
			if lo.set != "" || hi.set != "" {
				if t.unicode {
					return syntaxError("Invalid character class")
				}
				writeClassItem(&members, lo)
				members.WriteString(`\x{2D}`)
				writeClassItem(&members, hi)
				continue
			}
			if lo.char > hi.char {
				return syntaxError("Range out of order in character class")
			}
			fmt.Fprintf(&members, `\x{%X}-\x{%X}`, lo.char, hi.char)
			continue
		}
		writeClassItem(&members, lo)
	}
	if negated {
		t.out.WriteString("[^" + members.String() + "]")
	} else {
		t.out.WriteString("[" + members.String() + "]")
	}
	return nil
}

func writeClassItem(b *strings.Builder, item classItem) {
	if item.set != "" {
		b.WriteString(item.set)
		return
	}
	fmt.Fprintf(b, `\x{%X}`, item.char)
}

// classAtom reads one character or class escape inside a class at pos.
func (t *regexTranslator) classAtom() (classItem, error) {
	c := t.src[t.pos]
	if c != '\\' {
		t.pos++
		return classItem{char: c}, nil
	}
	next := t.peek(1)
	switch next {
	case -1:
		return classItem{}, syntaxError(`\ at end of pattern`)
	case 'd', 'D', 'w', 'W':
		t.pos += 2
		return classItem{set: `\` + string(next)}, nil
	case 's':
		t.pos += 2
		return classItem{set: jsWhitespace}, nil
	case 'S':
		t.pos += 2
		return classItem{set: jsNonWhitespace}, nil
	case 'b':
		t.pos += 2
		return classItem{char: '\b'}, nil
	case 'B':
		if t.unicode {
			return classItem{}, syntaxError("Invalid class escape")
		}
		t.pos += 2
		return classItem{char: 'B'}, nil
	case 'p', 'P':
		if t.unicode {
			set, negated, err := t.property()
			if err != nil {
				return classItem{}, err
			}
			if negated {
				// RE2 negates one \p class inside a class, not a list.
				if !strings.HasPrefix(set, `\p{`) || strings.Count(set, `\p{`) != 1 || !strings.HasSuffix(set, "}") {
					t.unsupported = true
				}
				set = `\P` + strings.TrimPrefix(set, `\p`)
			}
			return classItem{set: set}, nil
		}
	case '8', '9':
		if t.unicode {
			return classItem{}, syntaxError("Invalid class escape")
		}
		t.pos += 2
		return classItem{char: next}, nil
	case 'k':
		if t.unicode {
			return classItem{}, syntaxError("Invalid class escape")
		}
	}
	return t.characterEscape(true)
}
