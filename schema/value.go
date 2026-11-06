package schema

import (
	"cmp"
	"encoding/json"
	"math"
	"math/big"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/sky-valley/pi/internal/jstext"
)

// The value model. Schemas and the values they check are JSON values as Go
// holds them: nil, bool, a number (float64, or a json.Number or any Go
// integer or float type, each read as the JavaScript number it denotes),
// string (UTF-8, half a surrogate pair as its WTF-8 triple, as
// internal/jstext holds one), []any, []byte (pi-schema's "bytes", a
// Uint8Array upstream) and an object: an Object, whose members keep the
// own-key order a JavaScript object lists, or a map[string]any. Two
// JavaScript values that JSON never holds extend it, for values built in
// code and for TypeBox's Type.BigInt and Type.Undefined: a *big.Int is a
// bigint (`{"type":"bigint"}`), and Undefined is undefined
// (`{"type":"undefined"}`).
//
// A Go map has no insertion order, so a map[string]any lists its keys the
// way a JavaScript object would if its keys had been added in sorted order:
// array-index keys ascending, then the others by UTF-16 code unit. Errors are
// reported in key order, so a caller that needs upstream's order exactly
// passes an Object (Decode builds one from JSON text).

// Member is one key and value of an Object.
type Member struct {
	Key   string
	Value any
}

// Object is a JSON object whose members are listed in JavaScript own-key
// order: array-index keys ascending, then the others in insertion order (the
// order JSON.parse, Object.keys and JSON.stringify follow). Its keys are
// distinct, a precondition the package does not check (Decode, like
// JSON.parse, never repeats one): with a repeated key, reads see the first
// member while member counts (minProperties, maxProperties) count each, so
// such an object validates as no JavaScript object could. The package never
// reorders an Object: it lists its members in the order the caller gave, and
// a coerced copy keeps that order.
type Object []Member

// Get returns the value of key and whether the object has it.
func (o Object) Get(key string) (any, bool) {
	for _, m := range o {
		if m.Key == key {
			return m.Value, true
		}
	}
	return nil, false
}

// MarshalJSON writes the object as JSON.stringify does: members in order,
// numbers as JavaScript writes them, a non-finite number as null, a member
// set to Undefined left out. A bigint fails it, as JSON.stringify throws.
func (o Object) MarshalJSON() (out []byte, err error) {
	defer func() {
		if p := recover(); p != nil {
			ce, ok := p.(*compileError)
			if !ok {
				panic(p)
			}
			err = ce.err
		}
	}()
	return []byte(stringify(o)), nil
}

// undefinedValue is the type of Undefined.
type undefinedValue struct{}

// Undefined is JavaScript's undefined. A property set to it is present, as
// `{ a: undefined }` has the key a: an optional one counts as absent, and a
// required one is valid where its schema takes undefined (`{}`, or a union
// with `{"type":"undefined"}`). Decode never produces it; Coerce and Parse
// return it where a schema converts to it (`{"type":"undefined"}` takes
// null, "", 0, false and the like), and JSON (Object.MarshalJSON) leaves
// out a property set to it.
//
// encoding/json cannot leave a member out, so a map[string]any that Coerce
// or Parse returns keeps the key of a member set to Undefined, and
// json.Marshal writes it as null (where JSON.stringify drops it). Callers
// that marshal a result should marshal it as an Object or drop the
// Undefined members first.
var Undefined = undefinedValue{}

// MarshalJSON writes Undefined as null, as JSON.stringify writes an
// undefined array item (`[undefined]` is `[null]`).
func (undefinedValue) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

// undefined is Undefined as the package compares it: also what a read of a
// property an object does not have returns (propOf), as in JavaScript.
var undefined any = Undefined

// bigintOf returns v as the bigint it is, and whether it is one (typeof v
// === "bigint").
func bigintOf(v any) (*big.Int, bool) {
	n, ok := v.(*big.Int)
	return n, ok && n != nil
}

// Equal is JSON equality, as const, enum and uniqueItems compare (checks.ts
// deepEqual, exported as equal): the same primitives, arrays with equal
// items in order, objects with the same keys and equal values, whatever
// their order or kind (an Object equals a map[string]any with its members).
// On JSON values it answers as TypeBox's Value.Equal does.
func Equal(a, b any) bool { return deepEqual(a, b) }

// numberOf returns v as the JavaScript number it denotes, and whether v is a
// number at all (typeof v === "number").
func numberOf(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case json.Number:
		return jstext.Number(x), true
	case float32:
		return float64(x), true
	case int64:
		return float64(x), true
	case int32:
		return float64(x), true
	case int16:
		return float64(x), true
	case int8:
		return float64(x), true
	case uint:
		return float64(x), true
	case uint64:
		return float64(x), true
	case uint32:
		return float64(x), true
	case uint16:
		return float64(x), true
	case uint8:
		return float64(x), true
	}
	return 0, false
}

func isNumber(v any) bool {
	_, ok := numberOf(v)
	return ok
}

// isFiniteNumber is Number.isFinite: a number, not NaN or ±Infinity (the
// `number` type, since 55a5c6914).
func isFiniteNumber(v any) bool {
	f, ok := numberOf(v)
	return ok && isFinite(f)
}

// isInteger is Number.isInteger.
func isInteger(v any) bool {
	f, ok := numberOf(v)
	return ok && isIntegral(f)
}

func isIntegral(f float64) bool {
	return !math.IsInf(f, 0) && !math.IsNaN(f) && f == math.Trunc(f)
}

func isFinite(f float64) bool { return !math.IsInf(f, 0) && !math.IsNaN(f) }

// isObject is checks.ts isObject: a JSON object, not null, an array or bytes.
func isObject(v any) bool {
	switch v.(type) {
	case Object, map[string]any, *schemaObject:
		return true
	}
	return false
}

func isArray(v any) bool {
	_, ok := v.([]any)
	return ok
}

// ownKeys is Object.keys of an object (see the value model for a map's
// order).
func ownKeys(v any) []string {
	switch o := v.(type) {
	case Object:
		keys := make([]string, len(o))
		for i, m := range o {
			keys[i] = m.Key
		}
		return keys
	case map[string]any:
		keys := make([]string, 0, len(o))
		for k := range o {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, compareKeys)
		return keys
	case *schemaObject:
		keys := make([]string, len(o.members))
		for i, m := range o.members {
			keys[i] = m.Key
		}
		return keys
	}
	return nil
}

// compareKeys orders keys as a JavaScript object lists keys added in sorted
// order: array indices ascending, then the rest by UTF-16 code unit.
func compareKeys(a, b string) int {
	ai, aIndex := jstext.ArrayIndexKey(a)
	bi, bIndex := jstext.ArrayIndexKey(b)
	switch {
	case aIndex && bIndex:
		return cmp.Compare(ai, bi)
	case aIndex:
		return -1
	case bIndex:
		return 1
	}
	return jstext.CompareUTF16(a, b)
}

// getOwn is an own-property read: the value and whether the object has it.
func getOwn(v any, key string) (any, bool) {
	switch o := v.(type) {
	case Object:
		return o.Get(key)
	case map[string]any:
		x, ok := o[key]
		return x, ok
	case *schemaObject:
		return o.get(key)
	}
	return nil, false
}

// propOf is a property read, `v[key]`: undefined when v does not have it.
func propOf(v any, key string) any {
	if x, ok := getOwn(v, key); ok {
		return x
	}
	return undefined
}

func hasOwn(v any, key string) bool {
	_, ok := getOwn(v, key)
	return ok
}

func objectLen(v any) int {
	switch o := v.(type) {
	case Object:
		return len(o)
	case map[string]any:
		return len(o)
	case *schemaObject:
		return len(o.members)
	}
	return 0
}

// eachMember calls f with each key and value of an object, in no particular
// order for a map, until f returns false. It is the walk for checks, whose
// result does not depend on the order.
func eachMember(v any, f func(key string, value any) bool) {
	switch o := v.(type) {
	case Object:
		for _, m := range o {
			if !f(m.Key, m.Value) {
				return
			}
		}
	case map[string]any:
		for k, x := range o {
			if !f(k, x) {
				return
			}
		}
	case *schemaObject:
		for _, m := range o.members {
			if !f(m.Key, m.Value) {
				return
			}
		}
	}
}

// strictEquals is JavaScript's ===: numbers by value (NaN equals nothing, -0
// equals 0), strings and booleans by value, objects and arrays by identity.
func strictEquals(a, b any) bool {
	if x, ok := numberOf(a); ok {
		y, ok := numberOf(b)
		return ok && x == y
	}
	if x, ok := bigintOf(a); ok {
		y, ok := bigintOf(b)
		return ok && x.Cmp(y) == 0
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	case undefinedValue:
		_, ok := b.(undefinedValue)
		return ok
	}
	ka, oka := identityOf(a)
	kb, okb := identityOf(b)
	return oka && okb && ka == kb
}

// sameValue is Object.is: strictEquals, except that NaN is NaN and -0 is not 0.
func sameValue(a, b any) bool {
	if x, ok := numberOf(a); ok {
		y, ok := numberOf(b)
		if !ok {
			return false
		}
		if math.IsNaN(x) {
			return math.IsNaN(y)
		}
		return x == y && math.Signbit(x) == math.Signbit(y)
	}
	return strictEquals(a, b)
}

// identity is the reference an object, array or byte slice stands for, as a
// map key: the backing storage and the length.
type identity struct {
	kind byte
	ptr  uintptr
	n    int
}

// identityOf returns v's reference identity. An empty Object, array or byte
// slice has no storage of its own; all of a kind count as one, which no
// coercion can tell apart (nothing in an empty one changes).
func identityOf(v any) (identity, bool) {
	switch x := v.(type) {
	case Object:
		return identity{'o', reflect.ValueOf(x).Pointer(), len(x)}, true
	case []any:
		return identity{'a', reflect.ValueOf(x).Pointer(), len(x)}, true
	case []byte:
		return identity{'b', reflect.ValueOf(x).Pointer(), len(x)}, true
	case map[string]any:
		return identity{'m', reflect.ValueOf(x).Pointer(), 0}, true
	case *schemaObject:
		return identity{'s', reflect.ValueOf(x).Pointer(), 0}, true
	}
	return identity{}, false
}

// nanKey stands for NaN in a SameValueZero-keyed map, where NaN is one key.
type nanKey struct{}

// bigintKey is a bigint in a SameValueZero-keyed map: its digits.
type bigintKey string

// zeroKey is v as a key with SameValueZero equality (Map and Set keys): a
// number as its float64 (-0 as 0, NaN as one key), an object or array by
// identity, any other primitive as itself. ok is false for a value with
// neither (an empty array has no identity of its own).
func zeroKey(v any) (any, bool) {
	if f, ok := numberOf(v); ok {
		if math.IsNaN(f) {
			return nanKey{}, true
		}
		if f == 0 {
			f = 0
		}
		return f, true
	}
	if n, ok := bigintOf(v); ok {
		return bigintKey(n.String()), true
	}
	switch v.(type) {
	case nil, bool, string, undefinedValue:
		return v, true
	}
	if id, ok := identityOf(v); ok && (id.n > 0 || id.kind == 'm' || id.kind == 's') {
		return id, true
	}
	return nil, false
}

// isPrimitive is `typeof v !== "object" || v === null`.
func isPrimitive(v any) bool {
	switch v.(type) {
	case Object, map[string]any, *schemaObject, []any, []byte:
		return false
	}
	return true
}

// deepEqual is checks.ts deepEqual: JSON equality, objects by their own keys
// whatever their order. Bytes compare as the objects of index keys a
// Uint8Array is.
func deepEqual(a, b any) bool {
	if strictEquals(a, b) {
		return true
	}
	if isPrimitive(a) || isPrimitive(b) {
		return false
	}
	if x, ok := a.([]byte); ok {
		a = bytesObject(x)
	}
	if y, ok := b.([]byte); ok {
		b = bytesObject(y)
	}
	x, xArray := a.([]any)
	y, yArray := b.([]any)
	if xArray != yArray {
		return false
	}
	if xArray {
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if !deepEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	}
	if objectLen(a) != objectLen(b) {
		return false
	}
	equal := true
	eachMember(a, func(k string, xv any) bool {
		yv, ok := getOwn(b, k)
		equal = ok && deepEqual(xv, yv)
		return equal
	})
	return equal
}

func bytesObject(b []byte) Object {
	o := make(Object, len(b))
	for i, x := range b {
		o[i] = Member{strconv.Itoa(i), float64(x)}
	}
	return o
}

// jsString is String(v) for the values join and property keys meet: null as
// "null" (join writes null and undefined as "", see joinValues).
func jsString(v any) string {
	if f, ok := numberOf(v); ok {
		return jstext.NumberToString(f)
	}
	if n, ok := bigintOf(v); ok {
		return n.String()
	}
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(x)
	case string:
		return x
	case undefinedValue:
		return "undefined"
	case []any:
		return joinValues(x, ",")
	case []byte:
		parts := make([]string, len(x))
		for i, c := range x {
			parts[i] = strconv.Itoa(int(c))
		}
		return strings.Join(parts, ",")
	}
	return "[object Object]"
}

// joinValues is Array.prototype.join: null and undefined as "".
func joinValues(values []any, sep string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		switch v.(type) {
		case nil, undefinedValue:
		default:
			parts[i] = jsString(v)
		}
	}
	return strings.Join(parts, sep)
}
