// Ported from Ada 2.9.2 (MIT, Copyright 2023 Yagiz Nizipli and Daniel Lemire);
// see LICENSES/ada-LICENSE-MIT.

package formats

import (
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// UTS #46 statuses in uts46Table (tables_ada.go).
const (
	uts46Ignored uint8 = iota
	uts46Valid
	uts46Disallowed
	uts46Mapped
)

type uts46Entry struct {
	start  rune
	status uint8
	offset uint16 // into uts46Mappings, for uts46Mapped
	length uint8
}

// bidiClass is a Bidi_Class value, in the order of Ada's direction enum; a
// code point outside every range of Ada's dir_table has none (bidiNONE).
type bidiClass uint8

const (
	bidiNONE bidiClass = iota
	bidiBN
	bidiCS
	bidiES
	bidiON
	bidiEN
	bidiL
	bidiR
	bidiNSM
	bidiAL
	bidiAN
	bidiET
	bidiWS
	bidiRLO
	bidiLRO
	bidiPDF
	bidiRLE
	bidiRLI
	bidiFSI
	bidiPDI
	bidiLRI
	bidiB
	bidiS
	bidiLRE
)

type bidiRange struct {
	lo, hi rune
	class  bidiClass
}

// domainToASCII is Ada's ada::idna::to_ascii (UTS #46 ToASCII with
// CheckHyphens false, CheckBidi and CheckJoiners true, nontransitional,
// as the WHATWG URL standard asks), reporting failure instead of returning
// "". Labels are separated only by "." after mapping, and an empty label is
// kept as is.
func domainToASCII(domain string) (string, bool) {
	if isASCII(domain) {
		return asciiDomainToASCII(domain)
	}
	if !utf8.ValidString(domain) {
		return "", false // the percent-decoded bytes are not UTF-8
	}
	mapped, ok := uts46Map([]rune(domain))
	if !ok {
		return "", false
	}
	mapped = []rune(norm.NFC.String(string(mapped)))
	var out strings.Builder
	for i, label := range splitRunes(mapped, '.') {
		if i > 0 {
			out.WriteByte('.')
		}
		switch {
		case len(label) == 0:
		case hasPrefixRunes(label, "xn--"):
			for _, r := range label {
				if r >= 0x80 {
					return "", false
				}
			}
			decoded, ok := adaPunycodeDecode(string(label[4:]))
			if !ok || !validALabel(decoded) {
				return "", false
			}
			out.WriteString(string(label))
		case !hasNonASCII(label):
			out.WriteString(string(label))
		default:
			if !adaLabelValid(label) {
				return "", false
			}
			encoded, ok := adaPunycodeEncode(label)
			if !ok {
				return "", false
			}
			out.WriteString("xn--")
			out.WriteString(encoded)
		}
	}
	return out.String(), true
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// asciiDomainToASCII is Ada's from_ascii_to_ascii: lowercase, then check
// each "xn--" label. Ada 2.9.2 does not reject an A-label that decodes to
// ASCII alone (xn--a-), on either path.
func asciiDomainToASCII(domain string) (string, bool) {
	domain = strings.ToLower(domain)
	for label := range strings.SplitSeq(domain, ".") {
		if !strings.HasPrefix(label, "xn--") {
			continue
		}
		decoded, ok := adaPunycodeDecode(label[4:])
		if !ok || !validALabel(decoded) {
			return "", false
		}
	}
	return domain, true
}

// validALabel is what Ada asks of a decoded "xn--" label: unchanged by the
// mapping and by NFC, not empty, and valid.
func validALabel(decoded []rune) bool {
	mapped, ok := uts46Map(decoded)
	if !ok || !slices.Equal(mapped, decoded) || norm.NFC.String(string(decoded)) != string(decoded) {
		return false
	}
	return len(decoded) > 0 && adaLabelValid(decoded)
}

func splitRunes(rs []rune, sep rune) [][]rune {
	var out [][]rune
	start := 0
	for i, r := range rs {
		if r == sep {
			out = append(out, rs[start:i])
			start = i + 1
		}
	}
	return append(out, rs[start:])
}

func hasPrefixRunes(rs []rune, prefix string) bool {
	if len(rs) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		if rs[i] != rune(prefix[i]) {
			return false
		}
	}
	return true
}

// uts46Map is Ada's map: drop ignored code points, replace mapped ones, and
// fail on a disallowed one (code points past U+10FFFF land in the last,
// disallowed, entry).
func uts46Map(in []rune) ([]rune, bool) {
	out := make([]rune, 0, len(in))
	for _, r := range in {
		e := uts46Lookup(r)
		switch e.status {
		case uts46Ignored:
		case uts46Valid:
			out = append(out, r)
		case uts46Disallowed:
			return nil, false
		default:
			out = append(out, uts46Mappings[e.offset:int(e.offset)+int(e.length)]...)
		}
	}
	return out, true
}

func uts46Lookup(r rune) uts46Entry {
	i := sort.Search(len(uts46Table), func(i int) bool { return uts46Table[i].start > r })
	if i == 0 {
		return uts46Table[0]
	}
	return uts46Table[i-1]
}

func adaBidi(r rune) bidiClass {
	i := sort.Search(len(adaBidiTable), func(i int) bool { return adaBidiTable[i].hi >= r })
	if i < len(adaBidiTable) && r >= adaBidiTable[i].lo {
		return adaBidiTable[i].class
	}
	return bidiNONE
}

func inSorted(list []rune, r rune) bool {
	_, found := slices.BinarySearch(list, r)
	return found
}

// adaLabelValid is Ada's is_label_valid, kept with its shortcuts: the first
// ZWNJ or ZWJ decides the label outright (a virama before it accepts it,
// skipping the bidi rule), a ZWNJ's joining context is any L/D code point
// before it and any R/D after it, and the RFC 5893 rule applies to each
// right-to-left label on its own, its LTR branch never checking the last
// non-NSM code point.
func adaLabelValid(label []rune) bool {
	if len(label) == 0 {
		return true
	}
	if inSorted(adaCombining[:], label[0]) {
		return false
	}
	for i, r := range label {
		if r != 0x200c && r != 0x200d {
			continue
		}
		if i > 0 && inSorted(adaVirama[:], label[i-1]) {
			return true
		}
		if r == 0x200d || i+1 >= len(label) {
			return false
		}
		return slices.ContainsFunc(label[:i], func(r rune) bool {
			return inSorted(adaJoiningL[:], r) || inSorted(adaJoiningD[:], r)
		}) && slices.ContainsFunc(label[i+1:], func(r rune) bool {
			return inSorted(adaJoiningR[:], r) || inSorted(adaJoiningD[:], r)
		})
	}
	last := -1
	rtl := false
	for i, r := range label {
		switch adaBidi(r) {
		case bidiNSM:
		case bidiR, bidiAL, bidiAN:
			rtl = true
			last = i
		default:
			last = i
		}
	}
	if last < 0 {
		return false // nothing but NSM
	}
	if !rtl {
		return true
	}
	if adaBidi(label[0]) == bidiL {
		for _, r := range label[:last] {
			switch adaBidi(r) {
			case bidiL, bidiEN, bidiES, bidiCS, bidiET, bidiON, bidiBN, bidiNSM:
			default:
				return false
			}
		}
		return true
	}
	hasEN, hasAN := false, false
	for i, r := range label[:last+1] {
		d := adaBidi(r)
		switch d {
		case bidiEN:
			hasEN = true
		case bidiAN:
			hasAN = true
		}
		if hasEN && hasAN {
			return false
		}
		switch d {
		case bidiR, bidiAL, bidiAN, bidiEN, bidiES, bidiCS, bidiET, bidiON, bidiBN, bidiNSM:
		default:
			return false
		}
		if i == last && d != bidiR && d != bidiAL && d != bidiAN && d != bidiEN {
			return false
		}
	}
	return true
}

// adaPunycodeDecode is Ada's punycode_to_utf32 with its int32 overflow
// checks; it does not bound the code points (the mapping rejects them).
func adaPunycodeDecode(input string) ([]rune, bool) {
	var out []rune
	n, i, bias := int64(punyInitialN), int64(0), int64(punyInitialBias)
	if end := strings.LastIndexByte(input, '-'); end >= 0 {
		for j := 0; j < end; j++ {
			if input[j] >= 0x80 {
				return nil, false
			}
			out = append(out, rune(input[j]))
		}
		input = input[end+1:]
	}
	const maxInt32 = 1<<31 - 1
	for input != "" {
		oldi, w := i, int64(1)
		for k := int64(punyBase); ; k += punyBase {
			if input == "" {
				return nil, false
			}
			c := input[0]
			input = input[1:]
			var digit int64
			switch {
			case c >= 'a' && c <= 'z':
				digit = int64(c - 'a')
			case c >= '0' && c <= '9':
				digit = int64(c-'0') + 26
			default:
				return nil, false
			}
			if digit > (maxInt32-i)/w {
				return nil, false
			}
			i += digit * w
			t := k - bias
			switch {
			case k <= bias:
				t = punyTMin
			case k >= bias+punyTMax:
				t = punyTMax
			}
			if digit < t {
				break
			}
			if w > maxInt32/(punyBase-t) {
				return nil, false
			}
			w *= punyBase - t
		}
		written := int64(len(out)) + 1
		bias = adaAdapt(i-oldi, written, oldi == 0)
		if i/written > maxInt32-n {
			return nil, false
		}
		n += i / written
		i %= written
		if n < 0x80 {
			return nil, false
		}
		out = slices.Insert(out, int(i), rune(n))
		i++
	}
	return out, true
}

func adaAdapt(d, n int64, first bool) int64 {
	if first {
		d /= punyDamp
	} else {
		d /= 2
	}
	d += d / n
	k := int64(0)
	for d > ((punyBase-punyTMin)*punyTMax)/2 {
		d /= punyBase - punyTMin
		k += punyBase
	}
	return k + ((punyBase-punyTMin+1)*d)/(d+punySkew)
}

// adaPunycodeEncode is Ada's utf32_to_punycode with its overflow checks.
func adaPunycodeEncode(label []rune) (string, bool) {
	var out strings.Builder
	h := 0
	for _, r := range label {
		if r < 0x80 {
			h++
			out.WriteByte(byte(r))
		}
		if r > 0x10ffff || r >= 0xd880 && r < 0xe000 {
			return "", false
		}
	}
	b := h
	if b > 0 {
		out.WriteByte('-')
	}
	const maxInt32 = 1<<31 - 1
	n, d, bias := int64(punyInitialN), int64(0), int64(punyInitialBias)
	for h < len(label) {
		m := int64(0x10ffff)
		for _, r := range label {
			if int64(r) >= n && int64(r) < m {
				m = int64(r)
			}
		}
		if m-n > (maxInt32-d)/int64(h+1) {
			return "", false
		}
		d += (m - n) * int64(h+1)
		n = m
		for _, r := range label {
			if int64(r) < n {
				if d == maxInt32 {
					return "", false
				}
				d++
			}
			if int64(r) == n {
				q := d
				for k := int64(punyBase); ; k += punyBase {
					t := k - bias
					switch {
					case k <= bias:
						t = punyTMin
					case k >= bias+punyTMax:
						t = punyTMax
					}
					if q < t {
						break
					}
					out.WriteByte(punyDigit(t + (q-t)%(punyBase-t)))
					q = (q - t) / (punyBase - t)
				}
				out.WriteByte(punyDigit(q))
				bias = adaAdapt(d, int64(h+1), h == b)
				d = 0
				h++
			}
		}
		d++
		n++
	}
	return out.String(), true
}

func punyDigit(d int64) byte {
	if d < 26 {
		return byte('a' + d)
	}
	return byte('0' + d - 26)
}
