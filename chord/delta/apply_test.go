package delta

import (
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// ops parses a JSON array of decoded tuples through ParseOp — the batch a
// consumer holds after decode — so a test reads the way its upstream
// counterpart does.
func ops(t *testing.T, literal string) []Op {
	t.Helper()
	raw, ok := tree(t, literal).([]any)
	if !ok {
		t.Fatalf("bad ops literal %s: not an array", literal)
	}
	out := make([]Op, 0, len(raw))
	for _, v := range raw {
		op, err := ParseOp(v)
		if err != nil {
			t.Fatalf("bad ops literal %s: %v", literal, err)
		}
		out = append(out, op)
	}
	return out
}

// applied runs Apply on a JSON literal target and fails on error.
func applied(t *testing.T, target string, batch []Op) any {
	t.Helper()
	got, err := Apply(tree(t, target), batch)
	if err != nil {
		t.Fatalf("Apply(%s): %v", target, err)
	}
	return got
}

func wantTree(t *testing.T, got any, literal string) {
	t.Helper()
	if want := tree(t, literal); !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// pad is upstream's PAD: see "adaptive emission" — a delta must be able to win.
var pad = strings.Repeat("p", 400)

// delta.test.ts "tracker: intent" → the apply halves of "records an append,
// not a replacement" and "recovers truncate+append from a rolling window".
// The ops are the ones upstream's tracker published at 64eeb82a4; the
// immutable tracker's diff publishes the same (testdata "canonical strings").
func TestApplyAppendAndRollingWindow(t *testing.T) {
	got := applied(t, `{"s": "", "pad": "`+pad+`"}`, ops(t, `[["a", ["s"], "abcd"]]`))
	wantTree(t, got, `{"s": "abcd", "pad": "`+pad+`"}`)

	got = applied(t, `{"s": "abcdefgh", "pad": "`+pad+`"}`, ops(t, `[["t", ["s"], 3], ["a", ["s"], "xyz"]]`))
	wantTree(t, got, `{"s": "defghxyz", "pad": "`+pad+`"}`)
}

// delta.test.ts "tracker: root ops" → the apply halves of "splices a value
// that is itself an array" and "normalises length = 0 on the root". A root
// splice re-headers the slice; Apply returns the value for the same reason it
// does for "r".
func TestApplyRootOps(t *testing.T) {
	got := applied(t, `[1, 2, 3, "`+pad+`"]`, ops(t, `[["p", [], 4, 0, [4]]]`))
	wantTree(t, got, `[1, 2, 3, "`+pad+`", 4]`)

	got = applied(t, `[1, 2, 3]`, ops(t, `[["r", []]]`))
	wantTree(t, got, `[]`)
}

// delta.test.ts "immutable operation application" → "does not mutate a
// replacement payload targeted by a later operation".
func TestApplyImmutableDoesNotMutateReplacementPayload(t *testing.T) {
	replacement := map[string]any{"nested": map[string]any{"value": float64(1)}}
	next, err := ApplyImmutable[map[string]any](nil, []Op{
		Replace{Value: replacement},
		Set{Path: Path{Key("nested"), Key("value")}, Value: float64(2)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := replacement["nested"].(map[string]any)["value"]; got != float64(1) {
		t.Errorf("replacement.nested.value = %v, want 1 (payload mutated)", got)
	}
	if got := next["nested"].(map[string]any)["value"]; got != float64(2) {
		t.Errorf("next.nested.value = %v, want 2", got)
	}
}

// README: applyImmutable copies only containers along changed paths and
// shares unchanged subtrees; it does not mutate either complete input.
func TestApplyImmutableSharesUnchangedSubtrees(t *testing.T) {
	sibling := map[string]any{"keep": true}
	xs := []any{float64(1), float64(2)}
	before := map[string]any{"a": map[string]any{"xs": xs}, "sibling": sibling}

	after, err := ApplyImmutable(before, ops(t, `[["p", ["a", "xs"], 2, 0, [3]], ["s", ["a", "flag"], true]]`))
	if err != nil {
		t.Fatal(err)
	}
	wantTree(t, after, `{"a": {"xs": [1, 2, 3], "flag": true}, "sibling": {"keep": true}}`)
	wantTree(t, before, `{"a": {"xs": [1, 2]}, "sibling": {"keep": true}}`)
	if len(xs) != 2 {
		t.Errorf("previous slice re-headered: %v", xs)
	}
	if reflect.ValueOf(after["sibling"]).Pointer() != reflect.ValueOf(sibling).Pointer() {
		t.Error("unchanged sibling was copied, not shared")
	}
	if reflect.ValueOf(after["a"]).Pointer() == reflect.ValueOf(before["a"]).Pointer() {
		t.Error("container on the changed path was shared, not copied")
	}
}

// A set into an existing element and a delete both write INTO the leaf
// container rather than re-headering it, so only a real copy keeps the previous
// value intact. Pinned against pi: applyImmutable({xs:[1,2]}, [["s",["xs",0],9]])
// leaves before as {xs:[1,2]} and returns {xs:[9,2]}.
func TestApplyImmutableLeavesPreviousContainersUntouched(t *testing.T) {
	xs := []any{float64(1), float64(2)}
	before := map[string]any{"xs": xs}
	after, err := ApplyImmutable(before, ops(t, `[["s", ["xs", 0], 9]]`))
	if err != nil {
		t.Fatal(err)
	}
	wantTree(t, after, `{"xs": [9, 2]}`)
	wantTree(t, before, `{"xs": [1, 2]}`)
	if xs[0] != float64(1) {
		t.Errorf("previous slice written through: %v", xs)
	}

	xs = []any{float64(1), float64(2), float64(3)}
	before = map[string]any{"xs": xs}
	after, err = ApplyImmutable(before, ops(t, `[["d", ["xs", 1]]]`))
	if err != nil {
		t.Fatal(err)
	}
	wantTree(t, after, `{"xs": [1, 3]}`)
	wantTree(t, before, `{"xs": [1, 2, 3]}`)
	if xs[1] != float64(2) {
		t.Errorf("previous slice written through by delete: %v", xs)
	}

	// The same for an object leaf under an array.
	inner := map[string]any{"a": float64(1)}
	before = map[string]any{"xs": []any{inner}}
	after, err = ApplyImmutable(before, ops(t, `[["s", ["xs", 0, "a"], 2]]`))
	if err != nil {
		t.Fatal(err)
	}
	wantTree(t, after, `{"xs": [{"a": 2}]}`)
	wantTree(t, before, `{"xs": [{"a": 1}]}`)
}

// delta.test.ts "apply and fan-out" → "adopts an `r` payload rather than
// copying it". The consumer owns the batch it was handed; copying every base
// batch defensively doubles the memory of the one op carrying the whole value.
// The rule that follows: do not hand one batch to two in-process consumers.
// This test pins the aliasing so nobody "fixes" it by adding a clone back.
func TestApplyAdoptsReplacePayload(t *testing.T) {
	batch := []Op{Replace{Value: map[string]any{"n": float64(0)}}}
	a, err := Apply[map[string]any](nil, batch)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Apply[map[string]any](nil, batch)
	if err != nil {
		t.Fatal(err)
	}
	if a == nil || b == nil {
		t.Fatalf("r payload not adopted: a = %v, b = %v", a, b)
	}
	a["n"] = float64(1)
	if b["n"] != float64(1) {
		t.Errorf("b.n = %v, want 1 (r payload was copied)", b["n"])
	}
}

// delta.test.ts "apply and fan-out" → "folds a whole stream without a
// base-batch branch". Apply handles "r" by replacing and tolerates a zero
// target, so a consumer needs no IsBase check and no clone of its own. The
// batches are the ones upstream's tracker published at 64eeb82a4 for the
// same writes.
func TestApplyFoldsStreamWithoutBaseBranch(t *testing.T) {
	var replica any
	var err error
	send := func(literal string) {
		t.Helper()
		if replica, err = Apply(replica, ops(t, literal)); err != nil {
			t.Fatal(err)
		}
	}
	send(`[["r", {"x": 100, "l": ["xyz"]}]]`)
	send(`[["s", ["x"], 101]]`)
	wantTree(t, replica, `{"x": 101, "l": ["xyz"]}`)

	// The same with a typed replica: the zero map is upstream's undefined.
	var typed map[string]any
	typed, err = Apply(typed, ops(t, `[["r", {"x": 100, "l": ["xyz"]}]]`))
	if err != nil {
		t.Fatal(err)
	}
	typed, err = Apply(typed, ops(t, `[["s", ["x"], 101], ["p", ["l"], 1, 0, ["abc"]]]`))
	if err != nil {
		t.Fatal(err)
	}
	wantTree(t, typed, `{"x": 101, "l": ["xyz", "abc"]}`)
}

// Go only: a typed replica gets its own type back, and a stream whose base
// carries a different shape is an error rather than a panic.
func TestApplyTypedReplica(t *testing.T) {
	_, err := Apply(map[string]any{}, []Op{Replace{Value: []any{float64(1)}}})
	if err == nil {
		t.Fatal("array base into a map replica: want error, got nil")
	}
	if !strings.Contains(err.Error(), "[]interface {}") || !strings.Contains(err.Error(), "map[string]interface {}") {
		t.Errorf("error should name both types, got %q", err)
	}

	// A write into a zero replica is upstream's apply(undefined, [s]): an
	// unresolvable root, not a nil-map panic.
	var pe *PathError
	if _, err := Apply[map[string]any](nil, ops(t, `[["s", ["a"], 1]]`)); !errors.As(err, &pe) {
		t.Errorf("set into zero replica: got %v, want *PathError", err)
	} else if pe.Ref.(Path).String() != "[]" {
		t.Errorf("PathError ref = %s, want [] (the root)", pe.Ref.(Path))
	}
	if _, err := Apply[any](nil, ops(t, `[["d", ["a"]]]`)); !errors.As(err, &pe) {
		t.Errorf("delete in nil replica: got %v, want *PathError", err)
	}
}

// delta.test.ts "flush" → "rejects a constructor walk". ({}).constructor
// .constructor is Function — the classic escape ladder. ParseOp refuses the
// tuple (op_test.go); a typed op that carries the path anyway is refused by
// Apply itself, before anything is walked, and the target is untouched.
func TestApplyRejectsConstructorWalk(t *testing.T) {
	target := map[string]any{}
	_, err := Apply(target, []Op{Set{Path: Path{Key("constructor"), Key("prototype"), Key("gadget")}, Value: true}})
	var unsafe *UnsafePathError
	if !errors.As(err, &unsafe) {
		t.Fatalf("got %v, want *UnsafePathError", err)
	}
	if len(target) != 0 {
		t.Errorf("target written despite the refusal: %v", target)
	}
	_, err = ApplyImmutable(target, []Op{Delete{Path: Path{Key("__proto__")}}})
	if !errors.As(err, &unsafe) {
		t.Fatalf("immutable: got %v, want *UnsafePathError", err)
	}
}

// delta.test.ts (upstream 9a139c62b) "validates immutable operations before
// traversing the target": applyImmutable now runs assertValidOp before it
// copies a single container, as Apply always did. pi at 9a139c62b, under node:
// applyImmutable({}, [["s", ["missing", "__proto__"], 1]]) throws
// UnsafePathError for "__proto__" — not the PathError that walking "missing"
// would raise first — and [["s", ["__proto__", "w"], 1]] throws it too.
func TestApplyImmutableValidatesBeforeWalking(t *testing.T) {
	for _, path := range []Path{{Key("missing"), Key("__proto__")}, {Key("__proto__"), Key("w")}} {
		target := map[string]any{}
		_, err := ApplyImmutable(target, []Op{Set{Path: path, Value: 1.0}})
		var unsafe *UnsafePathError
		if !errors.As(err, &unsafe) || unsafe.Segment != Key("__proto__") {
			t.Errorf("ApplyImmutable(%s): got %v, want *UnsafePathError for __proto__", path, err)
		}
		if len(target) != 0 {
			t.Errorf("target written: %v", target)
		}
	}
}

// A string segment that reaches an array on the way down. apply's resolve
// applies the array rule first — always *UnsafePathError — but
// applyImmutable's copyContainers asks Object.hasOwn first: a key the array
// does not own ("5" past the end, "01", "foo", "-1", "4294967295") is an
// unresolvable path, and only one it does own ("0", "length") meets the
// array rule. The last segment of an s/d/a/t path is the leaf, not the walk.
// Every row is pi's at 9a139c62b, under node, on {"xs":[{"a":1}]}.
func TestApplyStringSegmentThroughAnArray(t *testing.T) {
	cases := []struct {
		op        string
		immutable string // the PathError's ref, or "" for *UnsafePathError
	}{
		{`["s", ["xs", "5", "a"], 2]`, `["xs","5"]`},
		{`["s", ["xs", "1", "a"], 2]`, `["xs","1"]`},
		{`["s", ["xs", "01", "a"], 2]`, `["xs","01"]`},
		{`["s", ["xs", "foo", "a"], 2]`, `["xs","foo"]`},
		{`["s", ["xs", "-1", "a"], 2]`, `["xs","-1"]`},
		{`["s", ["xs", "4294967295", "a"], 2]`, `["xs","4294967295"]`},
		{`["s", ["xs", "5", "a", "b"], 2]`, `["xs","5","a"]`},
		{`["p", ["xs", "5"], 0, 0, []]`, `["xs","5"]`},
		{`["m", ["xs", "foo"], []]`, `["xs","foo"]`},
		{`["s", ["xs", "0", "a"], 2]`, ""},
		{`["s", ["xs", "length", "a"], 2]`, ""},
		{`["p", ["xs", "0"], 0, 0, []]`, ""},
		{`["m", ["xs", "length"], []]`, ""},
	}
	for _, tc := range cases {
		var unsafe *UnsafePathError
		var pe *PathError
		if _, err := Apply(tree(t, `{"xs": [{"a": 1}]}`), ops(t, "["+tc.op+"]")); !errors.As(err, &unsafe) {
			t.Errorf("Apply %s: %v, want *UnsafePathError", tc.op, err)
		}
		_, err := ApplyImmutable(tree(t, `{"xs": [{"a": 1}]}`), ops(t, "["+tc.op+"]"))
		switch {
		case tc.immutable == "":
			if !errors.As(err, &unsafe) {
				t.Errorf("ApplyImmutable %s: %v, want *UnsafePathError", tc.op, err)
			}
		case !errors.As(err, &pe):
			t.Errorf("ApplyImmutable %s: %v, want *PathError %s", tc.op, err, tc.immutable)
		case refString(pe.Ref) != tc.immutable:
			t.Errorf("ApplyImmutable %s: PathError ref %s, want %s", tc.op, refString(pe.Ref), tc.immutable)
		}
	}
}

// delta.test.ts "flush" → "allows a reserved name as a VALUE key". Reserved
// as segments, not as values: a value is written whole and never walked.
func TestApplyAllowsReservedNameAsValueKey(t *testing.T) {
	got := applied(t, `{}`, ops(t, `[["s", ["a"], {"__proto__": {"z": 1}}]]`))
	wantTree(t, got, `{"a": {"__proto__": {"z": 1}}}`)

	// "clones and reads reserved value keys": the key is an own key of the
	// replica's value.
	got = applied(t, `null`, ops(t, `[["r", {"value": {"__proto__": {"z": 1}}}]]`))
	root, _ := got.(map[string]any)
	value, _ := root["value"].(map[string]any)
	if _, ok := value["__proto__"]; !ok {
		t.Errorf("__proto__ is not an own key of the value: %#v", got)
	}
}

// delta.test.ts "safety: array indices". An index may address an existing
// element or append exactly one past the end. Not an arbitrary cap: a sparse
// array does not survive a JSON round trip, so arr[7] = x on a length-3 array
// already produces unreplicable state.
func TestApplyArrayIndexSafety(t *testing.T) {
	var unsafe *UnsafePathError
	var pe *PathError

	// "writes an existing index"
	wantTree(t, applied(t, `{"xs": [1, 2, 3]}`, ops(t, `[["s", ["xs", 1], 9]]`)), `{"xs": [1, 9, 3]}`)
	// "appends one past the end"
	wantTree(t, applied(t, `{"xs": [1, 2, 3]}`, ops(t, `[["s", ["xs", 3], 9]]`)), `{"xs": [1, 2, 3, 9]}`)
	// "rejects a gap"
	if _, err := Apply(tree(t, `{"xs": [1, 2, 3]}`), ops(t, `[["s", ["xs", 5], 9]]`)); !errors.As(err, &unsafe) {
		t.Errorf("gap: got %v, want *UnsafePathError", err)
	} else if unsafe.Segment != Index(5) {
		t.Errorf("gap: Segment = %#v, want Index(5)", unsafe.Segment)
	}
	// ...including a gap of exactly one, the tight edge of the append window.
	if _, err := Apply(tree(t, `{"xs": [1, 2, 3]}`), ops(t, `[["s", ["xs", 4], 9]]`)); !errors.As(err, &unsafe) {
		t.Errorf("gap of one: got %v, want *UnsafePathError", err)
	}
	// "rejects a huge index": would otherwise allocate 4.29 billion entries from one op.
	if _, err := Apply(tree(t, `{"xs": []}`), ops(t, `[["s", ["xs", 4294967290], 1]]`)); !errors.As(err, &unsafe) {
		t.Errorf("huge index: got %v, want *UnsafePathError", err)
	}
	// "rejects string-spelled array indices at the consumer"
	if _, err := Apply(tree(t, `{"xs": [1, 2, 3]}`), ops(t, `[["s", ["xs", "7"], 9]]`)); !errors.As(err, &unsafe) {
		t.Errorf(`s ["xs","7"]: got %v, want *UnsafePathError`, err)
	} else if unsafe.Segment != Key("7") {
		t.Errorf(`s ["xs","7"]: Segment = %#v, want Key("7")`, unsafe.Segment)
	}
	if _, err := Apply(tree(t, `{"xs": ["a"]}`), ops(t, `[["a", ["xs", "0"], "b"]]`)); !errors.As(err, &unsafe) {
		t.Errorf(`a ["xs","0"]: got %v, want *UnsafePathError`, err)
	}
	// A string-spelled index on the way down, not just at the leaf.
	if _, err := Apply(tree(t, `{"xs": [{"a": 1}]}`), ops(t, `[["s", ["xs", "0", "a"], 2]]`)); !errors.As(err, &unsafe) {
		t.Errorf(`walk through ["xs","0"]: got %v, want *UnsafePathError`, err)
	}
	// Descending THROUGH an index equal to the length is unresolvable, not a
	// panic: pi's apply({xs:[1]}, [["s",["xs",1,"a"],2]]) → PathError ["xs",1].
	if _, err := Apply(tree(t, `{"xs": [1]}`), ops(t, `[["s", ["xs", 1, "a"], 2]]`)); !errors.As(err, &pe) {
		t.Errorf(`walk through ["xs",1] at the end: got %v, want *PathError`, err)
	} else if got := refString(pe.Ref); got != `["xs",1]` {
		t.Errorf(`walk through ["xs",1] at the end: ref = %s, want ["xs",1]`, got)
	}
	// "allows explicit growth with nulls"
	wantTree(t, applied(t, `{"xs": [1]}`, ops(t, `[["p", ["xs"], 1, 0, [null, null, 9]]]`)), `{"xs": [1, null, null, 9]}`)
	// "rejects deleting one past an array's end"
	if _, err := Apply(tree(t, `{"xs": [1]}`), ops(t, `[["d", ["xs", 1]]]`)); !errors.As(err, &pe) {
		t.Errorf("delete one past end: got %v, want *PathError", err)
	}
	// ...and a delete further out is the index rule, not a resolution failure.
	if _, err := Apply(tree(t, `{"xs": [1]}`), ops(t, `[["d", ["xs", 5]]]`)); !errors.As(err, &unsafe) {
		t.Errorf("delete past a gap: got %v, want *UnsafePathError", err)
	}
	// "applies large splice payloads without spreading them at once"
	items := make([]any, 300_000)
	got, err := Apply(map[string]any{"xs": []any{}}, []Op{Splice{Path: Path{Key("xs")}, Items: items}})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(got["xs"].([]any)); n != len(items) {
		t.Errorf("large splice: len = %d, want %d", n, len(items))
	}
}

// delta.test.ts "safety: op structure". Shape is the type's job in Go — an
// unknown verb, a string path or non-array items cannot be spelled as an Op
// and are ParseOp's to refuse (op_test.go) — so what is left for the applier
// is the nil element, the constraints Validate carries, and the value checks.
func TestApplyOpStructureSafety(t *testing.T) {
	var pe *PathError

	// "rejects a non-tuple op": the one shape an []Op can still hold.
	if _, err := Apply(tree(t, `{"a": 1}`), []Op{nil}); !errors.Is(err, ErrInvalidOp) {
		t.Errorf("nil op: got %v, want ErrInvalidOp", err)
	}
	// "rejects append to a missing or non-string value"
	if _, err := Apply(tree(t, `{"a": 1}`), ops(t, `[["a", ["missing"], "x"]]`)); !errors.As(err, &pe) {
		t.Errorf("append to missing: got %v, want *PathError", err)
	}
	if _, err := Apply(tree(t, `{"a": 1}`), ops(t, `[["a", ["a"], "x"]]`)); !errors.As(err, &pe) {
		t.Errorf("append to number: got %v, want *PathError", err)
	}
	// Appending one past an array's end reads undefined, which is not a string.
	if _, err := Apply(tree(t, `{"xs": ["a"]}`), ops(t, `[["a", ["xs", 1], "x"]]`)); !errors.As(err, &pe) {
		t.Errorf("append one past end: got %v, want *PathError", err)
	}
	// "rejects negative truncation": unspellable through ParseOp, so typed.
	if _, err := Apply(tree(t, `{"a": "abc"}`), []Op{Truncate{Path: Path{Key("a")}, Count: -1}}); !errors.Is(err, ErrInvalidOp) {
		t.Errorf("negative truncation: got %v, want ErrInvalidOp", err)
	}
	if _, err := Apply(tree(t, `{"a": "abc"}`), []Op{Set{Value: 1}}); !errors.Is(err, ErrInvalidOp) {
		t.Errorf("root set: got %v, want ErrInvalidOp", err)
	}
	// "clamps a splice remove past the end, as Array.prototype.splice does":
	// deterministic and identical on both sides — not a hole.
	wantTree(t, applied(t, `{"xs": [1, 2]}`, ops(t, `[["p", ["xs"], 0, 1000000000, []]]`)), `{"xs": []}`)
	// splice clamps the index the same way.
	wantTree(t, applied(t, `{"xs": [1, 2]}`, ops(t, `[["p", ["xs"], 5, 0, [9]]]`)), `{"xs": [1, 2, 9]}`)
}

// The path a PathError carries is the one upstream's resolve was handed: the
// parent path for s/d/a/t resolution failures, the op's full path when the
// value at it is the wrong kind. Pinned against pi's messages.
func TestApplyPathErrorsCarryTheResolvedPath(t *testing.T) {
	cases := []struct {
		name, target, batch, wantRef string
	}{
		{"set under missing parent", `{"a": 1}`, `[["s", ["b", "c"], 1]]`, `["b"]`},
		{"set under scalar parent", `{"a": 1}`, `[["s", ["a", "c"], 1]]`, `["a"]`},
		{"splice a non-array", `{"a": 1}`, `[["p", ["a"], 0, 0, []]]`, `["a"]`},
		{"append to a non-string", `{"a": 1}`, `[["a", ["a"], "x"]]`, `["a"]`},
		{"truncate a non-string", `{"a": 1}`, `[["t", ["a"], 1]]`, `["a"]`},
		{"append one past an array's end", `{"xs": ["a"]}`, `[["a", ["xs", 1], "x"]]`, `["xs",1]`},
		{"delete one past an array's end", `{"xs": [1]}`, `[["d", ["xs", 1]]]`, `["xs",1]`},
		{"splice the root when it is not an array", `"str"`, `[["p", [], 0, 0, []]]`, `[]`},
		{"set into a scalar root", `5`, `[["s", ["a"], 1]]`, `[]`},
	}
	for _, tc := range cases {
		for _, fn := range []struct {
			name string
			f    func(any, []Op) (any, error)
		}{{"Apply", Apply[any]}, {"ApplyImmutable", ApplyImmutable[any]}} {
			_, err := fn.f(tree(t, tc.target), ops(t, tc.batch))
			var pe *PathError
			if !errors.As(err, &pe) {
				t.Errorf("%s/%s: got %v, want *PathError", fn.name, tc.name, err)
				continue
			}
			if got := refString(pe.Ref); got != tc.wantRef {
				t.Errorf("%s/%s: PathError ref = %s, want %s", fn.name, tc.name, got, tc.wantRef)
			}
		}
	}
}

// Object keys are strings on both sides: an Index segment under an object is
// the property spelled as a number, the way JavaScript coerces it. Pinned
// against pi.
func TestApplyNumericSegmentOnObject(t *testing.T) {
	wantTree(t, applied(t, `{"o": {}}`, ops(t, `[["s", ["o", 3], 1]]`)), `{"o": {"3": 1}}`)
	wantTree(t, applied(t, `{"o": {"3": 1, "x": 2}}`, ops(t, `[["d", ["o", 3]]]`)), `{"o": {"x": 2}}`)
	// Deleting a missing key is a no-op, as `delete` is.
	wantTree(t, applied(t, `{"o": {}}`, ops(t, `[["d", ["o", "nope"]]]`)), `{"o": {}}`)
}

// "t" counts UTF-16 code units, the wire's unit, not bytes or runes; a Go
// replica that counted bytes would drift from a pi replica on the first
// non-ASCII window. Values pinned against pi except the split surrogate: pi
// holds the lone low surrogate "\ude00ab", which is not a Go string; U+FFFD is
// what encoding/json makes of that value once it crosses the wire, so a Go
// replica holds the same thing a Go decoder would read from a pi replica.
func TestApplyTruncateCountsUTF16CodeUnits(t *testing.T) {
	cases := []struct {
		in    string
		count int
		want  string
	}{
		{"héllo", 2, "llo"},
		{"😀ab", 2, "ab"},
		{"😀ab", 1, "�ab"},
		{"abc", 100, ""},
		{"abc", 0, "abc"},
		{"日本語", 1, "本語"},
		{"a😀b😀", 3, "b😀"},
		// Cuts that land after multi-byte runes, where a byte count would
		// stop short.
		{"日本語", 2, "語"},
		{"ééx", 2, "x"},
		{"😀😀x", 3, "�x"},
		{"😀😀x", 4, "x"},
		{"é😀é", 2, "�é"},
		{"é😀é", 3, "é"},
	}
	for _, tc := range cases {
		got, err := Apply(map[string]any{"a": tc.in}, []Op{Truncate{Path: Path{Key("a")}, Count: tc.count}})
		if err != nil {
			t.Fatalf("t %q %d: %v", tc.in, tc.count, err)
		}
		if got["a"] != tc.want {
			t.Errorf("t %q %d = %q, want %q", tc.in, tc.count, got["a"], tc.want)
		}
	}
}

// Apply is not transactional: ops before the failing one have already changed
// the replica (README, "Tracker lifecycle"). The error terminates the stream.
func TestApplyIsNotTransactional(t *testing.T) {
	target := map[string]any{"a": float64(1)}
	_, err := Apply(target, ops(t, `[["s", ["a"], 2], ["s", ["missing", "x"], 1]]`))
	if err == nil {
		t.Fatal("want error")
	}
	if target["a"] != float64(2) {
		t.Errorf("first op rolled back: a = %v, want 2", target["a"])
	}
}

// The "m" op (upstream 10d1ad621): new[i] = old[permutation[i]], at the root
// or under a path, and a PathError naming the op's path when the target is
// not an array of exactly the permutation's length. Every outcome is what pi's
// apply returned for the same batch under node at 10d1ad621.
func TestApplyPermute(t *testing.T) {
	wantTree(t, applied(t, `[10, 20, 30]`, ops(t, `[["m", [], [2, 0, 1]]]`)), `[30, 10, 20]`)
	wantTree(t, applied(t, `{"a": {"values": ["x", "y", "z"]}}`, ops(t, `[["m", ["a", "values"], [1, 2, 0]]]`)), `{"a": {"values": ["y", "z", "x"]}}`)
	// Two permutations compose: [2,0,1] then [1,2,0] is the identity. A
	// reorder that read from the half-written array would not be.
	wantTree(t, applied(t, `{"values": ["a", "b", "c"]}`, ops(t, `[["m", ["values"], [2, 0, 1]], ["m", ["values"], [1, 2, 0]]]`)), `{"values": ["a", "b", "c"]}`)
	wantTree(t, applied(t, `{"values": []}`, ops(t, `[["m", ["values"], []]]`)), `{"values": []}`)

	for _, tc := range []struct{ target, batch string }{
		{`{"values": [1, 2, 3]}`, `[["m", ["values"], [1, 0]]]`},
		{`{"values": [1]}`, `[["m", ["values"], [1, 0]]]`},
		{`{"values": "abc"}`, `[["m", ["values"], [0, 1, 2]]]`},
		{`{}`, `[["m", ["values"], [0]]]`},
	} {
		_, err := Apply(tree(t, tc.target), ops(t, tc.batch))
		var pe *PathError
		if !errors.As(err, &pe) || refString(pe.Ref) != `["values"]` {
			t.Errorf("Apply(%s, %s): got %v, want *PathError [\"values\"]", tc.target, tc.batch, err)
		}
	}
	// A typed op is validated before it is applied, as a parsed one is.
	if _, err := Apply(any([]any{1.0, 2.0}), []Op{Permute{Permutation: []int{1, 1}}}); !errors.Is(err, ErrInvalidOp) {
		t.Errorf("duplicate index applied: got %v, want ErrInvalidOp", err)
	}
}

// applyImmutable copies the containers along an "m" op's FULL path — the
// permuted array included, as for "p" — so the previous value is untouched
// while unchanged subtrees and the moved elements themselves stay shared.
// pi at 10d1ad621: beforeUnchanged, keepShared, itemShared and arrayCopied
// all true.
func TestApplyImmutablePermute(t *testing.T) {
	a, b, c := map[string]any{"id": "a"}, map[string]any{"id": "b"}, map[string]any{"id": "c"}
	values := []any{a, b, c}
	keep := map[string]any{"k": 1.0}
	before := map[string]any{"keep": keep, "a": map[string]any{"values": values}}
	after, err := ApplyImmutable(before, ops(t, `[["m", ["a", "values"], [2, 0, 1]]]`))
	if err != nil {
		t.Fatal(err)
	}
	wantTree(t, after, `{"keep": {"k": 1}, "a": {"values": [{"id": "c"}, {"id": "a"}, {"id": "b"}]}}`)
	wantTree(t, before, `{"keep": {"k": 1}, "a": {"values": [{"id": "a"}, {"id": "b"}, {"id": "c"}]}}`)
	if values[0].(map[string]any)["id"] != "a" {
		t.Errorf("previous array permuted in place: %v", values)
	}
	got := after
	if !same(got["keep"], keep) {
		t.Error("unchanged subtree copied")
	}
	moved := got["a"].(map[string]any)["values"].([]any)
	if !same(moved[1], a) {
		t.Error("moved element copied rather than shared")
	}

	root := []any{1.0, 2.0, 3.0}
	rootAfter, err := ApplyImmutable(root, ops(t, `[["m", [], [2, 1, 0]]]`))
	if err != nil {
		t.Fatal(err)
	}
	wantTree(t, rootAfter, `[3, 2, 1]`)
	wantTree(t, root, `[1, 2, 3]`)
}

// ─── delta-apply-immutable.test.ts (9e70c3d50) ───────────────────────────────
//
// Upstream freezes its inputs to prove nothing writes them; Go cannot freeze,
// so each case compares the inputs with a snapshot taken before.

func TestApplyImmutableCopiesEachTouchedContainerOnce(t *testing.T) {
	shared := tree(t, `{"nested": {"value": 1}}`)
	untouched := tree(t, `{"value": 9}`)
	rowPayload := tree(t, `{"id": 4, "label": "placed"}`)
	base := tree(t, `{"text": "abcdef", "stable": {"value": 7}, "branch": {"value": 1}, "copy": null, "placed": null,
		"untouched": null, "left": null, "right": null, "meta": {"count": 0, "obsolete": true},
		"rows": [{"id": 1, "label": "one"}, {"id": 2, "label": "two"}, {"id": 3, "label": "three"}]}`).(map[string]any)
	snapshot := func() string { return jsonText(t, []any{base, shared, untouched, rowPayload}) }
	before := snapshot()
	ops := []Op{
		Truncate{Path: Path{Key("text")}, Count: 2},
		Append{Path: Path{Key("text")}, Text: "!"},
		Set{Path: Path{Key("meta"), Key("count")}, Value: 1.0},
		Set{Path: Path{Key("meta"), Key("count")}, Value: 2.0},
		Delete{Path: Path{Key("meta"), Key("obsolete")}},
		Set{Path: Path{Key("copy")}, Value: base["branch"]},
		Set{Path: Path{Key("copy"), Key("value")}, Value: 2.0},
		Set{Path: Path{Key("placed")}, Value: shared},
		Set{Path: Path{Key("placed"), Key("nested"), Key("value")}, Value: 2.0},
		Set{Path: Path{Key("untouched")}, Value: untouched},
		Set{Path: Path{Key("left")}, Value: shared},
		Set{Path: Path{Key("right")}, Value: shared},
		Set{Path: Path{Key("left"), Key("nested"), Key("value")}, Value: 3.0},
		Splice{Path: Path{Key("rows")}, Index: 1, Remove: 1, Items: []any{rowPayload}},
		Set{Path: Path{Key("rows"), Index(1), Key("label")}, Value: "edited"},
		Permute{Path: Path{Key("rows")}, Permutation: []int{1, 0, 2}},
		Set{Path: Path{Key("rows"), Index(0), Key("label")}, Value: "moved"},
	}
	result, err := ApplyImmutable(base, ops)
	must(t, err)
	mutable, err := Apply(cloneJSON(base).(map[string]any), cloneOps(ops))
	must(t, err)
	wantJSON(t, result, mutable)
	if before != snapshot() {
		t.Error("ApplyImmutable wrote into its inputs or a payload")
	}
	switch {
	case result["text"] != "cdef!":
		t.Errorf("text %v", result["text"])
	case !same(result["stable"], base["stable"]), !same(result["untouched"], untouched):
		t.Error("an untouched container was copied")
	case same(result["copy"], base["branch"]), same(result["placed"], shared), same(result["left"], shared):
		t.Error("a payload that a later op walked into was written in place, not copied")
	case !same(result["right"], shared):
		t.Error("a payload no later op walked into was copied")
	case same(result["rows"].([]any)[0], rowPayload):
		t.Error("the spliced payload an op edited was not copied")
	}
	wantJSON(t, result["rows"].([]any)[0], tree(t, `{"id": 4, "label": "moved"}`))
}

func TestApplyImmutableBatchesProtectsReplacementPayloads(t *testing.T) {
	replacement := tree(t, `{"nested": {"value": 1}, "values": [1, 2, 3]}`)
	result, err := ApplyImmutableBatches[any](nil, slices.Values([][]Op{
		{Replace{Value: replacement}},
		{Set{Path: Path{Key("nested"), Key("value")}, Value: 2.0}},
		{Splice{Path: Path{Key("values")}, Index: 1, Remove: 1, Items: []any{4.0, 5.0}}, Permute{Path: Path{Key("values")}, Permutation: []int{3, 0, 1, 2}}},
	}))
	must(t, err)
	wantJSON(t, result, tree(t, `{"nested": {"value": 2}, "values": [3, 1, 4, 5]}`))
	wantJSON(t, replacement, tree(t, `{"nested": {"value": 1}, "values": [1, 2, 3]}`))

	array := tree(t, `[1, 2, 3]`)
	arrayResult, err := ApplyImmutable(array, []Op{
		Splice{Path: Path{}, Index: 1, Remove: 1, Items: []any{4.0, 5.0}},
		Permute{Path: Path{}, Permutation: []int{3, 0, 1, 2}},
		Delete{Path: Path{Index(1)}},
	})
	must(t, err)
	wantJSON(t, arrayResult, tree(t, `[3, 4, 5]`))
	wantJSON(t, array, tree(t, `[1, 2, 3]`))
}

func TestApplyImmutableBatchesSharesOneScope(t *testing.T) {
	base := tree(t, `{"text": "abcdef", "meta": {"count": 0}, "values": [{"id": 1, "value": 1}, {"id": 2, "value": 2}, {"id": 3, "value": 3}]}`)
	before := jsonText(t, base)
	batches := [][]Op{
		{Set{Path: Path{Key("meta"), Key("count")}, Value: 1.0}, Splice{Path: Path{Key("values")}, Index: 1, Remove: 1, Items: []any{tree(t, `{"id": 4, "value": 4}`)}}},
		{},
		{Permute{Path: Path{Key("values")}, Permutation: []int{2, 0, 1}}, Set{Path: Path{Key("values"), Index(2), Key("value")}, Value: 40.0}},
		{Truncate{Path: Path{Key("text")}, Count: 2}, Append{Path: Path{Key("text")}, Text: "!"}},
	}
	intermediate, err := ApplyImmutable(base, batches[0])
	must(t, err)
	intermediateSnapshot := jsonText(t, intermediate)
	sequential := intermediate
	for _, batch := range batches[1:] {
		sequential, err = ApplyImmutable(sequential, batch)
		must(t, err)
	}
	streamed, err := ApplyImmutableBatches(base, slices.Values(batches))
	must(t, err)
	flattened, err := ApplyImmutable(base, slices.Concat(batches...))
	must(t, err)
	wantJSON(t, streamed, sequential)
	wantJSON(t, streamed, flattened)
	if jsonText(t, intermediate) != intermediateSnapshot || jsonText(t, base) != before {
		t.Error("a later application wrote into an earlier revision or the base")
	}
}

func TestApplyImmutableBatchesReplaysTrackerBatches(t *testing.T) {
	values := make([]any, 8)
	for id := range values {
		values[id] = map[string]any{"id": float64(id), "score": 0.0}
	}
	initial := map[string]any{"text": "start", "values": values, "revision": 0.0}
	before := jsonText(t, initial)
	tr := Track(initial)
	var batches [][]Op
	for revision := 1; revision <= 40; revision++ {
		c := mustBegin(t, tr)
		state := c.State()
		must(t, state.Set("revision", float64(revision)))
		text, _ := state.Get("text")
		must(t, state.Set("text", text.(string)[1:]+strconv.Itoa(revision)))
		vs := state.At("values")
		var err error
		switch revision % 5 {
		case 0:
			err = vs.Reverse()
		case 1:
			_, err = vs.Push(map[string]any{"id": float64(100 + revision), "score": float64(revision)})
		case 2:
			_, _, err = vs.Shift()
		case 3:
			err = vs.At(revision%vs.Len()).Set("score", float64(revision))
		default:
			_, err = vs.Splice(1, 1, map[string]any{"id": float64(200 + revision), "score": float64(revision)})
		}
		must(t, err)
		p := mustPrepare(t, c)
		batches = append(batches, p.Ops())
		must(t, tr.Adopt(p))
	}
	replayed, err := ApplyImmutableBatches[any](initial, slices.Values(batches))
	must(t, err)
	wantJSON(t, replayed, tr.Value())
	if jsonText(t, initial) != before {
		t.Error("the replay wrote into the initial revision")
	}
}

func TestApplyImmutableBatchesStopsAtAnInvalidOp(t *testing.T) {
	base := tree(t, `{"nested": {"value": 1}}`)
	advancedPastInvalid := false
	batches := func(yield func([]Op) bool) {
		if !yield([]Op{Set{Path: Path{Key("nested"), Key("value")}, Value: 4.0}}) {
			return
		}
		if !yield([]Op{Set{Path: Path{Key("__proto__"), Key("polluted")}, Value: true}}) {
			return
		}
		advancedPastInvalid = true
	}
	var unsafe *UnsafePathError
	if _, err := ApplyImmutableBatches(base, batches); !errors.As(err, &unsafe) {
		t.Errorf("a reserved segment in a later batch: %v, want an UnsafePathError", err)
	}
	if advancedPastInvalid {
		t.Error("the replay read past the batch that failed")
	}
	wantJSON(t, base, tree(t, `{"nested": {"value": 1}}`))

	var pathErr *PathError
	if _, err := ApplyImmutable(tree(t, `{"values": []}`), []Op{Set{Path: Path{Key("values"), Key("missing"), Key("value")}, Value: 1.0}}); !errors.As(err, &pathErr) {
		t.Errorf("a key an array does not own: %v, want a PathError", err)
	}
	if _, err := ApplyImmutable(tree(t, `{"values": [{}]}`), []Op{Set{Path: Path{Key("values"), Key("0"), Key("value")}, Value: 1.0}}); !errors.As(err, &unsafe) {
		t.Errorf("a key an array owns: %v, want an UnsafePathError", err)
	}
}

func TestApplyImmutableFansOutWithoutSharingCopies(t *testing.T) {
	payload := tree(t, `{"nested": {"value": 1}}`)
	ops := []Op{Set{Path: Path{Key("placed")}, Value: payload}, Set{Path: Path{Key("placed"), Key("nested"), Key("value")}, Value: 2.0}}
	base := tree(t, `{"placed": null}`)
	first, err := ApplyImmutable(base, ops)
	must(t, err)
	second, err := ApplyImmutable(base, ops)
	must(t, err)
	wantJSON(t, first, second)
	if same(first, second) || same(first.(map[string]any)["placed"], second.(map[string]any)["placed"]) {
		t.Error("two applications of one batch share their copies")
	}
	wantJSON(t, payload, tree(t, `{"nested": {"value": 1}}`))
}

// "copies a wide object once rather than once per repeated write": counted
// here as allocations, where upstream counts Object.keys calls.
func TestApplyImmutableCopiesAWideObjectOnce(t *testing.T) {
	base := make(map[string]any, 20_000)
	for i := range 20_000 {
		base[fmt.Sprintf("field%d", i)] = float64(i)
	}
	ops := make([]Op, 1_000)
	for i := range ops {
		ops[i] = Set{Path: Path{Key(fmt.Sprintf("field%d", i))}, Value: float64(-i)}
	}
	var result map[string]any
	allocs := testing.AllocsPerRun(1, func() {
		var err error
		result, err = ApplyImmutable(base, ops)
		must(t, err)
	})
	if result["field999"] != -999.0 || base["field999"] != 999.0 {
		t.Fatalf("field999: result %v, base %v", result["field999"], base["field999"])
	}
	// One copy of a 20,000-member map is a few hundred allocations; one per
	// write would be a thousand copies of it.
	if allocs > 2_000 {
		t.Errorf("%v allocations for 1,000 writes to one object: it is copied per write, not once", allocs)
	}
	// ApplyImmutableBatches keeps the one copy across batches, too.
	batches := make([][]Op, len(ops))
	for i, op := range ops {
		batches[i] = []Op{op}
	}
	allocs = testing.AllocsPerRun(1, func() {
		var err error
		result, err = ApplyImmutableBatches(base, slices.Values(batches))
		must(t, err)
	})
	if result["field999"] != -999.0 || allocs > 2_000 {
		t.Errorf("%v allocations for 1,000 one-write batches to one object: it is copied per batch, not once", allocs)
	}
}

// delta-apply-immutable.test.ts (upstream 091bcc099) "appends to an array not
// yet copied in one copy, sharing everything else". Upstream asserts sharing
// and values; the one copy shows here as bytes, for one append: a copy that
// the insert then grows allocates the array about two and a quarter times.
// The batch's second append grows the copy, which the replay owns, as
// upstream's push grows the concat.
func TestApplyImmutableAppendsInOneCopy(t *testing.T) {
	const n = 100_000
	entries := make([]any, n)
	for i := range entries {
		entries[i] = map[string]any{"index": float64(i)}
	}
	meta := map[string]any{"count": float64(n)}
	log := map[string]any{"entries": entries, "meta": meta}
	other := map[string]any{"value": 1.0}
	base := map[string]any{"log": log, "other": other}
	appendOps := ops(t, `[["p", ["log", "entries"], 100000, 0, [{"index": 100000}]], ["p", ["log", "entries"], 100001, 0, [{"index": 100001}]]]`)

	bytes := allocatedBytes(func() {
		_, err := ApplyImmutable(base, appendOps[:1])
		must(t, err)
	})
	arrayBytes := uint64(n * 16) // an []any element is two words
	if bytes > arrayBytes*3/2 {
		t.Errorf("appending to a %d-entry array allocated %d bytes, %.2f times the array: it is copied, then grown into a second copy", n, bytes, float64(bytes)/float64(arrayBytes))
	}

	// A replay's later appends grow the copy it owns: not one copy each.
	batches := make([][]Op, 100)
	for i := range batches {
		batches[i] = []Op{Splice{Path: Path{Key("log"), Key("entries")}, Index: n + i, Items: []any{float64(i)}}}
	}
	bytes = allocatedBytes(func() {
		_, err := ApplyImmutableBatches(base, slices.Values(batches))
		must(t, err)
	})
	if bytes > arrayBytes*4 {
		t.Errorf("100 one-append batches allocated %.2f times the array: the replay copies its own array again", float64(bytes)/float64(arrayBytes))
	}

	result, err := ApplyImmutable(base, appendOps)
	must(t, err)

	got := result["log"].(map[string]any)["entries"].([]any)
	if len(got) != n+2 {
		t.Fatalf("result has %d entries, want %d", len(got), n+2)
	}
	wantJSON(t, got[n:], tree(t, `[{"index": 100000}, {"index": 100001}]`))
	if len(entries) != n || len(log["entries"].([]any)) != n {
		t.Errorf("base entries grew to %d", len(log["entries"].([]any)))
	}
	pointer := func(v any) uintptr { return reflect.ValueOf(v).Pointer() }
	if pointer(got[0]) != pointer(entries[0]) {
		t.Errorf("result entries[0] is a copy, want the base's own")
	}
	resultLog := result["log"].(map[string]any)
	if pointer(resultLog["meta"]) != pointer(meta) || pointer(result["other"]) != pointer(other) {
		t.Errorf("an untouched sibling was copied")
	}
	if pointer(resultLog) == pointer(log) {
		t.Errorf("result log is the base's: the parent of the appended array was written in place")
	}

	rootArray := []any{1.0, 2.0}
	appended, err := ApplyImmutable(rootArray, ops(t, `[["p", [], 2, 0, [3]]]`))
	must(t, err)
	wantJSON(t, appended, tree(t, `[1, 2, 3]`))
	wantJSON(t, rootArray, tree(t, `[1, 2]`))
	// An appended item that is itself an array stays one element.
	appended, err = ApplyImmutable(rootArray, ops(t, `[["p", [], 2, 0, [[3, 4]]]]`))
	must(t, err)
	wantJSON(t, appended, tree(t, `[1, 2, [3, 4]]`))

	// Invalid paths fail with the general path's errors. Upstream's
	// ["list", 1.5] has no Go form: an Index is an integer.
	document := tree(t, `{"values": [], "list": [[1]], "text": "a"}`)
	for _, c := range []struct {
		path   Path
		unsafe bool
	}{
		{Path{Key("values"), Key("missing")}, false},
		{Path{Key("missing")}, false},
		{Path{Key("values"), Index(0)}, false},
		{Path{Key("__proto__")}, true},
		{Path{Key("list"), Key("x")}, false},
		{Path{Key("text")}, false},
		{Path{Key("list"), Index(-1)}, true},
		{Path{Key("list"), Index(0), Key("constructor")}, true},
	} {
		_, err := ApplyImmutable(document, []Op{Splice{Path: c.path, Items: []any{}}})
		var pe *PathError
		var unsafe *UnsafePathError
		if c.unsafe && !errors.As(err, &unsafe) || !c.unsafe && !errors.As(err, &pe) {
			t.Errorf("splice at %s: got %v, want unsafe=%v", c.path, err, c.unsafe)
		}
	}
	wantJSON(t, document, tree(t, `{"values": [], "list": [[1]], "text": "a"}`))
	// An insert that is not at the end takes the general path.
	inserted, err := ApplyImmutable(rootArray, ops(t, `[["p", [], 1, 0, [9]]]`))
	must(t, err)
	wantJSON(t, inserted, tree(t, `[1, 9, 2]`))
}

// A replay keeps every copy it made alive until it returns. Were a copy freed
// once a later op dropped it, a payload a lazy batch allocates afterwards could
// take its address and read as the replay's own — and be written in place.
func TestApplyImmutableBatchesNeverWritesALazyPayload(t *testing.T) {
	base := map[string]any{"a": map[string]any{"x": 0.0}}
	var payloads []map[string]any
	batches := func(yield func([]Op) bool) {
		for round := range 3_000 {
			p := map[string]any{"x": 0.0}
			payloads = append(payloads, p)
			if !yield([]Op{Set{Path: Path{Key("a")}, Value: p}}) {
				return
			}
			// Copies p — a payload, not the replay's own — and drops the copy the
			// previous round's write made.
			if !yield([]Op{Set{Path: Path{Key("a"), Key("x")}, Value: float64(round + 1)}}) {
				return
			}
			if round%4 == 0 {
				runtime.GC()
			}
		}
	}
	result, err := ApplyImmutableBatches(base, batches)
	must(t, err)
	written := 0
	for _, p := range payloads {
		if p["x"] != 0.0 {
			written++
		}
	}
	if written > 0 {
		t.Errorf("%d of %d payloads were written in place", written, len(payloads))
	}
	wantJSON(t, result, tree(t, `{"a": {"x": 3000}}`))
	wantJSON(t, base, tree(t, `{"a": {"x": 0}}`))
}

// A nil map is JSON null, and an immutable application refuses to write into
// one, as upstream's copyContainers refuses a value that is not an object (and
// as Apply does): a map-typed replica that missed its base batch fails rather
// than absorbing the ops.
func TestApplyImmutableRefusesANilMap(t *testing.T) {
	var pathErr *PathError
	if _, err := ApplyImmutable[map[string]any](nil, []Op{Set{Path: Path{Key("a")}, Value: 1.0}}); !errors.As(err, &pathErr) {
		t.Errorf("a nil root: %v, want a PathError", err)
	}
	if _, err := ApplyImmutable(map[string]any{"a": map[string]any(nil)}, []Op{Set{Path: Path{Key("a"), Key("k")}, Value: 1.0}}); !errors.As(err, &pathErr) {
		t.Errorf("a nil member: %v, want a PathError", err)
	}
	if _, err := ApplyImmutableBatches[map[string]any](nil, slices.Values([][]Op{{Set{Path: Path{Key("a")}, Value: 1.0}}})); !errors.As(err, &pathErr) {
		t.Errorf("a nil root, batched: %v, want a PathError", err)
	}
}
