package chord

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/sky-valley/pi/chord/delta"
)

// Port of packages/chord/test/json.test.ts at 19a0361be.

// TestIsValueChecksStrictJSONWithoutNormalizingIt is upstream's single case,
// one assertion per line of it. Go has no undefined, so the "omitted"
// property carries the nearest thing — a value of a non-JSON kind — and the
// Uint8Array case is a []byte, which encoding/json would base64 rather than
// write as an array.
func TestIsValueChecksStrictJSONWithoutNormalizingIt(t *testing.T) {
	if !IsValue(map[string]any{"nested": []any{1, true, nil}}) {
		t.Error("nested plain JSON rejected")
	}
	if IsValue(map[string]any{"omitted": func() {}}) {
		t.Error("object holding a non-JSON member accepted")
	}
	if IsValue([]byte{1}) {
		t.Error("byte string accepted as an array")
	}
	if IsValue(math.Inf(1)) {
		t.Error("+Inf accepted")
	}
	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	if IsValue(cyclic) {
		t.Error("cyclic object accepted")
	}
}

// TestIsValueScalars pins the Go spelling of upstream's scalar rules: every
// numeric kind is a number, only finite floats pass, and Go-only kinds are not
// JSON.
func TestIsValueScalars(t *testing.T) {
	type named string
	accept := []any{nil, true, "s", named("n"), 0, int8(1), uint64(2), uintptr(3), float32(1.5), 2.5}
	for _, v := range accept {
		if !IsValue(v) {
			t.Errorf("IsValue(%#v) = false, want true", v)
		}
	}
	reject := []any{math.NaN(), math.Inf(-1), float32(math.Inf(1)), struct{}{}, new(int), make(chan int), complex(1, 0)}
	for _, v := range reject {
		if IsValue(v) {
			t.Errorf("IsValue(%#v) = true, want false", v)
		}
	}
}

// TestIsValueContainers: arrays and objects are checked by kind, so typed
// slices and string-keyed maps count, but a non-string key does not (upstream
// rejects symbol keys), and a struct is a class instance, not a plain object.
func TestIsValueContainers(t *testing.T) {
	type key string
	accept := []any{[]any{}, []string{"a"}, [2]int{1, 2}, map[string]any{}, map[key]int{"k": 1}, map[string][]any{"a": {nil}}}
	for _, v := range accept {
		if !IsValue(v) {
			t.Errorf("IsValue(%#v) = false, want true", v)
		}
	}
	reject := []any{map[int]any{1: "a"}, []any{math.NaN()}, map[string]any{"a": struct{}{}}, struct{ A int }{1}, []struct{}{{}}}
	for _, v := range reject {
		if IsValue(v) {
			t.Errorf("IsValue(%#v) = true, want false", v)
		}
	}
}

// TestIsValueHasNoDepthLimit: upstream dropped isJsonValue's 512-level cap
// (19a0361be) for an ancestor set, so deep nesting is a value and a cycle —
// through every container kind, including the typed ones that walk by
// reflection — is not.
func TestIsValueHasNoDepthLimit(t *testing.T) {
	type list []any
	type object map[string]any
	nest := func(n int, wrap func(any) any) any {
		var v any = wrap(nil)
		for range n - 1 {
			v = wrap(v)
		}
		return v
	}
	for name, wrap := range map[string]func(any) any{
		"[]any": func(v any) any {
			if v == nil {
				return []any{}
			}
			return []any{v}
		},
		"map[string]any": func(v any) any {
			if v == nil {
				return map[string]any{}
			}
			return map[string]any{"k": v}
		},
		"typed slice": func(v any) any {
			if v == nil {
				return list{}
			}
			return list{v}
		},
		"typed map": func(v any) any {
			if v == nil {
				return object{}
			}
			return object{"k": v}
		},
	} {
		deep := nest(5_000, wrap)
		if !IsValue(deep) {
			t.Errorf("%s: 5,000 nested rejected", name)
		}
		if _, err := CopyJSON(deep); err != nil {
			t.Errorf("%s: CopyJSON of 5,000 nested: %v", name, err)
		}
	}
	loop := make([]any, 1)
	loop[0] = loop
	typedLoop := make(list, 1)
	typedLoop[0] = typedLoop
	self := map[string]any{}
	self["self"] = self
	typedSelf := object{}
	typedSelf["self"] = typedSelf
	indirect := map[string]any{}
	indirect["rows"] = []any{map[string]any{"back": indirect}}
	for name, cyclic := range map[string]any{"[]any": loop, "typed slice": typedLoop, "map[string]any": self, "typed map": typedSelf, "through an array": indirect} {
		if IsValue(cyclic) {
			t.Errorf("%s: a cycle accepted", name)
		}
		var ve *ValueError
		if _, err := CopyJSON(cyclic); !errors.As(err, &ve) || ve.Message != "Value contains cycles and is not strict JSON" {
			t.Errorf("%s: CopyJSON of a cycle: %v", name, err)
		}
	}
	// An alias is not a cycle: two paths to one container.
	shared := []any{1}
	if !IsValue([]any{shared, shared, map[string]any{"a": shared}}) {
		t.Error("an aliased container is not a cycle")
	}
}

// TestCopyJSON is json.test.ts's copyJson cases: aliases become independent
// copies, an own "__proto__" member is kept, and cycles and non-strict values
// are refused with upstream's texts.
func TestCopyJSON(t *testing.T) {
	shared := map[string]any{"value": 1}
	input := map[string]any{"left": shared, "right": shared}
	v, err := CopyJSON(input)
	if err != nil {
		t.Fatal(err)
	}
	copied := v.(map[string]any)
	left, right := copied["left"].(map[string]any), copied["right"].(map[string]any)
	if left["value"] != 1.0 || right["value"] != 1.0 {
		t.Errorf("copied %v", copied)
	}
	left["value"] = 2.0
	if right["value"] != 1.0 || shared["value"] != 1 {
		t.Error("the copy retained an alias")
	}

	v, err = CopyJSON(map[string]any{"__proto__": map[string]any{"safe": true}})
	if err != nil {
		t.Fatal(err)
	}
	if safe := v.(map[string]any)["__proto__"].(map[string]any)["safe"]; safe != true {
		t.Errorf(`own "__proto__" member lost: %v`, v)
	}

	for _, tc := range []struct {
		value any
		text  string
	}{
		{math.NaN(), "Value contains a non-finite number and is not strict JSON"},
		{[]any{math.Inf(1)}, "Value contains a non-finite number and is not strict JSON"},
		{map[string]any{"f": func() {}}, "Value contains a non-JSON function; expected strict JSON"},
		{[]byte("a"), "Value must contain strict JSON plain objects or arrays"},
		{struct{ A int }{1}, "Value must contain strict JSON plain objects or arrays"},
		{map[int]any{1: "a"}, "Value must contain strict JSON plain objects or arrays"},
	} {
		var ve *ValueError
		if _, err := CopyJSON(tc.value); !errors.As(err, &ve) || ve.Message != tc.text {
			t.Errorf("CopyJSON(%#v): %v, want %q", tc.value, err, tc.text)
		}
		if IsValue(tc.value) {
			t.Errorf("IsValue(%#v) accepts what CopyJSON refuses", tc.value)
		}
	}

	// The copy is the decoded-tree representation: float64 numbers, []any
	// and map[string]any, whatever Go kinds went in.
	type key string
	v, err = CopyJSON(map[key][]int{"k": {1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	if xs, ok := v.(map[string]any)["k"].([]any); !ok || len(xs) != 2 || xs[0] != 1.0 {
		t.Errorf("typed containers copied as %#v", v)
	}
}

// IsValue is chord's public strict-JSON check (pi isJsonValue, a root export),
// which a host may run over every payload it relays, so a tree encoding/json
// decoded is checked without allocating: no sorted keys, no boxed values, and
// no ancestor set until the walk is deep enough to be a cycle.
func TestIsValueDoesNotAllocate(t *testing.T) {
	rows := make([]any, 2_000)
	for i := range rows {
		rows[i] = map[string]any{"id": float64(i), "label": "row", "tags": []any{"a", true, nil}, "nested": map[string]any{"x": 1.5}}
	}
	payload := map[string]any{"rows": rows, "total": 2_000.0}
	if allocs := testing.AllocsPerRun(10, func() {
		if !IsValue(payload) {
			t.Fatal("payload rejected")
		}
	}); allocs != 0 {
		t.Errorf("IsValue allocated %v times on a decoded payload", allocs)
	}
}

// A live draft is a value, read through it as pi's isJsonValue and copyJson
// read a proxy — by itself or inside another value; a settled one is not.
func TestCopyJSONReadsADraft(t *testing.T) {
	tr := delta.Track[any](map[string]any{"a": 1.0, "list": []any{1.0, map[string]any{"b": 1.0}, 3.0}})
	c, err := tr.BeginChange()
	if err != nil {
		t.Fatal(err)
	}
	state := c.State()
	if err := state.Set("a", 5.0); err != nil {
		t.Fatal(err)
	}
	if err := state.At("list").At(1).Set("b", 2.0); err != nil {
		t.Fatal(err)
	}
	if !IsValue(state) || !IsValue([]any{state}) {
		t.Error("a live draft is not a value")
	}
	v, err := CopyJSON(map[string]any{"draft": state})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(v)
	if string(got) != `{"draft":{"a":5,"list":[1,{"b":2},3]}}` {
		t.Errorf("CopyJSON of a draft: %s", got)
	}
	c.Abort()
	if IsValue(state) {
		t.Error("a settled draft is a value")
	}
	if _, err := CopyJSON(state); !errors.Is(err, delta.ErrDraftSettled) {
		t.Errorf("CopyJSON of a settled draft: %v, want delta.ErrDraftSettled", err)
	}
}
