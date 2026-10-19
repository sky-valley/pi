// Package jsonstrict decodes an untyped value tree into typed Go values,
// rejecting anything the target type does not declare.
//
// The input is what a JSON or CBOR decoder produces when asked for a plain
// tree: nil, bool, int64 or float64, string, []byte, []any and map[string]any.
// The output is whatever struct the caller declares, filled field by field with
// the same rules TypeBox's additionalProperties:false imposes upstream: an
// unknown property is a rejection, not something to ignore. A peer that can
// smuggle extra fields past the parser can reach code paths the schema was
// meant to gate, so this is a security boundary rather than a tidiness rule.
//
// chord owns the one decoder built on this package, for its strict-JSON
// service boundary.
package jsonstrict

import (
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
)

// maxSafeInteger is JavaScript's Number.MAX_SAFE_INTEGER. pi has one number
// type, so an integer field is satisfied by anything Number.isInteger accepts
// within this range — including a float that happens to be integral.
const maxSafeInteger = 1<<53 - 1

// Error is a value that failed structural or constraint checking.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// Errorf builds an *Error the way fmt.Errorf builds an error.
func Errorf(format string, a ...any) *Error {
	return &Error{Msg: fmt.Sprintf(format, a...)}
}

// Validator is implemented by types carrying constraints the struct shape
// alone cannot express — TypeBox's minLength, minimum, and literals. A struct
// that implements it is validated as soon as it has been filled.
type Validator interface {
	Validate() error
}

// Decoder decodes value trees into the struct types one package declares.
//
// The zero value is ready to use; Tag and Root are read on every call, so set
// them before decoding. Union registration is not synchronized against
// decoding: register every union in init, before the first Decode.
type Decoder struct {
	// Tag is the struct tag key that names fields on the wire, such as "cbor"
	// or "json". A field tagged "-" is skipped; ",omitempty" marks it optional.
	// Untagged exported fields decode under their Go name.
	Tag string
	// Root is how errors name the top-level value ("message is missing
	// required property"). It defaults to "value".
	Root string

	// unions maps a union interface type to the function that resolves a
	// decoded value into one of its members. pi discriminates these unions
	// with a literal property; Go dispatches on the same property, registered
	// here so the reflective decoder can recurse through interface-typed
	// fields.
	unions map[reflect.Type]func(any) (any, error)

	// fields caches structFields per type. One Decoder serves every connection
	// of its package, so the cache must be safe for concurrent use.
	fields sync.Map
}

// RegisterUnion tells d how to resolve a value into the union interface T.
// It is a package-level generic function because Go methods cannot take type
// parameters.
func RegisterUnion[T any](d *Decoder, decode func(any) (T, error)) {
	if d.unions == nil {
		d.unions = map[reflect.Type]func(any) (any, error){}
	}
	d.unions[reflect.TypeFor[T]()] = func(v any) (any, error) { return decode(v) }
}

// Decode strictly decodes value into target, which must be a non-nil pointer.
//
// Validation is not repeated at the top level: decodeStruct validates every
// struct it fills, the target included, so a second Validator check here
// would run the same constraints twice.
func (d *Decoder) Decode(value any, target any) error {
	rv := reflect.ValueOf(target)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return Errorf("decode target must be a non-nil pointer")
	}
	return d.decodeValue(value, rv.Elem(), "")
}

// DecodeMember decodes value into a fresh T and returns it, for union arms.
func DecodeMember[T any](d *Decoder, value any) (T, error) {
	var out T
	if err := d.Decode(value, &out); err != nil {
		return out, err
	}
	return out, nil
}

// Discriminant reads a union's tag property, the first thing every union
// decoder needs, and returns it with the object it came from.
func Discriminant(value any, property string) (string, map[string]any, error) {
	entries, ok := value.(map[string]any)
	if !ok {
		return "", nil, Errorf("expected an object")
	}
	raw, present := entries[property]
	if !present {
		return "", nil, Errorf("missing %q", property)
	}
	tag, ok := raw.(string)
	if !ok {
		return "", nil, Errorf("%q must be a string", property)
	}
	return tag, entries, nil
}

func path(prefix, field string) string {
	if prefix == "" {
		return field
	}
	return prefix + "." + field
}

func (d *Decoder) decodeValue(value any, target reflect.Value, at string) error {
	// A registered union interface resolves by its discriminant before any
	// structural rules apply.
	if decode, ok := d.unions[target.Type()]; ok {
		resolved, err := decode(value)
		if err != nil {
			return prefixError(err, at)
		}
		target.Set(reflect.ValueOf(resolved))
		return nil
	}

	switch target.Kind() {
	case reflect.Pointer:
		elem := reflect.New(target.Type().Elem())
		if err := d.decodeValue(value, elem.Elem(), at); err != nil {
			return err
		}
		target.Set(elem)
		return nil

	case reflect.Interface:
		// An unconstrained interface is JsonValue: anything the tree decoder
		// produced is already a legal value.
		if target.NumMethod() == 0 {
			// reflect.ValueOf(nil) is the zero Value and cannot be Set, so a
			// null JsonValue leaves the field at its nil zero value. Missing
			// this is a remotely triggerable panic: any peer could crash the
			// process by sending null for a tool input.
			if value == nil {
				return nil
			}
			target.Set(reflect.ValueOf(value))
			return nil
		}
		return Errorf("no decoder registered for union %s at %s", target.Type(), at)

	case reflect.String:
		s, ok := value.(string)
		if !ok {
			return Errorf("%s must be a string", d.describe(at))
		}
		target.SetString(s)
		return nil

	case reflect.Bool:
		b, ok := value.(bool)
		if !ok {
			return Errorf("%s must be a boolean", d.describe(at))
		}
		target.SetBool(b)
		return nil

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		switch n := value.(type) {
		case int64:
			target.SetInt(n)
			return nil
		case float64:
			// TypeBox checks Number.isInteger, and pi has one number type, so
			// an integral float satisfies an integer field. pi's own encoder
			// folds these to CBOR ints, but a third-party peer may not — and
			// a JSON tree carries every number as float64.
			if n != math.Trunc(n) || math.IsInf(n, 0) || math.Abs(n) > maxSafeInteger {
				return Errorf("%s must be an integer", d.describe(at))
			}
			target.SetInt(int64(n))
			return nil
		default:
			return Errorf("%s must be an integer", d.describe(at))
		}

	case reflect.Float32, reflect.Float64:
		// pi has one number type, so an integer on the wire is a legal value
		// for a float field — and the CBOR encoder writes integral floats as
		// integers, so this is the same value coming back.
		switch n := value.(type) {
		case float64:
			target.SetFloat(n)
		case int64:
			target.SetFloat(float64(n))
		default:
			return Errorf("%s must be a number", d.describe(at))
		}
		return nil

	case reflect.Slice:
		if target.Type().Elem().Kind() == reflect.Uint8 {
			b, ok := value.([]byte)
			if !ok {
				return Errorf("%s must be a byte string", d.describe(at))
			}
			target.SetBytes(b)
			return nil
		}
		items, ok := value.([]any)
		if !ok {
			return Errorf("%s must be an array", d.describe(at))
		}
		out := reflect.MakeSlice(target.Type(), len(items), len(items))
		for i, item := range items {
			if err := d.decodeValue(item, out.Index(i), fmt.Sprintf("%s[%d]", at, i)); err != nil {
				return err
			}
		}
		target.Set(out)
		return nil

	case reflect.Map:
		// A decoded object always has string keys, so a map field with any
		// other key type has no decodable form; without this the SetMapIndex
		// below panics on the type mismatch.
		keyType := target.Type().Key()
		if keyType.Kind() != reflect.String {
			return Errorf("%s cannot decode into a map keyed by %s: objects have string keys, "+
				"so declare the field as a map with a string key type", d.describe(at), keyType)
		}
		entries, ok := value.(map[string]any)
		if !ok {
			return Errorf("%s must be an object", d.describe(at))
		}
		out := reflect.MakeMapWithSize(target.Type(), len(entries))
		for key, item := range entries {
			elem := reflect.New(target.Type().Elem()).Elem()
			if err := d.decodeValue(item, elem, path(at, key)); err != nil {
				return err
			}
			// Convert: a named string type is still a legal key type, but
			// SetMapIndex demands the map's exact key type.
			out.SetMapIndex(reflect.ValueOf(key).Convert(keyType), elem)
		}
		target.Set(out)
		return nil

	case reflect.Struct:
		return d.decodeStruct(value, target, at)

	default:
		return Errorf("cannot decode into %s at %s", target.Kind(), at)
	}
}

func (d *Decoder) decodeStruct(value any, target reflect.Value, at string) error {
	entries, ok := value.(map[string]any)
	if !ok {
		return Errorf("%s must be an object", d.describe(at))
	}

	fields := d.structFields(target.Type())
	seen := make(map[string]bool, len(entries))
	for _, f := range fields {
		raw, present := entries[f.name]
		if !present {
			if f.optional {
				continue
			}
			return Errorf("%s is missing required property %q", d.describe(at), f.name)
		}
		seen[f.name] = true
		if err := d.decodeValue(raw, target.FieldByIndex(f.index), path(at, f.name)); err != nil {
			return err
		}
	}
	for key := range entries {
		if !seen[key] {
			return Errorf("%s has unexpected property %q", d.describe(at), key)
		}
	}

	if v, ok := target.Addr().Interface().(Validator); ok {
		if err := v.Validate(); err != nil {
			return prefixError(err, at)
		}
	}
	return nil
}

func (d *Decoder) describe(at string) string {
	if at != "" {
		return at
	}
	if d.Root != "" {
		return d.Root
	}
	return "value"
}

func prefixError(err error, at string) error {
	if at == "" {
		return err
	}
	return Errorf("%s: %s", at, err.Error())
}

type fieldInfo struct {
	name     string
	index    []int
	optional bool
}

func (d *Decoder) structFields(t reflect.Type) []fieldInfo {
	if cached, ok := d.fields.Load(t); ok {
		return cached.([]fieldInfo)
	}
	fields := make([]fieldInfo, 0, t.NumField())
	for i := range t.NumField() {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		tag := sf.Tag.Get(d.Tag)
		if tag == "-" {
			continue
		}
		name, rest, _ := strings.Cut(tag, ",")
		if name == "" {
			name = sf.Name
		}
		fields = append(fields, fieldInfo{
			name:     name,
			index:    sf.Index,
			optional: strings.Contains(rest, "omitempty"),
		})
	}
	d.fields.Store(t, fields)
	return fields
}
