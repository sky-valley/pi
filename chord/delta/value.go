package delta

import (
	"cmp"
	"encoding/json"
	"math"
	"reflect"
	"slices"

	"github.com/sky-valley/pi/internal/jstext"
)

// ─── JSON values in Go ───────────────────────────────────────────────────────
//
// The port's own layer, with no upstream file behind it: what JavaScript's
// === and object identity mean for the tree encoding/json decodes — nil,
// bool, float64, string, []any, map[string]any.
//
// A container's identity is its address: a map's header, a slice's first
// element and length. Two empty Go slices with no capacity share one
// zero-size address, so an empty slice with no capacity — every [] that
// encoding/json decodes — is identical to nothing (see anonymous), and every
// array the port allocates for a revision or a draft has a backing array of its
// own, capacity at least one (newArray).

// identity is a container's JavaScript object identity: the map header's
// address, or a slice's backing array together with its length (a shorter
// view of the same backing array is a different array). len is -1 for a map.
type identity struct {
	ptr uintptr
	len int
}

// identityOf reports v's identity when v is a container.
func identityOf(v any) (identity, bool) {
	switch c := v.(type) {
	case map[string]any:
		return identity{ptr: reflect.ValueOf(v).Pointer(), len: -1}, true
	case []any:
		return identity{ptr: reflect.ValueOf(v).Pointer(), len: len(c)}, true
	}
	return identity{}, false
}

// anonymous reports whether v is an array with no identity of its own: an
// empty slice with no capacity. Go gives every such slice one address, so
// the []s encoding/json decodes — each its own array in JavaScript, since
// JSON.parse makes a new one for each — would otherwise all be ONE array, and
// anchor alignments pi never makes. Treating them as identical to nothing is
// exact for decoded values; the tracker's own arrays always have capacity, so
// a caller that means to share an empty array between revisions gives it one
// too (make([]any, 0, 1)).
func anonymous(v any) bool {
	xs, ok := v.([]any)
	return ok && cap(xs) == 0
}

func isContainer(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return true
	}
	return false
}

// newArray allocates an array the tracker owns: length n, and a backing array
// of its own even when n is 0, so that its identity is its own.
func newArray(n int) []any {
	return make([]any, n, max(n, 1))
}

// cloneArray is a shallow copy with a backing array of its own.
func cloneArray(xs []any) []any {
	out := newArray(len(xs))
	copy(out, xs)
	return out
}

// same is upstream's `left === right` on two JSON values: scalars by value —
// numbers of any Go kind as the one JavaScript number, so 0 === -0 — and
// containers by identity, which an anonymous array does not have.
func same(a, b any) bool {
	switch x := a.(type) {
	case nil:
		return b == nil
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	case map[string]any, []any:
		if anonymous(x) {
			// An anonymous b needs no check of its own: no container with an
			// identity has its zero-size address.
			return false
		}
		ia, _ := identityOf(x)
		ib, ok := identityOf(b)
		return ok && ia == ib
	}
	if fa, ok := number(a); ok {
		fb, ok := number(b)
		return ok && fa == fb
	}
	return false
}

// objectIs is Object.is: same, except that 0 and -0 differ (and NaN, which no
// JSON value holds, would equal itself).
func objectIs(a, b any) bool {
	fa, oka := number(a)
	fb, okb := number(b)
	if oka && okb {
		return fa == fb && math.Signbit(fa) == math.Signbit(fb) || fa != fa && fb != fb
	}
	return same(a, b)
}

// valueKey is a JSON value as a key of upstream's Map<JsonValue, …>, which
// compares with SameValueZero: scalars by value, containers by identity.
type valueKey struct {
	kind byte // 'n'ull, 'b'ool, 'f'loat, 's'tring, 'c'ontainer
	b    bool
	f    float64
	s    string
	id   identity
}

// keyOf is v's map key; false for a value that is not JSON, or an anonymous
// array, which matches nothing.
func keyOf(v any) (valueKey, bool) {
	switch x := v.(type) {
	case nil:
		return valueKey{kind: 'n'}, true
	case bool:
		return valueKey{kind: 'b', b: x}, true
	case string:
		return valueKey{kind: 's', s: x}, true
	case map[string]any, []any:
		if anonymous(x) {
			return valueKey{}, false
		}
		id, _ := identityOf(x)
		return valueKey{kind: 'c', id: id}, true
	}
	if f, ok := number(v); ok {
		// A Go map compares float64 keys with ==, under which -0 is 0: the
		// SameValueZero upstream's Map applies.
		return valueKey{kind: 'f', f: f}, true
	}
	return valueKey{}, false
}

// number reads any Go numeric kind as the one JSON number it is, json.Number
// included — a UseNumber decoder yields those, and pi holds every JSON number
// in one type.
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return float64(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		return rv.Float(), true
	}
	return 0, false
}

// jsonEqual is upstream's equalJson: same, or two containers of one kind whose
// members are pairwise equalJson. A nil map or slice is null, as encoding/json
// writes it: equal to null, and to no container.
func jsonEqual(a, b any) bool {
	if same(a, b) {
		return true
	}
	if na, nb := isNull(a), isNull(b); na || nb {
		return na && nb
	}
	switch x := a.(type) {
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jsonEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !jsonEqual(v, w) {
				return false
			}
		}
		return true
	}
	return false
}

// isNull reports whether v is JSON null: nil, or a nil map or slice.
func isNull(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case map[string]any:
		return x == nil
	case []any:
		return x == nil
	}
	return false
}

// ─── Key order ───────────────────────────────────────────────────────────────

// keyOrder is the order an object's members are enumerated in: integer-like
// keys first, ascending — JavaScript's own rule for them — then the rest by
// UTF-16 code unit. JavaScript enumerates the rest in insertion order, which a
// Go map does not keep; this is the order of an object whose members were
// inserted sorted (ledger Divergences, "chord/delta immutable tracking").
func keyOrder(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, compareKeys)
	return keys
}

func compareKeys(a, b string) int {
	ia, oka := canonicalIndex(a)
	ib, okb := canonicalIndex(b)
	switch {
	case oka && okb:
		return cmp.Compare(ia, ib)
	case oka:
		return -1
	case okb:
		return 1
	}
	return jstext.CompareUTF16(a, b)
}

// canonicalIndex reports whether s is an array index as JavaScript spells
// one: the decimal form of an integer in [0, 2^32-2], without sign or
// leading zeros. It parses 32 bits whatever the platform's int, so an index
// past 2^31 is one on a 32-bit build too.
func canonicalIndex(s string) (int64, bool) {
	// The common case, a key that is not a number at all, is settled without
	// strconv, whose error for it allocates.
	if s == "" || len(s) > 10 || s[0] < '0' || s[0] > '9' || s[0] == '0' && len(s) > 1 {
		return 0, false
	}
	var n int64
	for i := range len(s) {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	if n > maxArrayIndex {
		return 0, false
	}
	return n, true
}

// maxArrayIndex is the largest index a JavaScript array has: 2^32 - 2.
const maxArrayIndex = 1<<32 - 2
