package ai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"

	"github.com/sky-valley/pi/internal/jstext"
)

// OrderedField is one key/value pair of an OrderedObject.
type OrderedField struct {
	Key   string
	Value any
}

// OrderedObject marshals key/value pairs in slice order, mirroring
// JSON.stringify of a JS object — whose own keys list array indices first,
// ascending, then the rest in the order they were added — for byte-exact
// request bodies.
// Values may nest further OrderedObjects and []any, so the order of an entire
// decoded document survives, not just its top level. A number that is not
// finite, in the object or in an array in it, is written null, as
// JSON.stringify writes Infinity and NaN; encoding/json would refuse it.
type OrderedObject []OrderedField

func (o OrderedObject) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := writeJSONString(&buf, f.Key); err != nil {
			return nil, err
		}
		buf.WriteByte(':')
		if err := marshalOrderedValue(&buf, f.Value); err != nil {
			return nil, err
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// marshalOrderedValue writes v as json.Marshal does, except that a number,
// directly or in an array, is written as JSON.stringify writes it: null when
// it is not finite, and 0 for negative zero, which json.Marshal writes -0.
func marshalOrderedValue(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case string:
		return writeJSONString(buf, t)
	case float64:
		if math.IsInf(t, 0) || math.IsNaN(t) {
			buf.WriteString("null")
			return nil
		}
		if t == 0 {
			buf.WriteByte('0')
			return nil
		}
	case []any:
		if t == nil {
			break // json.Marshal's null
		}
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := marshalOrderedValue(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	buf.Write(b)
	return nil
}

// writeJSONString writes s as json.Marshal does, except for half a surrogate
// pair, held as its WTF-8 triple (a tool call's arguments keep one as
// JSON.parse does): JSON.stringify writes it as a backslash-u escape of the
// code unit, where json.Marshal would write each of its three bytes as
// U+FFFD.
func writeJSONString(buf *bytes.Buffer, s string) error {
	start := 0
	quoted := false
	for i := 0; i < len(s); {
		unit, ok := jstext.WTF8Surrogate(s[i:])
		if !ok {
			i++
			continue
		}
		if !quoted {
			buf.WriteByte('"')
			quoted = true
		}
		if err := writeJSONStringBody(buf, s[start:i]); err != nil {
			return err
		}
		fmt.Fprintf(buf, `%cu%04x`, '\\', unit)
		i += 3
		start = i
	}
	if !quoted {
		b, err := json.Marshal(s)
		buf.Write(b)
		return err
	}
	if err := writeJSONStringBody(buf, s[start:]); err != nil {
		return err
	}
	buf.WriteByte('"')
	return nil
}

// writeJSONStringBody writes s as json.Marshal does, less the quotes.
func writeJSONStringBody(buf *bytes.Buffer, s string) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	buf.Write(b[1 : len(b)-1])
	return nil
}

// UnmarshalJSON reads a JSON object as DecodeOrderedValue does, so an object
// read back from JSON keeps the key order JSON.parse's object lists, at every
// depth. null leaves o as it is, as the json.Unmarshaler convention asks
// (encoding/json's own decode of null would set the slice to nil).
func (o *OrderedObject) UnmarshalJSON(data []byte) error {
	if string(bytes.TrimSpace(data)) == "null" {
		return nil
	}
	v, err := DecodeOrderedValue(data)
	if err != nil {
		return err
	}
	obj, ok := v.(OrderedObject)
	if !ok {
		return fmt.Errorf("json: cannot unmarshal %.20s into an ai.OrderedObject: the value must be a JSON object", bytes.TrimSpace(data))
	}
	*o = obj
	return nil
}

// Get returns the value of key, as a property read of the object would, and
// whether the object has it.
func (o OrderedObject) Get(key string) (any, bool) {
	if i := o.indexOf(key); i >= 0 {
		return o[i].Value, true
	}
	return nil, false
}

// Plain projects the object onto the map form `encoding/json` would have
// decoded, dropping the recorded order. It is what makes an OrderedObject
// checkable against the map it accompanies.
func (o OrderedObject) Plain() map[string]any {
	m := make(map[string]any, len(o))
	for _, f := range o {
		m[f.Key] = plainValue(f.Value)
	}
	return m
}

func plainValue(v any) any {
	switch t := v.(type) {
	case OrderedObject:
		return t.Plain()
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = plainValue(e)
		}
		return out
	default:
		return v
	}
}

// DecodeOrderedObject decodes a JSON object into both the map `encoding/json`
// would produce and an order-preserving twin, holding what JSON.parse returns
// (DecodeOrderedValue's values): the twin lists the keys in the order
// JSON.parse's object does (see parseOrdered), numbers are float64 — ±Inf past
// float64's range, where json.Unmarshal fails — and nulls nil. The two forms
// differ only in whether key order survives.
//
// Anything that is not a single, complete JSON object is an error, matching
// json.Unmarshal into a map[string]any.
func DecodeOrderedObject(data []byte) (map[string]any, OrderedObject, error) {
	v, err := parseOrdered(data)
	if err != nil {
		return nil, nil, err
	}
	obj, ok := v.(OrderedObject)
	if !ok {
		return nil, nil, fmt.Errorf("expected a JSON object, got %s", bytes.TrimSpace(data)[:1])
	}
	return obj.Plain(), obj, nil
}

// DecodeOrderedValue decodes any single, complete JSON value the way
// JSON.parse does, in encoding/json's shapes: every object (at any depth)
// becomes an OrderedObject, in the order JSON.parse's object lists its keys,
// an array is []any, and scalars are float64, string, bool or nil. A number
// past float64's range is ±Inf, as JSON.parse reads it, where json.Unmarshal
// fails. It is the value pi hands to an observer of a parsed JSON event,
// whose key order a JSON.stringify of it would reveal. It reads the text in
// one pass (parseOrdered).
func DecodeOrderedValue(data []byte) (any, error) {
	return parseOrdered(data)
}

// DecodeOrderedValueKeepingSurrogates is DecodeOrderedValue, except that half
// a surrogate pair — an escape whose other half does not follow, or a WTF-8
// surrogate triple — is kept as its WTF-8 triple, as JSON.parse keeps the
// code unit, rather than read as U+FFFD. Adapters read streamed tool-call
// argument deltas with it, so a pair cut between two deltas joins again in
// the arguments (ToolCallArgumentsStream), as it does in pi.
func DecodeOrderedValueKeepingSurrogates(data []byte) (any, error) {
	return parseOrderedKeepingSurrogates(data)
}
