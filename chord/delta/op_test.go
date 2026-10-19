package delta

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

// tree parses a JSON literal into the any-tree encoding/json produces, which is
// the form ParseOp/ParseWireOp take — the Go counterpart of the `unknown`
// upstream's assertValidOp takes.
func tree(t *testing.T, literal string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(literal), &v); err != nil {
		t.Fatalf("bad test literal %s: %v", literal, err)
	}
	return v
}

// delta.test.ts "assertions" → "assertValidOp accepts decoded ops and rejects
// wire forms". Validating Op against the wire grammar would be laxer than the
// type: a two-element ["s", value] would pass, and apply would read the value
// as a path. Each vocabulary gets the validator that matches it.
func TestParseOpAcceptsDecodedOpsAndRejectsWireForms(t *testing.T) {
	decoded := []struct {
		literal string
		want    Op
	}{
		{`["r", {"a": 1}]`, Replace{Value: map[string]any{"a": float64(1)}}},
		{`["s", ["a"], 1]`, Set{Path: Path{Key("a")}, Value: float64(1)}},
		{`["d", ["a"]]`, Delete{Path: Path{Key("a")}}},
		{`["a", ["a"], "x"]`, Append{Path: Path{Key("a")}, Text: "x"}},
		{`["t", ["a"], 2]`, Truncate{Path: Path{Key("a")}, Count: 2}},
		{`["p", ["a"], 0, 0, []]`, Splice{Path: Path{Key("a")}, Index: 0, Remove: 0, Items: []any{}}},
		{`["m", ["a"], [1, 0]]`, Permute{Path: Path{Key("a")}, Permutation: []int{1, 0}}},
		{`["m", [], []]`, Permute{Path: Path{}, Permutation: []int{}}},
	}
	for _, tc := range decoded {
		got, err := ParseOp(tree(t, tc.literal))
		if err != nil {
			t.Errorf("ParseOp(%s): unexpected error %v", tc.literal, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseOp(%s) = %#v, want %#v", tc.literal, got, tc.want)
		}
	}

	wireOnly := []struct {
		literal string
		want    WireOp
	}{
		{`["s", 1]`, WireSet{Value: float64(1)}},
		{`["d"]`, WireDelete{}},
		{`["a", "x"]`, WireAppend{Text: "x"}},
		{`["t", 2]`, WireTruncate{Count: 2}},
		{`["p", 0, 0, []]`, WireSplice{Items: []any{}}},
		{`["m", [1, 0]]`, WirePermute{Permutation: []int{1, 0}}},
		{`["m", 0, [1, 0]]`, WirePermute{Ref: PathID(0), Permutation: []int{1, 0}}},
		{`["#", 0, ["a"]]`, Define{ID: 0, Path: Path{Key("a")}}},
		{`["s", 0, 1]`, WireSet{Ref: PathID(0), Value: float64(1)}},
	}
	for _, tc := range wireOnly {
		if _, err := ParseOp(tree(t, tc.literal)); err == nil {
			t.Errorf("ParseOp(%s): wire-only form accepted by the decoded validator", tc.literal)
		}
		got, err := ParseWireOp(tree(t, tc.literal))
		if err != nil {
			t.Errorf("ParseWireOp(%s): unexpected error %v", tc.literal, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseWireOp(%s) = %#v, want %#v", tc.literal, got, tc.want)
		}
	}
}

// The decoded forms are also valid wire ops: an inline path is a PathRef.
func TestParseWireOpAcceptsInlinePaths(t *testing.T) {
	cases := []struct {
		literal string
		want    WireOp
	}{
		{`["r", {"a": 1}]`, Replace{Value: map[string]any{"a": float64(1)}}},
		{`["s", ["a"], 1]`, WireSet{Ref: Path{Key("a")}, Value: float64(1)}},
		{`["d", ["a"]]`, WireDelete{Ref: Path{Key("a")}}},
		{`["a", ["a"], "x"]`, WireAppend{Ref: Path{Key("a")}, Text: "x"}},
		{`["t", ["a"], 2]`, WireTruncate{Ref: Path{Key("a")}, Count: 2}},
		{`["p", ["a"], 0, 0, []]`, WireSplice{Ref: Path{Key("a")}, Items: []any{}}},
		{`["p", [], 1, 0, [null]]`, WireSplice{Ref: Path{}, Index: 1, Items: []any{nil}}},
	}
	for _, tc := range cases {
		got, err := ParseWireOp(tree(t, tc.literal))
		if err != nil {
			t.Errorf("ParseWireOp(%s): unexpected error %v", tc.literal, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseWireOp(%s) = %#v, want %#v", tc.literal, got, tc.want)
		}
	}
}

// delta.test.ts "assertions" → "does not recursively inspect operation payloads".
func TestValidatorsDoNotInspectPayloads(t *testing.T) {
	// A Go map stands in for upstream's `new Map()`: neither is a JSON value.
	if _, err := ParseOp([]any{"s", []any{"value"}, map[int]int{1: 2}}); err != nil {
		t.Errorf("ParseOp rejected an s payload it must not inspect: %v", err)
	}
	if _, err := ParseWireOp([]any{"r", time.Now()}); err != nil {
		t.Errorf("ParseWireOp rejected an r payload it must not inspect: %v", err)
	}
}

// delta.test.ts "safety: op structure" — the CVE-2025-55182 lesson: a decoder
// must not trust tuple shape. Upstream drives these through apply, which calls
// assertValidOp first; the validator is the layer that rejects them.
func TestOpStructure(t *testing.T) {
	t.Run("rejects an unknown verb rather than skipping it", func(t *testing.T) {
		// Silently skipping is how a newer producer's op vanishes and a replica drifts.
		if _, err := ParseOp(tree(t, `["ZZZ", ["a"], 9]`)); err == nil {
			t.Fatal("unknown verb accepted")
		}
		if _, err := ParseWireOp(tree(t, `["ZZZ", ["a"], 9]`)); err == nil {
			t.Fatal("unknown wire verb accepted")
		}
	})
	t.Run("rejects non-array splice items", func(t *testing.T) {
		// Unvalidated, this spreads a string: ["n","o","t","-","a",…]
		if _, err := ParseOp(tree(t, `["p", ["xs"], 0, 0, "not-an-array"]`)); err == nil {
			t.Fatal("string items accepted")
		}
		if _, err := ParseWireOp(tree(t, `["p", 0, 0, "not-an-array"]`)); err == nil {
			t.Fatal("string items accepted on the wire")
		}
	})
	t.Run("rejects a string path", func(t *testing.T) {
		// Unvalidated, "a".slice(0,-1) is "", so it resolved to the ROOT and wrote.
		if _, err := ParseOp(tree(t, `["s", "a", 9]`)); err == nil {
			t.Fatal("string path accepted")
		}
		if _, err := ParseWireOp(tree(t, `["s", "a", 9]`)); err == nil {
			t.Fatal("string path accepted on the wire")
		}
	})
	t.Run("rejects a non-tuple op", func(t *testing.T) {
		for _, v := range []any{tree(t, `{"op": "s"}`), nil, "s", tree(t, `[]`)} {
			if _, err := ParseOp(v); err == nil {
				t.Errorf("ParseOp(%#v): non-tuple accepted", v)
			}
			if _, err := ParseWireOp(v); err == nil {
				t.Errorf("ParseWireOp(%#v): non-tuple accepted", v)
			}
		}
	})
	t.Run("rejects negative truncation", func(t *testing.T) {
		if _, err := ParseOp(tree(t, `["t", ["a"], -1]`)); err == nil {
			t.Fatal("negative t count accepted")
		}
		if _, err := ParseWireOp(tree(t, `["t", ["a"], -1]`)); err == nil {
			t.Fatal("negative t count accepted on the wire")
		}
		if _, err := ParseWireOp(tree(t, `["t", -1]`)); err == nil {
			t.Fatal("negative short-form t count accepted on the wire")
		}
		if err := (Truncate{Path: Path{Key("a")}, Count: -1}).Validate(); err == nil {
			t.Fatal("typed negative t count validated")
		}
	})
	t.Run("rejects fractional and negative splice bounds", func(t *testing.T) {
		for _, literal := range []string{
			`["p", ["xs"], 1.5, 0, []]`, `["p", ["xs"], 0, -1, []]`, `["p", ["xs"], -1, 0, []]`,
		} {
			if _, err := ParseOp(tree(t, literal)); err == nil {
				t.Errorf("ParseOp(%s): accepted", literal)
			}
		}
		if err := (Splice{Index: -1}).Validate(); err == nil {
			t.Error("typed negative p index validated")
		}
		if err := (Splice{Remove: -1}).Validate(); err == nil {
			t.Error("typed negative p remove validated")
		}
	})
	t.Run("rejects wrong arity", func(t *testing.T) {
		for _, literal := range []string{
			`["r"]`, `["r", 1, 2]`, `["s", ["a"]]`, `["s", ["a"], 1, 2]`, `["d", ["a"], 1]`,
			`["a", ["a"]]`, `["t", ["a"]]`, `["p", ["a"], 0, 0]`, `["p", ["a"], 0, 0, [], 1]`,
		} {
			if _, err := ParseOp(tree(t, literal)); err == nil {
				t.Errorf("ParseOp(%s): accepted", literal)
			}
		}
		for _, literal := range []string{
			`["r"]`, `["s"]`, `["s", ["a"], 1, 2]`, `["d", ["a"], 1]`, `["a"]`, `["a", ["a"]]`,
			`["t"]`, `["p", 0, 0]`, `["p", ["a"], 0, 0, [], 1]`, `["#", 0]`, `["#", 0, ["a"], 1]`,
		} {
			if _, err := ParseWireOp(tree(t, literal)); err == nil {
				t.Errorf("ParseWireOp(%s): accepted", literal)
			}
		}
	})
	t.Run("rejects a non-string append text", func(t *testing.T) {
		if _, err := ParseOp(tree(t, `["a", ["a"], 1]`)); err == nil {
			t.Fatal("numeric a text accepted")
		}
		if _, err := ParseWireOp(tree(t, `["a", 1]`)); err == nil {
			t.Fatal("numeric short-form a text accepted on the wire")
		}
	})
	t.Run("rejects a bad path id", func(t *testing.T) {
		for _, literal := range []string{`["s", -1, 1]`, `["s", 1.5, 1]`, `["#", -1, ["a"]]`, `["#", 0.5, ["a"]]`, `["#", 0, "a"]`} {
			if _, err := ParseWireOp(tree(t, literal)); err == nil {
				t.Errorf("ParseWireOp(%s): accepted", literal)
			}
		}
	})
	t.Run("root paths are legal only for p", func(t *testing.T) {
		if _, err := ParseOp(tree(t, `["p", [], 0, 0, [1]]`)); err != nil {
			t.Errorf("root p rejected: %v", err)
		}
		for _, literal := range []string{`["s", [], 1]`, `["d", []]`, `["a", [], "x"]`, `["t", [], 1]`} {
			if _, err := ParseOp(tree(t, literal)); err == nil {
				t.Errorf("ParseOp(%s): root path accepted", literal)
			}
		}
		for _, op := range []Op{Set{Value: 1}, Delete{}, Append{Text: "x"}, Truncate{Count: 1}} {
			if err := op.Validate(); err == nil {
				t.Errorf("%T with an empty path validated", op)
			}
		}
		if err := (Splice{Items: []any{1}}).Validate(); err != nil {
			t.Errorf("typed root p rejected: %v", err)
		}
	})
	t.Run("shape errors carry the sentinel", func(t *testing.T) {
		_, err := ParseOp(tree(t, `["s", "a", 9]`))
		if !errors.Is(err, ErrInvalidOp) {
			t.Fatalf("got %v, want ErrInvalidOp", err)
		}
	})
}

// delta.test.ts "flush" → "rejects a constructor walk" and "rejects a forbidden
// path reached through an interned id". ({}).constructor.constructor is Function
// — the classic escape ladder. Both validators refuse the path before an applier
// or decoder could walk it.
func TestOpValidatorsRejectReservedPaths(t *testing.T) {
	_, err := ParseOp(tree(t, `["s", ["constructor", "prototype", "gadget"], true]`))
	var unsafe *UnsafePathError
	if !errors.As(err, &unsafe) {
		t.Fatalf("constructor walk: got %v, want *UnsafePathError", err)
	}
	if unsafe.Segment != Key("constructor") {
		t.Errorf("Segment = %#v, want Key(\"constructor\")", unsafe.Segment)
	}

	_, err = ParseWireOp(tree(t, `["#", 0, ["__proto__", "w"]]`))
	if !errors.As(err, &unsafe) {
		t.Fatalf("interned __proto__: got %v, want *UnsafePathError", err)
	}
	if unsafe.Segment != Key("__proto__") {
		t.Errorf("Segment = %#v, want Key(\"__proto__\")", unsafe.Segment)
	}

	if err := (Set{Path: Path{Key("__proto__"), Key("isAdmin")}, Value: true}).Validate(); !errors.As(err, &unsafe) {
		t.Errorf("typed reserved path: got %v, want *UnsafePathError", err)
	}
	if err := (Define{ID: 0, Path: Path{Key("prototype")}}).Validate(); !errors.As(err, &unsafe) {
		t.Errorf("typed reserved # path: got %v, want *UnsafePathError", err)
	}
}

// upstream's okRef → assertSafePath: an INLINE path through any wire verb is
// checked, not only the "#" definition, and the same holds for a decoded "p"
// (the one decoded verb whose path may be empty and so takes a different
// route). ParseWireOp(["s", ["__proto__", "isAdmin"], true]) is the vector.
func TestValidatorsRejectReservedInlinePathsOnEveryVerb(t *testing.T) {
	var unsafe *UnsafePathError
	for _, literal := range []string{
		`["s", ["__proto__", "isAdmin"], true]`, `["d", ["constructor"]]`, `["a", ["prototype"], "x"]`,
		`["t", ["__proto__"], 1]`, `["p", ["constructor"], 0, 0, []]`, `["p", ["a", "__proto__"], 0, 0, []]`,
		`["m", ["prototype"], [0]]`,
	} {
		if _, err := ParseWireOp(tree(t, literal)); !errors.As(err, &unsafe) {
			t.Errorf("ParseWireOp(%s): got %v, want *UnsafePathError", literal, err)
		}
	}
	for _, literal := range []string{`["p", ["__proto__"], 0, 0, []]`, `["p", ["xs", "constructor"], 0, 0, []]`, `["m", ["xs", "__proto__"], [0]]`} {
		if _, err := ParseOp(tree(t, literal)); !errors.As(err, &unsafe) {
			t.Errorf("ParseOp(%s): got %v, want *UnsafePathError", literal, err)
		}
	}
	// A negative index is unsafe on the same route.
	if _, err := ParseWireOp(tree(t, `["s", ["xs", -1], 1]`)); !errors.As(err, &unsafe) {
		t.Errorf("negative inline index on the wire: got %v, want *UnsafePathError", err)
	}
}

// The typed wire ops check the same constraints as the parser, so an encoder or
// decoder holding typed values re-checks them without re-parsing.
func TestWireOpValidate(t *testing.T) {
	var unsafe *UnsafePathError
	reserved := Path{Key("__proto__"), Key("x")}
	for _, op := range []WireOp{
		WireSet{Ref: reserved, Value: 1}, WireDelete{Ref: reserved}, WireAppend{Ref: reserved, Text: "x"},
		WireTruncate{Ref: reserved, Count: 1}, WireSplice{Ref: reserved}, Define{ID: 0, Path: reserved},
		WireSplice{Ref: Path{Index(-1)}},
	} {
		if err := op.Validate(); !errors.As(err, &unsafe) {
			t.Errorf("%#v.Validate() = %v, want *UnsafePathError", op, err)
		}
	}
	for _, op := range []WireOp{
		WireSet{Ref: PathID(-1), Value: 1}, WireDelete{Ref: PathID(-1)}, WireAppend{Ref: PathID(-1), Text: "x"},
		WireTruncate{Ref: PathID(-1), Count: 1}, WireSplice{Ref: PathID(-1)}, Define{ID: -1, Path: Path{Key("a")}},
		WireTruncate{Count: -1}, WireTruncate{Ref: Path{Key("a")}, Count: -1},
		WireSplice{Index: -1}, WireSplice{Remove: -1}, WireSplice{Ref: PathID(0), Remove: -1},
	} {
		if err := op.Validate(); !errors.Is(err, ErrInvalidOp) {
			t.Errorf("%#v.Validate() = %v, want ErrInvalidOp", op, err)
		}
	}
	for _, op := range []WireOp{
		Replace{}, WireSet{Value: 1}, WireDelete{}, WireAppend{}, WireTruncate{}, WireSplice{},
		WireSet{Ref: PathID(0), Value: 1}, WireSet{Ref: Path{Key("a"), Index(0)}, Value: 1},
		Define{ID: 3, Path: Path{}}, WireSplice{Ref: Path{}},
	} {
		if err := op.Validate(); err != nil {
			t.Errorf("%#v.Validate() = %v, want nil", op, err)
		}
	}
}

// Number.isInteger(1e300) is true, so pi validates a huge count and its apply
// clamps it: slice(1e300) is the empty string, splice(0, 1e300) removes all,
// splice(1e300, 0, x) appends. A count is a quantity measured against a
// length, so past what an int holds it saturates and the outcome is the same.
// A segment or a path id is an address and must be exact: beyond
// Number.MAX_SAFE_INTEGER it is refused. Pinned against pi where pi has an
// outcome; the ["#", 1e300] refusal is the port's (recorded).
func TestHugeIntegers(t *testing.T) {
	op, err := ParseOp(tree(t, `["t", ["a"], 1e300]`))
	if err != nil {
		t.Fatalf("t 1e300: %v", err)
	}
	if op.(Truncate).Count != math.MaxInt {
		t.Errorf("t 1e300: Count = %d, want math.MaxInt", op.(Truncate).Count)
	}
	target := `{"a": "abc", "xs": [1, 2]}`
	wantTree(t, applied(t, target, ops(t, `[["t", ["a"], 1e300]]`)), `{"a": "", "xs": [1, 2]}`)
	wantTree(t, applied(t, target, ops(t, `[["p", ["xs"], 0, 1e300, []]]`)), `{"a": "abc", "xs": []}`)
	wantTree(t, applied(t, target, ops(t, `[["p", ["xs"], 1e300, 0, [7]]]`)), `{"a": "abc", "xs": [1, 2, 7]}`)
	if _, err := ParseWireOp(tree(t, `["t", 1e300]`)); err != nil {
		t.Errorf("short-form t 1e300: %v", err)
	}
	if _, err := ParseOp(tree(t, `["t", ["a"], -1e300]`)); !errors.Is(err, ErrInvalidOp) {
		t.Errorf("t -1e300: got %v, want ErrInvalidOp", err)
	}

	// An address past 2^31 is exact whatever the platform's int: pi writes
	// parent[3e9] to the property "3000000000".
	wantTree(t, applied(t, `{"o": {}}`, ops(t, `[["s", ["o", 3e9], 1]]`)), `{"o": {"3000000000": 1}}`)

	var unsafe *UnsafePathError
	// pi: assertIndexInRange throws UnsafePathError(1e300) for an array parent.
	if _, err := ParseOp(tree(t, `["s", ["xs", 1e300], 1]`)); !errors.As(err, &unsafe) {
		t.Errorf("index 1e300: got %v, want *UnsafePathError", err)
	}
	if err := (Path{Index(1 << 60)}).Validate(); !errors.As(err, &unsafe) {
		t.Errorf("Index(1<<60): got %v, want *UnsafePathError", err)
	}
	if err := (Path{Index(maxSafeInteger)}).Validate(); err != nil {
		t.Errorf("Index(MAX_SAFE_INTEGER): %v", err)
	}
	if _, err := ParseWireOp(tree(t, `["#", 1e300, ["a"]]`)); !errors.Is(err, ErrInvalidOp) {
		t.Errorf("# 1e300: got %v, want ErrInvalidOp", err)
	}
	if err := (WireSet{Ref: PathID(1 << 60)}).Validate(); !errors.Is(err, ErrInvalidOp) {
		t.Errorf("PathID(1<<60): got %v, want ErrInvalidOp", err)
	}
}

// delta.test.ts "flush" → "allows a reserved name as a VALUE key". Reserved as
// segments, not as values.
func TestReservedNameAllowedAsValueKey(t *testing.T) {
	got, err := ParseOp(tree(t, `["s", ["a"], {"__proto__": {"z": 1}}]`))
	if err != nil {
		t.Fatalf("reserved key inside a payload rejected: %v", err)
	}
	want := Set{Path: Path{Key("a")}, Value: map[string]any{"__proto__": map[string]any{"z": float64(1)}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// isBase is exact rather than heuristic: the diff puts r at index 0 or
// nowhere. It works unchanged on either vocabulary because r encodes to itself.
func TestIsBase(t *testing.T) {
	if !IsBase([]Op{Replace{Value: 1}, Set{Path: Path{Key("a")}, Value: 2}}) {
		t.Error("batch opening with r is not base")
	}
	if !IsBase([]WireOp{Replace{Value: 1}}) {
		t.Error("wire batch opening with r is not base")
	}
	if IsBase([]Op{Set{Path: Path{Key("a")}, Value: 2}, Replace{Value: 1}}) {
		t.Error("r not at index 0 counted as base")
	}
	if IsBase([]Op{}) || IsBase([]WireOp(nil)) {
		t.Error("empty batch counted as base")
	}
	if !IsReplace(Replace{}) || IsReplace[WireOp](WireDelete{}) {
		t.Error("IsReplace misclassifies")
	}
	// Replace has value receivers, so *Replace is an Op too.
	if !IsBase([]Op{&Replace{}}) || !IsReplace(&Replace{}) {
		t.Error("*Replace not classified as a replacement")
	}
}

// Goldens from src/delta/README.md "Operation vocabulary" and the encoder
// examples there; each Go op marshals to exactly the tuple pi writes. The
// op's own MarshalJSON is checked because json.Marshal re-escapes <, > and &
// in whatever a Marshaler returns; an Encoder with SetEscapeHTML(false) does
// not, and passes these bytes through verbatim.
func TestOpJSONGoldens(t *testing.T) {
	cases := []struct {
		op   json.Marshaler
		want string
	}{
		{Replace{Value: map[string]any{"output": ""}}, `["r",{"output":""}]`},
		{Set{Path: Path{Key("operation"), Key("message"), Key("content"), Index(0), Key("text")}, Value: "hi"}, `["s",["operation","message","content",0,"text"],"hi"]`},
		{Delete{Path: Path{Key("retry")}}, `["d",["retry"]]`},
		{Append{Path: Path{Key("output")}, Text: "next chunk"}, `["a",["output"],"next chunk"]`},
		{Truncate{Path: Path{Key("output")}, Count: 200}, `["t",["output"],200]`},
		{Splice{Path: Path{Key("xs")}, Index: 2, Remove: 0, Items: []any{3}}, `["p",["xs"],2,0,[3]]`},
		{Splice{Path: nil, Index: 4, Remove: 0, Items: nil}, `["p",[],4,0,[]]`},
		{Set{Path: Path{Key("x")}, Value: []any{"r", map[string]any{"evil": true}}}, `["s",["x"],["r",{"evil":true}]]`},
		{Set{Path: Path{Key("a<b&c")}, Value: "<&>"}, `["s",["a<b&c"],"<&>"]`},
		// Wire: README's adjacent ops on one path, then the second-use definition.
		{WireTruncate{Ref: Path{Key("output")}, Count: 200}, `["t",["output"],200]`},
		{WireAppend{Text: "next chunk"}, `["a","next chunk"]`},
		{Define{ID: 0, Path: Path{Key("output")}}, `["#",0,["output"]]`},
		{WireAppend{Ref: PathID(0), Text: "more"}, `["a",0,"more"]`},
		{WireSet{Ref: PathID(3), Value: nil}, `["s",3,null]`},
		{WireSet{Value: 2}, `["s",2]`},
		{WireDelete{}, `["d"]`},
		{WireDelete{Ref: Path{Key("a")}}, `["d",["a"]]`},
		{WireTruncate{Count: 5}, `["t",5]`},
		{WireSplice{Index: 1, Remove: 2, Items: []any{nil}}, `["p",1,2,[null]]`},
		{WireSplice{Ref: PathID(1), Index: 0, Remove: 0, Items: nil}, `["p",1,0,0,[]]`},
		{Permute{Path: Path{Key("values")}, Permutation: []int{2, 0, 1}}, `["m",["values"],[2,0,1]]`},
		{Permute{}, `["m",[],[]]`},
		{WirePermute{Permutation: []int{1, 2, 0}}, `["m",[1,2,0]]`},
		{WirePermute{Ref: PathID(0), Permutation: []int{0, 2, 1}}, `["m",0,[0,2,1]]`},
	}
	for _, tc := range cases {
		got, err := tc.op.MarshalJSON()
		if err != nil {
			t.Errorf("%#v: %v", tc.op, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("%#v marshals to %s, want %s", tc.op, got, tc.want)
		}
	}
}

func TestOpJSONRoundTrip(t *testing.T) {
	ops := []Op{
		Replace{Value: map[string]any{"a": []any{float64(1), "x", nil, true}}},
		Set{Path: Path{Key("a"), Index(3)}, Value: float64(1.5)},
		Delete{Path: Path{Key("a\x00b")}},
		Append{Path: Path{Key("s")}, Text: "\u2028 tail"},
		Truncate{Path: Path{Key("s")}, Count: 3},
		Splice{Path: Path{}, Index: 1, Remove: 2, Items: []any{map[string]any{"k": "v"}}},
	}
	for _, op := range ops {
		data, err := json.Marshal(op)
		if err != nil {
			t.Fatalf("%#v: %v", op, err)
		}
		got, err := UnmarshalOp(data)
		if err != nil {
			t.Fatalf("UnmarshalOp(%s): %v", data, err)
		}
		if !reflect.DeepEqual(got, op) {
			t.Errorf("round trip of %s: got %#v, want %#v", data, got, op)
		}
	}
	wire := []WireOp{
		Replace{Value: "v"},
		Define{ID: 7, Path: Path{Key("a"), Index(0)}},
		WireSet{Ref: PathID(7), Value: float64(1)},
		WireSet{Value: float64(2)},
		WireDelete{Ref: Path{Key("b")}},
		WireDelete{},
		WireAppend{Ref: PathID(7), Text: "x"},
		WireAppend{Text: "y"},
		WireTruncate{Ref: Path{Key("s")}, Count: 1},
		WireTruncate{Count: 2},
		WireSplice{Ref: Path{}, Index: 0, Remove: 0, Items: []any{float64(1)}},
		WireSplice{Index: 1, Remove: 1, Items: []any{}},
	}
	for _, op := range wire {
		data, err := json.Marshal(op)
		if err != nil {
			t.Fatalf("%#v: %v", op, err)
		}
		got, err := UnmarshalWireOp(data)
		if err != nil {
			t.Fatalf("UnmarshalWireOp(%s): %v", data, err)
		}
		if !reflect.DeepEqual(got, op) {
			t.Errorf("round trip of %s: got %#v, want %#v", data, got, op)
		}
	}
	if _, err := UnmarshalOp([]byte(`["s", 1]`)); err == nil {
		t.Error("UnmarshalOp accepted a wire short form")
	}
	if _, err := UnmarshalOp([]byte(`["s"`)); err == nil {
		t.Error("UnmarshalOp accepted malformed JSON")
	}
}

// Go producers hand ParseOp trees built in code, not only trees encoding/json
// produced: an int index and a json.Number count are both integers.
func TestParseOpAcceptsGoIntegerKinds(t *testing.T) {
	got, err := ParseOp([]any{"p", []any{"xs", 1}, int64(2), json.Number("0"), []any{}})
	if err != nil {
		t.Fatalf("ParseOp: %v", err)
	}
	want := Splice{Path: Path{Key("xs"), Index(1)}, Index: 2, Remove: 0, Items: []any{}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
	if _, err := ParseOp([]any{"t", []any{"s"}, float64(1) + 1e-9}); err == nil {
		t.Error("non-integral float count accepted")
	}
	// A count past the safe range is a quantity and saturates; a segment
	// past it is an address and is refused (TestHugeIntegers).
	if _, err := ParseOp([]any{"t", []any{"s"}, int64(1 << 60)}); err != nil {
		t.Errorf("count beyond Number.MAX_SAFE_INTEGER: %v", err)
	}
	if _, err := ParseOp([]any{"s", []any{"xs", int64(1 << 60)}, 1}); err == nil {
		t.Error("index beyond Number.MAX_SAFE_INTEGER accepted")
	}
}

// ParseOp classifies any tree that spells an op: the one encoding/json
// produces, and one a Go producer builds from the package's own types.
func TestParseOpAcceptsOwnTypes(t *testing.T) {
	got, err := ParseOp([]any{"s", Path{Key("a"), Index(0)}, 1})
	if err != nil {
		t.Fatalf("Path segment tree: %v", err)
	}
	if want := (Set{Path: Path{Key("a"), Index(0)}, Value: 1}); !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
	got, err = ParseOp([]any{"d", []any{Key("a"), Index(0)}})
	if err != nil {
		t.Fatalf("Key/Index in a []any: %v", err)
	}
	if want := (Delete{Path: Path{Key("a"), Index(0)}}); !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
	wire, err := ParseWireOp([]any{"s", PathID(2), 1})
	if err != nil {
		t.Fatalf("PathID ref: %v", err)
	}
	if want := (WireSet{Ref: PathID(2), Value: 1}); !reflect.DeepEqual(wire, want) {
		t.Errorf("got %#v, want %#v", wire, want)
	}
	// Reserved and negative segments are refused on this route too.
	var unsafe *UnsafePathError
	if _, err := ParseOp([]any{"s", Path{Key("__proto__")}, 1}); !errors.As(err, &unsafe) {
		t.Errorf("typed reserved path through ParseOp: got %v, want *UnsafePathError", err)
	}
}

// state-diff.test.ts (upstream 10d1ad621) "validates and encodes permutations",
// the validator half, widened to every shape assertPermutation and the "m"
// arity checks refuse. Each rejection is the message pi's assertValidOp or
// assertValidWireOp threw for the same tuple under node at 10d1ad621.
func TestParsePermute(t *testing.T) {
	cases := []struct {
		literal, pi string
		wire        bool
	}{
		{`["m", ["values"], [0, 0]]`, "not a bijection", false},
		{`["m", ["values"], [0, 2]]`, "not a bijection", false},
		{`["m", ["values"], [-1, 0]]`, "not a bijection", false},
		{`["m", ["values"], [0.5, 0]]`, "not a bijection", false},
		{`["m", ["values"], ["0"]]`, "not a bijection", false},
		{`["m", ["values"], 3]`, "not an array", false},
		{`["m", ["values"]]`, "expects", false},
		{`["m", ["values"], [0], 1]`, "expects", false},
		{`["m", "values", [0]]`, "path", false},
		{`["m", [1, 1]]`, "not a bijection", true},
		{`["m", 0, "x"]`, "not an array", true},
		{`["m"]`, "expects", true},
		{`["m", 0, [0], 1]`, "expects", true},
		{`["m", -1, [0]]`, "path id", true},
	}
	for _, tc := range cases {
		parse := func(v any) error { _, err := ParseOp(v); return err }
		if tc.wire {
			parse = func(v any) error { _, err := ParseWireOp(v); return err }
		}
		err := parse(tree(t, tc.literal))
		if !errors.Is(err, ErrInvalidOp) || !strings.Contains(err.Error(), tc.pi) {
			t.Errorf("%s: got %v, want ErrInvalidOp mentioning %q", tc.literal, err, tc.pi)
		}
	}
	// pi accepts these: the root, the empty permutation, both wire forms.
	for _, literal := range []string{`["m", [], [1, 0]]`, `["m", ["values"], []]`} {
		if _, err := ParseOp(tree(t, literal)); err != nil {
			t.Errorf("ParseOp(%s): %v", literal, err)
		}
	}
	for _, literal := range []string{`["m", [1, 0]]`, `["m", 0, [1, 0]]`, `["m", [], [1, 0]]`} {
		if _, err := ParseWireOp(tree(t, literal)); err != nil {
			t.Errorf("ParseWireOp(%s): %v", literal, err)
		}
	}

	// The typed values check the same constraint without a parse.
	for _, op := range []tuple{
		Permute{Path: Path{Key("values")}, Permutation: []int{0, 0}},
		Permute{Permutation: []int{1}},
		WirePermute{Permutation: []int{-1, 0}},
		WirePermute{Ref: PathID(0), Permutation: []int{2, 0}},
	} {
		if err := op.Validate(); !errors.Is(err, ErrInvalidOp) || !strings.Contains(err.Error(), "not a bijection") {
			t.Errorf("%#v.Validate() = %v, want a bijection error", op, err)
		}
	}
	var unsafe *UnsafePathError
	if err := (Permute{Path: Path{Key("__proto__")}, Permutation: []int{0}}).Validate(); !errors.As(err, &unsafe) {
		t.Errorf("reserved Permute path: got %v, want *UnsafePathError", err)
	}
	if err := (WirePermute{Ref: Path{Key("constructor")}}).Validate(); !errors.As(err, &unsafe) {
		t.Errorf("reserved WirePermute path: got %v, want *UnsafePathError", err)
	}
}
