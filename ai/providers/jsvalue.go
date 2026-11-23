package providers

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf16"

	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/internal/jstext"
)

// This file reads a parsed JSON value — ai.DecodeOrderedValue's shapes:
// ai.OrderedObject, []any, float64, string, bool, nil — with JavaScript's
// semantics, for adapter code that pi writes against untyped SDK objects and
// so behaves on a mistyped field the way V8 does rather than failing a Go
// type check.

// jsUndefinedValue is JavaScript's undefined: what reading a property that is
// not there yields, distinct from a JSON null (nil).
type jsUndefinedValue struct{}

var jsUndefined any = jsUndefinedValue{}

// errNoPrimitive is V8's TypeError for converting an object whose own
// "toString" member (never callable in JSON) leaves it no primitive value.
var errNoPrimitive = errors.New("Cannot convert object to primitive value")

// jsGet is the property read `v.key` (or `v?.key`) of a JSON value: an
// object's member, else undefined — a JSON array, string, number or boolean
// has none of the names adapters read.
func jsGet(v any, key string) any {
	if o, ok := v.(ai.OrderedObject); ok {
		for _, f := range o {
			if f.Key == key {
				return f.Value
			}
		}
	}
	return jsUndefined
}

// jsIndex0 is `v?.[0]`: an array's first element, a string's first UTF-16
// code unit, an object's "0" member, else undefined.
func jsIndex0(v any) any {
	switch t := v.(type) {
	case []any:
		if len(t) > 0 {
			return t[0]
		}
	case string:
		if units := utf16.Encode([]rune(t)); len(units) > 0 {
			return string(utf16.Decode(units[:1]))
		}
	case ai.OrderedObject:
		return jsGet(t, "0")
	}
	return jsUndefined
}

// jsLengthTruthy is Boolean(v?.length): an array or a string when non-empty,
// an object by its own "length" member's truthiness, anything else (whose
// length is undefined) false.
func jsLengthTruthy(v any) bool {
	switch t := v.(type) {
	case []any:
		return len(t) > 0
	case string:
		return t != ""
	case ai.OrderedObject:
		return jsTruthy(jsGet(t, "length"))
	}
	return false
}

// jsTruthy is Boolean(v) for a JSON-decoded value (objects and arrays are
// always truthy) or undefined.
func jsTruthy(v any) bool {
	switch t := v.(type) {
	case nil, jsUndefinedValue:
		return false
	case bool:
		return t
	case float64:
		return t != 0 && !math.IsNaN(t)
	case string:
		return t != ""
	}
	return true
}

// jsToString is String(v) — and a template literal's or a string
// concatenation's reading of v. An array joins its elements' strings with
// ",", null and undefined elements as ""; an object is "[object Object]",
// unless an own "toString" member leaves it no primitive value.
func jsToString(v any) (string, error) {
	switch t := v.(type) {
	case jsUndefinedValue:
		return "undefined", nil
	case nil:
		return "null", nil
	case bool:
		if t {
			return "true", nil
		}
		return "false", nil
	case float64:
		return jstext.NumberToString(t), nil
	case string:
		return t, nil
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			if e == nil || e == jsUndefined {
				continue
			}
			s, err := jsToString(e)
			if err != nil {
				return "", err
			}
			parts[i] = s
		}
		return strings.Join(parts, ","), nil
	case ai.OrderedObject:
		if jsGet(t, "toString") != jsUndefined {
			return "", errNoPrimitive
		}
		return "[object Object]", nil
	}
	return "", fmt.Errorf("jsToString: %T is not a parsed JSON value", v)
}

// jsToNumber is Number(v): an array or object by the number its string
// spells.
func jsToNumber(v any) (float64, error) {
	switch t := v.(type) {
	case jsUndefinedValue:
		return math.NaN(), nil
	case nil:
		return 0, nil
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case float64:
		return t, nil
	case string:
		return jstext.StringToNumber(t), nil
	}
	s, err := jsToString(v)
	if err != nil {
		return 0, err
	}
	return jstext.StringToNumber(s), nil
}

// jsAdd is `a + b`: string concatenation when either side's primitive is a
// string (an array's or object's is), else numeric addition.
func jsAdd(a, b any) (any, error) {
	prim := func(v any) (any, error) {
		switch v.(type) {
		case []any, ai.OrderedObject:
			return jsToString(v)
		}
		return v, nil
	}
	pa, err := prim(a)
	if err != nil {
		return nil, err
	}
	pb, err := prim(b)
	if err != nil {
		return nil, err
	}
	_, sa := pa.(string)
	_, sb := pb.(string)
	if sa || sb {
		x, _ := jsToString(pa)
		y, _ := jsToString(pb)
		return x + y, nil
	}
	x, _ := jsToNumber(pa)
	y, _ := jsToNumber(pb)
	return x + y, nil
}

// jsStrictEqual is `a === b` for parsed JSON values: primitives by value, an
// object or array only by identity, which two parsed values never share.
func jsStrictEqual(a, b any) bool {
	switch a.(type) {
	case nil, jsUndefinedValue, bool, float64, string:
		return a == b
	}
	return false
}
