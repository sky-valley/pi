package schema

import (
	"cmp"
	"math"
	"math/big"
	"strings"
	"unicode/utf8"

	"github.com/sky-valley/pi/internal/jstext"
)

// check is a compiled boolean check (types.ts Check). The factories here
// (checks.ts) each return a closure that captures only its arguments, never
// the compiler.
//
// Upstream also inlines its leaf checks into object and array loops by a
// numeric code (LEAF_CODES, keyed by function identity); Go funcs are not
// comparable and the inlining is a V8 speed trick with no observable effect,
// so the port calls the leaf checks.
type check func(v any) bool

func always(any) bool { return true }
func never(any) bool  { return false }

func isString(v any) bool {
	_, ok := v.(string)
	return ok
}

func isBoolean(v any) bool {
	_, ok := v.(bool)
	return ok
}

func isNull(v any) bool { return v == nil }

func isBytes(v any) bool {
	_, ok := v.([]byte)
	return ok
}

func isBigInt(v any) bool {
	_, ok := bigintOf(v)
	return ok
}

func isUndefined(v any) bool { return v == undefined }

// typeChecks is checks.ts TYPE_CHECKS: the check of a `type` name. `bytes` (a
// []byte, a Uint8Array upstream), `bigint` (a *big.Int, TypeBox's
// Type.BigInt) and `undefined` (Undefined, TypeBox's Type.Undefined) extend
// JSON Schema for values that never pass through JSON. A `number` is finite,
// as in TypeBox: JSON has no NaN or Infinity, so only a value built in code
// can be one.
var typeChecks = map[string]check{
	"string":    isString,
	"number":    isFiniteNumber,
	"integer":   isInteger,
	"boolean":   isBoolean,
	"null":      isNull,
	"array":     isArray,
	"object":    isObject,
	"bytes":     isBytes,
	"bigint":    isBigInt,
	"undefined": isUndefined,
}

// guard is whether a combined object or array check also checks the type: not
// at all, strictly, or passing other types.
type guard int

const (
	guardNone guard = iota
	guardStrict
	guardLoose
)

// and is all of parts.
func and(parts []check) check {
	switch len(parts) {
	case 0:
		return always
	case 1:
		return parts[0]
	case 2:
		a, b := parts[0], parts[1]
		return func(v any) bool { return a(v) && b(v) }
	}
	return func(v any) bool {
		for _, p := range parts {
			if !p(v) {
				return false
			}
		}
		return true
	}
}

// scopedCheck is `c` with `uri` pushed onto the dynamic scope while it runs
// (only for schemas with a bookended `$dynamicRef`).
func scopedCheck(scope *[]string, uri string, c check) check {
	return func(v any) bool {
		*scope = append(*scope, uri)
		ok := c(v)
		*scope = (*scope)[:len(*scope)-1]
		return ok
	}
}

// typeCheck is any of the named types.
func typeCheck(types []any) check {
	checks := make([]check, len(types))
	for i, t := range types {
		checks[i] = never
		if name, ok := t.(string); ok {
			if c, ok := typeChecks[name]; ok {
				checks[i] = c
			}
		}
	}
	if len(checks) == 1 {
		return checks[0]
	}
	return func(v any) bool {
		for _, c := range checks {
			if c(v) {
				return true
			}
		}
		return false
	}
}

// bytesCheck is a []byte within the length bounds.
func bytesCheck(lo, hi float64) check {
	return func(v any) bool {
		b, ok := v.([]byte)
		return ok && float64(len(b)) >= lo && float64(len(b)) <= hi
	}
}

// codePoints is the number of Unicode code points of s: JSON Schema string
// lengths count code points, not UTF-16 units. Half a surrogate pair (a WTF-8
// triple) is a code point of its own; a high half followed by a low half is
// one.
func codePoints(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if unit, ok := jstext.WTF8Surrogate(s[i:]); ok {
			i += 3
			if lo, ok := jstext.WTF8Surrogate(s[i:]); ok && unit <= 0xDBFF && lo >= 0xDC00 {
				i += 3
			}
			n++
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		n++
	}
	return n
}

// lengthOk is checks.ts lengthOk. UTF-16 units bound code points from both
// sides (units / 2 <= points <= units), so most strings need no count.
func lengthOk(s string, lo, hi float64) bool {
	units := float64(jstext.CodeUnitLength(s))
	if units < lo {
		return false
	}
	if units < 2*lo && float64(codePoints(s)) < lo {
		return false
	}
	return units <= hi || float64(codePoints(s)) <= hi
}

// stringCheck is the string keywords: length bounds, pattern and format.
func stringCheck(strict bool, lo, hi float64, re *pattern, format FormatCheck) check {
	return func(v any) bool {
		s, ok := v.(string)
		if !ok {
			return !strict
		}
		return lengthOk(s, lo, hi) && (re == nil || re.test(s)) && (format == nil || format(s))
	}
}

// numberType is what the number keywords' check asks of the type: nothing
// (other types pass), a number, an integer, or a bigint.
type numberType int

const (
	othersPass numberType = iota
	numberOnly
	integerOnly
	bigintOnly
)

// bound is a number keyword's value, or a value the number keywords check
// (checks.ts Bound): a number, or a bigint when n is set (TypeBox's
// Type.BigInt writes its bounds as bigints).
type bound struct {
	f float64
	n *big.Int
}

// boundOf is v as a bound: a number or a bigint.
func boundOf(v any) (bound, bool) {
	if n, ok := bigintOf(v); ok {
		return bound{n: n}, true
	}
	f, ok := numberOf(v)
	return bound{f: f}, ok
}

// compareBounds compares a and b exactly, as JavaScript's relational
// operators compare numbers and bigints of either kind; ok is false where a
// NaN makes every comparison false.
func compareBounds(a, b bound) (c int, ok bool) {
	switch {
	case a.n == nil && b.n == nil:
		if math.IsNaN(a.f) || math.IsNaN(b.f) {
			return 0, false
		}
		return cmp.Compare(a.f, b.f), true
	case a.n != nil && b.n != nil:
		return a.n.Cmp(b.n), true
	case a.n != nil:
		c, ok := compareBounds(b, a)
		return -c, ok
	}
	switch {
	case math.IsNaN(a.f):
		return 0, false
	case math.IsInf(a.f, 1):
		return 1, true
	case math.IsInf(a.f, -1):
		return -1, true
	}
	return new(big.Float).SetFloat64(a.f).Cmp(new(big.Float).SetInt(b.n)), true
}

// inBounds is `v >= lo && v <= hi && v > xlo && v < xhi`.
func inBounds(v, lo, hi, xlo, xhi bound) bool {
	c, ok := compareBounds(v, lo)
	if !ok || c < 0 {
		return false
	}
	if c, ok = compareBounds(v, hi); !ok || c > 0 {
		return false
	}
	if c, ok = compareBounds(v, xlo); !ok || c <= 0 {
		return false
	}
	c, ok = compareBounds(v, xhi)
	return ok && c < 0
}

// numberCheck is the number keywords, for numbers and bigints alike (as
// TypeBox applies them): bounds of either kind compare with values of either
// kind.
func numberCheck(typ numberType, lo, hi, xlo, xhi bound, multipleOf *bound) check {
	return func(v any) bool {
		x, ok := boundOf(v)
		if !ok {
			return typ == othersPass
		}
		switch typ {
		case bigintOnly:
			if x.n == nil {
				return false
			}
		case numberOnly, integerOnly:
			if x.n != nil || !isFinite(x.f) || (typ == integerOnly && !isIntegral(x.f)) {
				return false
			}
		}
		return inBounds(x, lo, hi, xlo, xhi) && (multipleOf == nil || isMultipleBound(x, *multipleOf))
	}
}

// isMultipleBound is checks.ts isMultiple of a number or a bigint: with a
// bigint on either side, exact integer arithmetic, and a bigint is no
// multiple of 0 or of a fraction. Deliberately unlike TypeBox, which throws
// there (`BigInt(0.5)`, division by zero), as upstream's is: a check answers
// for every value and never throws.
func isMultipleBound(v, d bound) bool {
	if v.n == nil && d.n == nil {
		return isMultiple(v.f, d.f)
	}
	vi, ok := integerOf(v)
	if !ok {
		return false
	}
	di, ok := integerOf(d)
	return ok && di.Sign() != 0 && new(big.Int).Rem(vi, di).Sign() == 0
}

// integerOf is BigInt(b) of an integral bound; ok is false for a number that
// is not an integer.
func integerOf(b bound) (*big.Int, bool) {
	if b.n != nil {
		return b.n, true
	}
	if !isIntegral(b.f) {
		return nil, false
	}
	n, _ := new(big.Float).SetFloat64(b.f).Int(nil)
	return n, true
}

// isMultiple is checks.ts isMultiple of two numbers: whether v is a multiple
// of d, exactly for decimals (0.0075 / 0.0001 is 74.99999999999999 in
// floating point).
func isMultiple(v, d float64) bool {
	q := v / d
	if isIntegral(q) {
		return true
	}
	if !isFinite(q) {
		return math.Mod(v, d) == 0
	}
	vc, ve := decimal(v)
	dc, de := decimal(d)
	if ve >= de {
		x := new(big.Int).Mul(vc, pow10(ve-de))
		return x.Rem(x, dc).Sign() == 0
	}
	m := new(big.Int).Mul(dc, pow10(de-ve))
	return new(big.Int).Rem(vc, m).Sign() == 0
}

func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// decimal is |x| as an integer coefficient and a power of ten, read from
// JavaScript's shortest decimal form of it.
func decimal(x float64) (*big.Int, int) {
	mantissa, exponent, _ := strings.Cut(jstext.NumberToString(math.Abs(x)), "e")
	exp := 0
	if exponent != "" {
		exp = int(jstext.StringToNumber(exponent))
	}
	fraction := 0
	if point := strings.IndexByte(mantissa, '.'); point >= 0 {
		fraction = len(mantissa) - point - 1
	}
	c, _ := new(big.Int).SetString(strings.Replace(mantissa, ".", "", 1), 10)
	if c == nil {
		c = new(big.Int)
	}
	return c, exp - fraction
}

// guardObject is inner for objects; other values pass unless strict.
func guardObject(inner check, strict bool) check {
	if strict {
		return func(v any) bool { return isObject(v) && inner(v) }
	}
	return func(v any) bool { return !isObject(v) || inner(v) }
}

// guardArray is inner for arrays; other values pass unless strict.
func guardArray(inner check, strict bool) check {
	if strict {
		return func(v any) bool { return isArray(v) && inner(v) }
	}
	return func(v any) bool { return !isArray(v) || inner(v) }
}

// declared is one declared property of an object check.
type declared struct {
	key      string
	check    check
	required bool
}

// declaredObject is an object with declared properties only: each own key
// is found among them, `closed` rejects any other key, and the required ones
// must all be there. An optional property set to Undefined counts as absent.
func declaredObject(entries []declared, requiredCount int, closed bool, g guard) check {
	at := make(map[string]int, len(entries))
	for i, e := range entries {
		if _, dup := at[e.key]; !dup {
			at[e.key] = i
		}
	}
	return func(v any) bool {
		if g != guardNone && !isObject(v) {
			return g == guardLoose
		}
		required := 0
		ok := true
		eachMember(v, func(k string, x any) bool {
			i, found := at[k]
			if !found {
				ok = !closed
				return ok
			}
			// An optional property set to undefined counts as absent; a
			// required one is present, and its schema decides whether
			// undefined is a value of it (TypeBox's Type.Undefined,
			// Unknown, Any).
			if x == undefined && !entries[i].required {
				return true
			}
			if !entries[i].check(x) {
				ok = false
				return false
			}
			if entries[i].required {
				required++
			}
			return true
		})
		return ok && required == requiredCount
	}
}

// patternCheck is one patternProperties entry.
type patternCheck struct {
	re    *pattern
	check check
}

// openObject is an object with declared properties, required keys not
// declared, pattern properties and additional properties.
func openObject(entries []declared, keys, extraRequired []string, patterns []patternCheck, additional check, g guard) check {
	var known map[string]bool
	if len(patterns) > 0 || additional != nil {
		known = make(map[string]bool, len(keys))
		for _, k := range keys {
			known[k] = true
		}
	}
	return func(v any) bool {
		if g != guardNone && !isObject(v) {
			return g == guardLoose
		}
		for _, e := range entries {
			x, ok := getOwn(v, e.key)
			// Absent, or an optional property set to undefined (which
			// counts as absent); see declaredObject.
			if !ok || (x == undefined && !e.required) {
				if e.required {
					return false
				}
				continue
			}
			if !e.check(x) {
				return false
			}
		}
		for _, k := range extraRequired {
			if !hasOwn(v, k) {
				return false
			}
		}
		if known == nil {
			return true
		}
		ok := true
		eachMember(v, func(k string, x any) bool {
			matched := known[k]
			for _, p := range patterns {
				if p.re.test(k) {
					matched = true
					if !p.check(x) {
						ok = false
						return false
					}
				}
			}
			if !matched && additional != nil && !additional(x) {
				ok = false
			}
			return ok
		})
		return ok
	}
}

// propertyNamesCheck is every property name passing c (v is an object).
func propertyNamesCheck(c check) check {
	return func(v any) bool {
		ok := true
		eachMember(v, func(k string, _ any) bool {
			ok = c(k)
			return ok
		})
		return ok
	}
}

// propertyCountCheck is minProperties and maxProperties (v is an object).
func propertyCountCheck(lo, hi float64) check {
	return func(v any) bool {
		n := float64(objectLen(v))
		return n >= lo && n <= hi
	}
}

// dependency is one dependentRequired entry.
type dependency struct {
	key  string
	list []any
}

// dependentRequiredCheck is dependentRequired (v is an object). A listed name
// that is not a string is the property key it converts to, as upstream's
// Object.hasOwn reads it.
func dependentRequiredCheck(entries []dependency) check {
	return func(v any) bool {
		for _, e := range entries {
			if !hasOwn(v, e.key) {
				continue
			}
			for _, r := range e.list {
				if !hasOwn(v, jsString(r)) {
					return false
				}
			}
		}
		return true
	}
}

// dependentSchema is one dependentSchemas entry.
type dependentSchema struct {
	key   string
	check check
}

// dependentSchemasCheck is dependentSchemas (v is an object).
func dependentSchemasCheck(entries []dependentSchema) check {
	return func(v any) bool {
		for _, e := range entries {
			if hasOwn(v, e.key) && !e.check(v) {
				return false
			}
		}
		return true
	}
}

// listCheck is the common array: guard, length bounds, prefix items and the
// items after them, in one closure.
func listCheck(g guard, lo, hi float64, prefix []check, rest check) check {
	return func(v any) bool {
		list, ok := v.([]any)
		if !ok {
			return g == guardLoose
		}
		n := float64(len(list))
		if n < lo || n > hi {
			return false
		}
		for i := 0; i < len(list) && i < len(prefix); i++ {
			if !prefix[i](list[i]) {
				return false
			}
		}
		if rest == nil {
			return true
		}
		for i := len(prefix); i < len(list); i++ {
			if !rest(list[i]) {
				return false
			}
		}
		return true
	}
}

// itemsCheck is prefix items and the items after them (v is an array).
func itemsCheck(prefix []check, rest check) check {
	return listCheck(guardNone, 0, math.Inf(1), prefix, rest)
}

// lengthCheck is minItems and maxItems (v is an array).
func lengthCheck(lo, hi float64) check {
	return func(v any) bool {
		n := float64(len(v.([]any)))
		return n >= lo && n <= hi
	}
}

// containsCheck is contains with its bounds (v is an array).
func containsCheck(c check, lo, hi float64) check {
	return func(v any) bool {
		n := 0.0
		for _, x := range v.([]any) {
			if c(x) {
				if n++; n > hi {
					return false
				}
			}
		}
		return n >= lo
	}
}

// uniqueCheck is uniqueItems (v is an array).
func uniqueCheck(v any) bool {
	list := v.([]any)
	for i := 1; i < len(list); i++ {
		for j := 0; j < i; j++ {
			if deepEqual(list[i], list[j]) {
				return false
			}
		}
	}
	return true
}

// enumCheck is enum: up to 8 primitives by a scan (===), more by a set
// (SameValueZero, so NaN is a member of a set that has it), objects and
// arrays by deep equality.
func enumCheck(values []any) check {
	var primitives, objects []any
	for _, x := range values {
		if isPrimitive(x) {
			primitives = append(primitives, x)
		} else {
			objects = append(objects, x)
		}
	}
	if len(objects) == 0 && len(primitives) <= 8 {
		return func(v any) bool {
			for _, p := range primitives {
				if strictEquals(p, v) {
					return true
				}
			}
			return false
		}
	}
	set := make(map[any]bool, len(primitives))
	for _, p := range primitives {
		if k, ok := zeroKey(p); ok {
			set[k] = true
		}
	}
	return func(v any) bool {
		if isPrimitive(v) {
			k, ok := zeroKey(v)
			return ok && set[k]
		}
		for _, o := range objects {
			if deepEqual(v, o) {
				return true
			}
		}
		return false
	}
}

// constCheck is const.
func constCheck(value any) check {
	if isPrimitive(value) {
		return func(v any) bool { return strictEquals(v, value) }
	}
	return func(v any) bool { return deepEqual(v, value) }
}

// anyCheck is anyOf.
func anyCheck(checks []check) check {
	return func(v any) bool {
		for _, c := range checks {
			if c(v) {
				return true
			}
		}
		return false
	}
}

// oneCheck is oneOf.
func oneCheck(checks []check) check {
	return func(v any) bool {
		n := 0
		for _, c := range checks {
			if c(v) {
				if n++; n > 1 {
					return false
				}
			}
		}
		return n == 1
	}
}

// notCheck is not.
func notCheck(c check) check {
	return func(v any) bool { return !c(v) }
}

// ifCheck is if, then and else.
func ifCheck(cond, then, otherwise check) check {
	return func(v any) bool {
		if cond(v) {
			return then(v)
		}
		return otherwise(v)
	}
}

// taggedCheck is a union of objects tagged by a const on key: the tag selects
// the one branch that can match.
func taggedCheck(key string, tags []any, checks []check) check {
	return func(v any) bool {
		if !isObject(v) {
			return false
		}
		tag, ok := getOwn(v, key)
		if !ok {
			return false
		}
		for i, t := range tags {
			if strictEquals(t, tag) {
				return checks[i](v)
			}
		}
		return false
	}
}
