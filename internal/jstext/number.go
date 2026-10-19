package jstext

import (
	"encoding/json"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// NumberToString formats a float64 the way JavaScript's String(number) /
// Number.prototype.toString does.
//
// JS uses the shortest round-trippable decimal, switching to exponential
// notation only when the decimal exponent is >= 21 or <= -7. This differs from
// Go's FormatFloat('g'), which would render 1000000 as "1e+06" and 1e-7 as
// "1e-07"; JS renders them "1000000" and "1e-7".
func NumberToString(f float64) string {
	if math.IsInf(f, 1) {
		return "Infinity"
	}
	if math.IsInf(f, -1) {
		return "-Infinity"
	}
	if math.IsNaN(f) {
		return "NaN"
	}
	if f == 0 {
		// Negative zero stringifies to "0" in JS.
		return "0"
	}

	neg := math.Signbit(f)
	abs := math.Abs(f)

	// Shortest round-trippable significand and base-10 exponent.
	// 'e' format gives "d.dddde±dd"; parse mantissa digits + exponent k where
	// value = digits * 10^(k - (len(digits)-1)).
	mant := strconv.FormatFloat(abs, 'e', -1, 64)
	eIdx := strings.IndexByte(mant, 'e')
	digitsPart := mant[:eIdx]
	exp10, _ := strconv.Atoi(mant[eIdx+1:])

	// Strip the decimal point to get the bare significant digits.
	digits := strings.Replace(digitsPart, ".", "", 1)
	// n = exponent such that value = digits-as-integer-with-implied-point.
	// Per ECMA-262 Number::toString: let k = number of digits, and s = the
	// integer formed by the digits; n is the position of the decimal point.
	k := len(digits)
	n := exp10 + 1 // point sits after the first 'n' digits when n in (0,k]

	var out string
	switch {
	case k <= n && n <= 21:
		// Integer with trailing zeros: digits followed by (n-k) zeros.
		out = digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		// Decimal point inside the digit string.
		out = digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		// 0.00...digits
		out = "0." + strings.Repeat("0", -n) + digits
	default:
		// Exponential notation. Mantissa is first digit, optional ".rest",
		// exponent is (n-1) with explicit sign and no leading zero padding.
		e := n - 1
		var sign string
		if e >= 0 {
			sign = "+"
		} else {
			sign = "-"
			e = -e
		}
		if k == 1 {
			out = digits + "e" + sign + strconv.Itoa(e)
		} else {
			out = digits[:1] + "." + digits[1:] + "e" + sign + strconv.Itoa(e)
		}
	}

	if neg {
		return "-" + out
	}
	return out
}

// ToString is JavaScript's String(value) for a value Parse returns: "null",
// "true" or "false", a number as NumberToString writes it, a string as is, an
// array as Array.prototype.join(",") writes it (null elements as ""), and an
// object as "[object Object]".
//
// ok is false for a value with no string form, where String() throws a
// TypeError: an object with its own "toString" member, directly or inside an
// array. JSON cannot make that member callable, so String() falls back to
// valueOf, which returns the object itself.
func ToString(v any) (s string, ok bool) {
	switch x := v.(type) {
	case nil:
		return "null", true
	case bool:
		return strconv.FormatBool(x), true
	case json.Number:
		return NumberToString(Number(x)), true
	case string:
		return x, true
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			if e == nil {
				continue
			}
			if parts[i], ok = ToString(e); !ok {
				return "", false
			}
		}
		return strings.Join(parts, ","), true
	case map[string]any:
		if _, own := x["toString"]; own {
			return "", false
		}
	}
	return "[object Object]", true
}

// ToFixed is Number.prototype.toFixed(fractionDigits) (ECMA-262): x's exact
// binary value rounded to fractionDigits decimals, a tie going to the larger
// magnitude, with the sign written apart from the digits — so -0 prints as
// "0.000000" and a negative that rounds to zero keeps its "-". NaN, ±Infinity
// and a magnitude of 1e21 or more print as String(x). strconv.FormatFloat is
// not the same: it rounds a tie to even. fractionDigits must be 0-100.
func ToFixed(x float64, fractionDigits int) string {
	if math.IsNaN(x) || math.IsInf(x, 0) || math.Abs(x) >= 1e21 {
		return NumberToString(x)
	}
	sign := ""
	if x < 0 {
		sign, x = "-", -x
	}
	scaled := new(big.Rat).SetFloat64(x)
	scaled.Mul(scaled, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(fractionDigits)), nil)))
	// n = floor(scaled + 1/2): the nearest integer, the larger one on a tie.
	scaled.Add(scaled, big.NewRat(1, 2))
	digits := new(big.Int).Quo(scaled.Num(), scaled.Denom()).String()
	if fractionDigits == 0 {
		return sign + digits
	}
	if len(digits) <= fractionDigits {
		digits = strings.Repeat("0", fractionDigits+1-len(digits)) + digits
	}
	point := len(digits) - fractionDigits
	return sign + digits[:point] + "." + digits[point:]
}

// StringToNumber is ECMA-262 StringToNumber, the Number(string) coercion: the
// string trimmed of JavaScript whitespace, "" as 0, an unsigned 0x/0b/0o
// integer of any size, "Infinity" with an optional sign, or a signed decimal
// literal; anything else is NaN. Verified against node: " 12 "→12,
// "0x10"→16, "0b101"→5, "0o17"→15, "+5"→5, "1e3"→1000, ".5"→0.5, "5."→5,
// ""→0, "1e1000"→Infinity, and NaN for "1_0", "0x1p4", "-0x10", "12abc" and
// "NaN".
func StringToNumber(value string) float64 {
	s := Trim(value) // StringToNumber trims StrWhiteSpaceChar, trim's set
	if s == "" {
		return 0 // Number("") === 0
	}

	// Non-decimal integer literals: 0x/0X, 0b/0B, 0o/0O. No sign allowed.
	if len(s) > 2 && s[0] == '0' {
		var base int
		switch s[1] {
		case 'x', 'X':
			base = 16
		case 'b', 'B':
			base = 2
		case 'o', 'O':
			base = 8
		}
		if base != 0 {
			digits := s[2:]
			if !validDigits(digits, base) {
				return math.NaN()
			}
			// Arbitrary precision (JS allows >2^64), rounded to float64.
			n, ok := new(big.Int).SetString(digits, base)
			if !ok {
				return math.NaN()
			}
			f, _ := new(big.Float).SetInt(n).Float64()
			return f
		}
	}

	sign := 1.0
	rest := s
	switch s[0] {
	case '+':
		rest = s[1:]
	case '-':
		sign, rest = -1, s[1:]
	}
	if rest == "Infinity" {
		return sign * math.Inf(1)
	}
	// Validate StrUnsignedDecimalLiteral strictly before ParseFloat: Go's
	// ParseFloat accepts JS-invalid forms ("1_0", "0x1p4", "inf", "nan").
	if !isStrUnsignedDecimalLiteral(rest) {
		return math.NaN()
	}
	f, err := strconv.ParseFloat(rest, 64)
	if err != nil {
		if ne, isNum := err.(*strconv.NumError); isNum && ne.Err == strconv.ErrRange {
			// Overflow → ±Inf (like JS "1e1000" → Infinity); underflow → ~0.
			return sign * f
		}
		return math.NaN()
	}
	return sign * f
}

func validDigits(s string, base int) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		var d byte
		switch {
		case c >= '0' && c <= '9':
			d = c - '0'
		case c >= 'a' && c <= 'z':
			d = c - 'a' + 10
		case c >= 'A' && c <= 'Z':
			d = c - 'A' + 10
		default:
			return false
		}
		if int(d) >= base {
			return false
		}
	}
	return true
}

// isStrUnsignedDecimalLiteral validates ECMA-262 StrUnsignedDecimalLiteral:
// digits [. digits] [ExponentPart] | . digits [ExponentPart].
func isStrUnsignedDecimalLiteral(s string) bool {
	i := 0
	digits := func() int {
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		return i - start
	}
	intLen := digits()
	fracLen := 0
	if i < len(s) && s[i] == '.' {
		i++
		fracLen = digits()
	}
	if intLen == 0 && fracLen == 0 {
		return false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		if digits() == 0 {
			return false
		}
	}
	return i == len(s)
}

// ToNumber is JavaScript's Number(value) for a value Parse returns: null 0,
// booleans 0 and 1, a number as Number reads it, a string by StringToNumber,
// and an array or object by the number its string form spells (ToPrimitive
// with hint number tries valueOf first, which never yields a primitive for
// JSON, then toString). ok is false where that string form throws — see
// ToString.
func ToNumber(v any) (n float64, ok bool) {
	switch x := v.(type) {
	case nil:
		return 0, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case json.Number:
		return Number(x), true
	case string:
		return StringToNumber(x), true
	}
	s, ok := ToString(v)
	if !ok {
		return math.NaN(), false
	}
	return StringToNumber(s), true
}
