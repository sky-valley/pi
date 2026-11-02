// Adapted from TypeBox 1.3.27 (MIT, Copyright (c) 2017-2026 Haydn Paterson);
// see LICENSES/typebox-LICENSE.

package formats

import (
	"math"
	"unicode/utf16"
)

// RFC 3492 parameters.
const (
	punyBase        = 36
	punyTMin        = 1
	punyTMax        = 26
	punySkew        = 38
	punyDamp        = 700
	punyInitialBias = 72
	punyInitialN    = 128
)

// punyDecode is idna/format/puny.ts Decode, which returns the decoded label
// or throws. It computes in float64 exactly as the JavaScript does in
// doubles: nothing guards against overflow, so a long run of digits drives
// the values past 2^53 (and Adapt's `delta >> 1` through ToInt32) before
// String.fromCodePoint rejects the result. The code points come back the
// way JavaScript would iterate the decoded string: a decoded surrogate pair
// joins into one code point, a lone surrogate stays a code point of its own.
func punyDecode(input string) ([]rune, bool) {
	for i := 0; i < len(input); i++ {
		if input[i] >= 0x80 {
			return nil, false // charCodeAt >= 128 before the delimiter, or a bad digit after it
		}
	}
	var output []float64
	n, i, bias := float64(punyInitialN), 0.0, float64(punyInitialBias)
	delim := -1
	for j := len(input) - 1; j >= 0; j-- {
		if input[j] == '-' {
			delim = j
			break
		}
	}
	for j := 0; j < delim; j++ {
		output = append(output, float64(input[j]))
	}
	in := 0
	if delim >= 0 {
		in = delim + 1
	}
	for in < len(input) {
		oldi, w, k := i, 1.0, float64(punyBase)
		for {
			if in >= len(input) {
				return nil, false
			}
			c := input[in]
			in++
			var digit float64
			switch {
			case c >= 'a' && c <= 'z':
				digit = float64(c - 'a')
			case c >= '0' && c <= '9':
				digit = float64(c-'0') + 26
			default:
				return nil, false
			}
			i += digit * w
			var t float64
			switch {
			case k <= bias:
				t = punyTMin
			case k >= bias+punyTMax:
				t = punyTMax
			default:
				t = k - bias
			}
			if digit < t {
				break
			}
			w *= punyBase - t
			k += punyBase
		}
		outLen := float64(len(output) + 1)
		bias = punyAdapt(i-oldi, outLen, oldi == 0)
		n += math.Floor(i / outLen)
		i = math.Mod(i, outLen)
		output = insertAt(output, i, n)
		i++
	}
	// String.fromCodePoint(...output) throws on anything but an integer in [0, 0x10FFFF].
	units := make([]uint16, 0, len(output))
	for _, cp := range output {
		if !(cp >= 0 && cp <= 0x10ffff) || cp != math.Trunc(cp) {
			return nil, false
		}
		if cp >= 0x10000 {
			hi, lo := utf16.EncodeRune(rune(cp))
			units = append(units, uint16(hi), uint16(lo))
		} else {
			units = append(units, uint16(cp))
		}
	}
	return decodeUTF16(units), true
}

// insertAt is Array.prototype.splice(i, 0, v) for the indices punyDecode
// produces: ToIntegerOrInfinity(NaN) is 0, and a finite index is in range.
func insertAt(xs []float64, at, v float64) []float64 {
	idx := 0
	if !math.IsNaN(at) {
		idx = int(math.Max(0, math.Min(at, float64(len(xs)))))
	}
	xs = append(xs, 0)
	copy(xs[idx+1:], xs[idx:])
	xs[idx] = v
	return xs
}

// decodeUTF16 iterates UTF-16 code units by code point, as a JavaScript
// for-of does: a well-formed pair is one code point, a lone surrogate is
// itself.
func decodeUTF16(units []uint16) []rune {
	out := make([]rune, 0, len(units))
	for i := 0; i < len(units); i++ {
		u := rune(units[i])
		if utf16.IsSurrogate(u) && u < 0xdc00 && i+1 < len(units) {
			if r := utf16.DecodeRune(u, rune(units[i+1])); r != 0xfffd {
				out = append(out, r)
				i++
				continue
			}
		}
		out = append(out, u)
	}
	return out
}

// punyAdapt is puny.ts Adapt over doubles; `delta >> 1` truncates through
// ToInt32 first.
func punyAdapt(delta, numPoints float64, firstTime bool) float64 {
	if firstTime {
		delta = math.Floor(delta / punyDamp)
	} else {
		delta = float64(toInt32(delta) >> 1)
	}
	delta += math.Floor(delta / numPoints)
	k := 0.0
	for delta > ((punyBase-punyTMin)*punyTMax)>>1 {
		delta = math.Floor(delta / (punyBase - punyTMin))
		k += punyBase
	}
	return k + math.Floor(((punyBase-punyTMin+1)*delta)/(delta+punySkew))
}

// toInt32 is ECMAScript ToInt32.
func toInt32(f float64) int32 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	m := math.Mod(math.Trunc(f), 1<<32)
	if m < 0 {
		m += 1 << 32
	}
	return int32(uint32(m))
}

// punyEncodedLen is the length of puny.ts Encode(label): the Punycode
// payload of a label's code points, whose digits are all ASCII.
func punyEncodedLen(cps []rune) int {
	basic := 0
	for _, r := range cps {
		if r < 0x80 {
			basic++
		}
	}
	length := basic
	if basic > 0 {
		length++ // the "-" delimiter
	}
	n, delta, bias := rune(punyInitialN), 0, float64(punyInitialBias)
	for handled := basic; handled < len(cps); {
		m := rune(math.MaxInt32)
		for _, r := range cps {
			if r >= n && r < m {
				m = r
			}
		}
		delta += int(m-n) * (handled + 1)
		n = m
		for _, r := range cps {
			if r < n {
				delta++
			}
			if r == n {
				q := delta
				for k := punyBase; ; k += punyBase {
					t := punyTMax
					switch b := int(bias); {
					case k <= b:
						t = punyTMin
					case k < b+punyTMax:
						t = k - b
					}
					if q < t {
						break
					}
					length++
					q = (q - t) / (punyBase - t)
				}
				length++
				bias = punyAdapt(float64(delta), float64(handled+1), handled == basic)
				delta = 0
				handled++
			}
		}
		delta++
		n++
	}
	return length
}
