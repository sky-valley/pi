package delta

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"weak"

	"github.com/sky-valley/pi/chord/internal/jsonvalue"
)

// The upstream assertions ops goldens cannot carry: object identity (what is
// shared between revisions and what is copied), aliasing of the caller's
// values, the lifecycle errors as Go sentinels, and what a settled change
// retains. testdata/upstream_delta.json pins the ops, values and error text
// of the same cases against pi.

func mustBegin[T any](t *testing.T, tr *Tracker[T]) *Change[T] {
	t.Helper()
	c, err := tr.BeginChange()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustPrepare[T any](t *testing.T, c *Change[T]) *Prepared[T] {
	t.Helper()
	p, err := c.Prepare()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func asObject(v any) map[string]any { return v.(map[string]any) }

// tracker.test.ts "materializes an immutable next revision and adopts it by
// pointer swap" and "takes O(1) immutable ownership for replacement and its
// operation payload": Track and PrepareReplace keep what they are handed,
// and nothing reaches the committed revision until the prepared change is
// adopted.
func TestTrackerTakesOwnership(t *testing.T) {
	input := map[string]any{"count": 1.0, "nested": map[string]any{"text": "a"}, "values": []any{1.0}}
	tr := Track(input)
	if !same(tr.Value(), input) {
		t.Error("Track copied the root; it takes ownership")
	}

	c := mustBegin(t, tr)
	must(t, c.State().Set("count", 2))
	concat(t, c.State().At("nested"), "text", "b")
	_, err := c.State().At("values").Push(2)
	must(t, err)
	wantJSON(t, tr.Value(), tree(t, `{"count": 1, "nested": {"text": "a"}, "values": [1]}`))

	p := mustPrepare(t, c)
	if !same(p.Base(), input) {
		t.Error("Base is not the committed revision")
	}
	wantJSON(t, p.Value(), tree(t, `{"count": 2, "nested": {"text": "ab"}, "values": [1, 2]}`))
	wantJSON(t, tr.Value(), tree(t, `{"count": 1, "nested": {"text": "a"}, "values": [1]}`))
	if err := mustPanic(t, func() { c.State().Get("count") }); !errors.Is(err, ErrDraftSettled) {
		t.Errorf("read after Prepare: panic %v, want ErrDraftSettled", err)
	}
	must(t, tr.Adopt(p))
	if !same(tr.Value(), p.Value()) {
		t.Error("Adopt did not commit the prepared revision itself")
	}

	replacement := map[string]any{"count": 3.0}
	pr, err := tr.PrepareReplace(replacement)
	must(t, err)
	if !same(pr.Value(), replacement) {
		t.Error("PrepareReplace copied the replacement; it takes ownership")
	}
	if r, ok := pr.Ops()[0].(Replace); !ok || len(pr.Ops()) != 1 || !same(r.Value, replacement) {
		t.Errorf("replacement ops %s, want one Replace carrying the replacement itself", jsonText(t, pr.Ops()))
	}
}

// tracker.test.ts "normalizes a deeply equal replacement without changing
// committed identity" and delta.test.ts "normalizes a deep no-op to exact
// previous identity": a no-op prepares the committed revision itself, and
// adopting it still advances the revision.
func TestTrackerNoOpKeepsIdentity(t *testing.T) {
	tr := mustTrack(t, `{"value": {"nested": [1, 2]}}`)
	c := mustBegin(t, tr)
	must(t, c.State().Set("value", map[string]any{"nested": []any{1, 2}}))
	p := mustPrepare(t, c)
	if !same(p.Value(), p.Base()) || len(p.Ops()) != 0 {
		t.Errorf("deep no-op: value is base %v, ops %s", same(p.Value(), p.Base()), jsonText(t, p.Ops()))
	}
	base := tr.Value()
	must(t, tr.Adopt(p))
	if !same(tr.Value(), base) || tr.Revision() != 1 {
		t.Errorf("adopting a no-op: same revision value %v, revision %d (want 1)", same(tr.Value(), base), tr.Revision())
	}
	noop, err := tr.PrepareReplace(tree(t, `{"value": {"nested": [1, 2]}}`))
	must(t, err)
	if !same(noop.Value(), tr.Value()) || len(noop.Ops()) != 0 || noop.BaseRevision() != 1 {
		t.Error("an equal replacement is not the committed revision itself")
	}
}

// tracker.test.ts "allows competing contexts and invalidates loser views",
// "rejects foreign and aborted prepared values", "lets a settled Change abort
// its prepared result": each refusal is its sentinel, and a refusal changes
// nothing.
func TestTrackerLifecycleErrors(t *testing.T) {
	first := mustTrack(t, `{"value": 0}`)
	second := mustTrack(t, `{"value": 0}`)
	winner := mustBegin(t, first)
	loser := mustBegin(t, first)
	held := loser.State()
	must(t, winner.State().Set("value", 1))
	must(t, held.Set("value", 2))
	p := mustPrepare(t, winner)
	if _, err := winner.Prepare(); !errors.Is(err, ErrChangeSettled) {
		t.Errorf("second Prepare: %v", err)
	}
	if err := second.Adopt(p); !errors.Is(err, ErrForeignPrepared) {
		t.Errorf("foreign Adopt: %v", err)
	}
	if err := second.Adopt(nil); !errors.Is(err, ErrForeignPrepared) {
		t.Errorf("nil Adopt: %v", err)
	}
	must(t, first.Adopt(p))
	if err := first.Adopt(p); !errors.Is(err, ErrPreparedUsed) {
		t.Errorf("repeated Adopt: %v", err)
	}
	if err := held.Set("value", 3); !errors.Is(err, ErrDraftSettled) {
		t.Errorf("write through the loser's draft: %v", err)
	}
	if _, err := loser.Prepare(); !errors.Is(err, ErrDraftSettled) {
		t.Errorf("Prepare of the loser: %v", err)
	}
	loser.Abort()
	if _, err := loser.Prepare(); !errors.Is(err, ErrChangeSettled) {
		t.Errorf("Prepare after Abort: %v", err)
	}

	stale, err := first.PrepareReplace(tree(t, `{"value": 2}`))
	must(t, err)
	aborted, err := first.PrepareReplace(tree(t, `{"value": 4}`))
	must(t, err)
	next, err := first.PrepareReplace(tree(t, `{"value": 3}`))
	must(t, err)
	must(t, first.Adopt(next))
	for range 2 {
		if err := first.Adopt(stale); !errors.Is(err, ErrStalePrepared) {
			t.Errorf("stale Adopt: %v", err)
		}
	}
	aborted.Abort()
	if err := first.Adopt(aborted); !errors.Is(err, ErrPreparedAborted) {
		t.Errorf("Adopt of a stale prepared change aborted first: %v, want aborted", err)
	}
	wantJSON(t, first.Value(), tree(t, `{"value": 3}`))

	c := mustBegin(t, first)
	must(t, c.State().Set("value", 9))
	pc := mustPrepare(t, c)
	c.Abort()
	c.Abort()
	if err := first.Adopt(pc); !errors.Is(err, ErrPreparedAborted) {
		t.Errorf("Adopt after the change's Abort: %v", err)
	}
	if got := first.Revision(); got != 2 {
		t.Errorf("revision %d, want 2", got)
	}
}

// noPanic runs fn and fails the test, rather than crashing it, if fn panics.
func noPanic(t *testing.T, what string, fn func() error) (err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s panicked: %v", what, r)
			err = nil
		}
	}()
	return fn()
}

// Go only: the zero values of the exported types. A zero Tracker holds no
// revision and a zero Prepared was prepared by no tracker, so each call
// reports how to get a real one instead of panicking — the zero Prepared as
// upstream's adopt reports anything it did not prepare ("belongs to a
// different tracker").
func TestTrackerZeroValues(t *testing.T) {
	var zero Tracker[any]
	if err := noPanic(t, "BeginChange on a zero Tracker", func() error { _, err := zero.BeginChange(); return err }); !errors.Is(err, ErrZeroTracker) {
		t.Errorf("BeginChange on a zero Tracker: %v, want ErrZeroTracker", err)
	}
	if err := noPanic(t, "PrepareReplace on a zero Tracker", func() error {
		_, err := zero.PrepareReplace(map[string]any{})
		return err
	}); !errors.Is(err, ErrZeroTracker) {
		t.Errorf("PrepareReplace on a zero Tracker: %v, want ErrZeroTracker", err)
	}
	tr := mustTrack(t, `{"value": 0}`)
	p, err := tr.PrepareReplace(tree(t, `{"value": 1}`))
	must(t, err)
	if err := noPanic(t, "Adopt on a zero Tracker", func() error { return zero.Adopt(p) }); !errors.Is(err, ErrZeroTracker) {
		t.Errorf("Adopt on a zero Tracker: %v, want ErrZeroTracker", err)
	}
	zeroPrepared := &Prepared[any]{}
	if err := noPanic(t, "Adopt of a zero Prepared", func() error { return tr.Adopt(zeroPrepared) }); !errors.Is(err, ErrForeignPrepared) {
		t.Errorf("Adopt of a zero Prepared: %v, want ErrForeignPrepared", err)
	}
	noPanic(t, "a zero Prepared", func() error { zeroPrepared.Abort(); _ = zeroPrepared.BaseRevision(); return nil })
	must(t, tr.Adopt(p)) // the refusals changed nothing

	var change Change[any]
	if err := noPanic(t, "Prepare of a zero Change", func() error { _, err := change.Prepare(); return err }); err == nil || !strings.Contains(err.Error(), "BeginChange") {
		t.Errorf("Prepare of a zero Change: %v, want an error naming BeginChange", err)
	}
	noPanic(t, "Abort of a zero Change", func() error { change.Abort(); return nil })
	if err := noPanic(t, "Set through a zero Change's State", func() error { return change.State().Set("x", 1) }); !errors.Is(err, errNilDraft) {
		t.Errorf("Set through a zero Change's State: %v, want errNilDraft", err)
	}
	var draft Draft
	if err := noPanic(t, "Set on a zero Draft", func() error { return draft.Set("x", 1) }); !errors.Is(err, errNilDraft) {
		t.Errorf("Set on a zero Draft: %v, want errNilDraft", err)
	}
	noPanic(t, "reading a zero Draft", func() error {
		if draft.Len() != 0 || draft.Keys() != nil || draft.Has("x") {
			t.Error("a zero Draft reads as non-empty")
		}
		return nil
	})
}

// tracker.test.ts "aborts idempotently and revokes held descendants".
func TestTrackerAbortSettlesDrafts(t *testing.T) {
	tr := mustTrack(t, `{"child": {"value": 1}, "values": [1, 2]}`)
	c := mustBegin(t, tr)
	child := c.State().At("child")
	must(t, child.Set("value", 2))
	c.Abort()
	wantJSON(t, tr.Value(), tree(t, `{"child": {"value": 1}, "values": [1, 2]}`))
	if err := mustPanic(t, func() { child.Get("value") }); !errors.Is(err, ErrDraftSettled) {
		t.Errorf("read after Abort: %v", err)
	}
	if err := child.Set("value", 3); !errors.Is(err, ErrDraftSettled) {
		t.Errorf("write after Abort: %v", err)
	}
	if err := c.State().Set("placed", child); !errors.Is(err, ErrDraftSettled) {
		t.Errorf("placing a settled draft: %v", err)
	}
	c.Abort()
	if _, err := c.Prepare(); !errors.Is(err, ErrChangeSettled) {
		t.Errorf("Prepare after Abort: %v", err)
	}
	other := mustBegin(t, tr)
	if err := other.State().Set("placed", child); !errors.Is(err, ErrDraftSettled) {
		t.Errorf("placing another change's settled draft: %v", err)
	}
	other.Abort()
}

// delta-clone.test.ts and tracker.test.ts "clones property, index, push,
// unshift, splice, fill, and copyWithin placements" and "expands repeated
// source aliases into independent placements": one Go map placed twice
// becomes two values, and neither sees the caller's later writes.
func TestDraftCopiesPlacements(t *testing.T) {
	tr := mustTrack(t, `{"rows": [], "left": null, "right": null}`)
	assigned := map[string]any{"nested": map[string]any{"value": 1}}
	c := mustBegin(t, tr)
	_, err := c.State().At("rows").Push(assigned, assigned)
	must(t, err)
	must(t, c.State().Set("left", assigned))
	must(t, c.State().Set("right", assigned))
	asObject(assigned["nested"])["value"] = 9
	if v, _ := c.State().At("rows").At(0).At("nested").Get("value"); v != 1.0 {
		t.Errorf("draft saw the caller's write: %v", v)
	}
	must(t, c.State().At("left").At("nested").Set("value", 2))
	p := mustPrepare(t, c)
	v := p.Value().(map[string]any)
	rows := v["rows"].([]any)
	if same(rows[0], rows[1]) || same(v["left"], v["right"]) {
		t.Error("repeated placements share one container")
	}
	wantJSON(t, v, tree(t, `{"rows": [{"nested": {"value": 1}}, {"nested": {"value": 1}}], "left": {"nested": {"value": 2}}, "right": {"nested": {"value": 1}}}`))
	replica, err := ApplyImmutable(p.Base(), p.Ops())
	must(t, err)
	wantJSON(t, replica, v)
}

// tracker.test.ts "shares immutable operation placements with the
// materialized candidate": a placement's payload is the container the
// prepared revision holds.
func TestTrackerPayloadsShareWithValue(t *testing.T) {
	tr := mustTrack(t, `{"rows": [{"id": 0}], "meta": {}}`)
	c := mustBegin(t, tr)
	_, err := c.State().At("rows").Push(map[string]any{"id": 1})
	must(t, err)
	p := mustPrepare(t, c)
	splice, ok := p.Ops()[0].(Splice)
	if !ok {
		t.Fatalf("ops %s, want a splice first", jsonText(t, p.Ops()))
	}
	if placed := p.Value().(map[string]any)["rows"].([]any)[1]; !same(placed, splice.Items[0]) {
		t.Error("the prepared revision holds a copy of the splice's payload")
	}
}

// state-draft.test.ts "copies only changed branches", and tracker.test.ts
// "uses one proxy identity per accessed container" and "does not dirty
// read-only traversals".
func TestDraftCopiesOnlyChangedBranches(t *testing.T) {
	tr := mustTrack(t, `{"changed": {"count": 1, "sibling": {"value": "kept"}}, "untouched": {"value": 2}, "read": {"x": {}}}`)
	base := tr.Value().(map[string]any)
	c := mustBegin(t, tr)
	first := c.State().At("changed")
	if c.State().At("changed") != first {
		t.Error("two reads of one member are two drafts")
	}
	c.State().At("read").At("x").Get("y")
	must(t, first.Set("count", 3))
	p := mustPrepare(t, c)
	v := p.Value().(map[string]any)
	switch {
	case same(v, base):
		t.Error("the root was not copied")
	case same(v["changed"], base["changed"]):
		t.Error("the changed branch was not copied")
	case !same(asObject(v["changed"])["sibling"], asObject(base["changed"])["sibling"]):
		t.Error("an unchanged sibling was copied")
	case !same(v["untouched"], base["untouched"]), !same(v["read"], base["read"]):
		t.Error("an untouched or only-read branch was copied")
	}
}

// tracker.test.ts "normalizes restored overrides and cancelled structural
// edits to no operations": written-and-restored branches keep their identity.
func TestDraftRestoredBranchKeepsIdentity(t *testing.T) {
	tr := mustTrack(t, `{"a": 1, "o": {"x": 1}, "v": [1, 2]}`)
	base := tr.Value().(map[string]any)
	c := mustBegin(t, tr)
	must(t, c.State().Set("a", 2))
	must(t, c.State().At("o").Set("x", 5))
	must(t, c.State().At("o").Set("x", 1))
	_, err := c.State().At("v").Push(3)
	must(t, err)
	_, _, err = c.State().At("v").Pop()
	must(t, err)
	p := mustPrepare(t, c)
	v := p.Value().(map[string]any)
	if !same(v["o"], base["o"]) || !same(v["v"], base["v"]) {
		t.Error("a written-and-restored branch lost its identity")
	}
	wantOps(t, p.Ops(), `[["s", ["a"], 2]]`)
}

// Two empty arrays are two arrays. encoding/json gives every [] it decodes one
// zero-size address, so a revision's empty arrays have no identity of their
// own (value.go, anonymous); the overlay finds their nodes by where they sit,
// so writes to one never land in another.
func TestDraftEmptyArraysAreDistinct(t *testing.T) {
	tr := mustTrack(t, `{"a": [], "b": [], "c": {"d": [], "e": []}, "rows": [[], []]}`)
	c := mustBegin(t, tr)
	if c.State().At("a") == c.State().At("b") || c.State().At("rows").At(0) == c.State().At("rows").At(1) {
		t.Fatal("two empty arrays share one draft")
	}
	_, err := c.State().At("a").Push(1)
	must(t, err)
	_, err = c.State().At("c").At("e").Push(2)
	must(t, err)
	_, err = c.State().At("rows").At(1).Push(5)
	must(t, err)
	must(t, c.State().Set("f", []any{}))
	must(t, c.State().Set("g", []any{}))
	_, err = c.State().At("g").Push(3)
	must(t, err)
	p := mustPrepare(t, c)
	wantJSON(t, p.Value(), tree(t, `{"a": [1], "b": [], "c": {"d": [], "e": [2]}, "f": [], "g": [3], "rows": [[], [5]]}`))
	must(t, tr.Adopt(p))
	c = mustBegin(t, tr)
	_, err = c.State().At("f").Push(4)
	must(t, err)
	wantJSON(t, mustPrepare(t, c).Value(), tree(t, `{"a": [1], "b": [], "c": {"d": [], "e": [2]}, "f": [4], "g": [3], "rows": [[], [5]]}`))
}

// Go values in, JSON values out: a placement is chord's copyJson of what the
// caller hands over (chord.IsValue's Go spelling of strict JSON), so any Go
// numeric kind is one JavaScript number, a typed slice or string-keyed map a
// plain array or object, and a typed nil container an empty one. Anything else
// is refused with pi's text, and a refused placement leaves the draft as it
// was.
func TestDraftPlacementRepresentation(t *testing.T) {
	type myInt int
	type myFloat float64
	type key string
	tr := mustTrack(t, `{"xs": []}`)
	c := mustBegin(t, tr)
	must(t, c.State().Set("v", map[string]any{
		"int": 1, "int64": int64(2), "uint8": uint8(3), "float32": float32(0.5), "named": myInt(4),
		"namedFloat": myFloat(6.5), "number": json.Number("5e2"), "nilMap": map[string]any(nil), "nilSlice": []any(nil),
		"strings": []string{"a"}, "typedMap": map[key]int{"k": 1}, "array": [2]bool{true, false},
	}))
	v, _ := c.State().At("v").Get("nilMap")
	if d, ok := v.(*Draft); !ok || d.Len() != 0 || d.IsArray() {
		t.Errorf("a nil map placed as %#v, want an empty object", v)
	}
	p := mustPrepare(t, c)
	wantJSON(t, asObject(p.Value())["v"], tree(t, `{"int":1,"int64":2,"uint8":3,"float32":0.5,"named":4,"namedFloat":6.5,"number":500,"nilMap":{},"nilSlice":[],"strings":["a"],"typedMap":{"k":1},"array":[true,false]}`))
	var normalize func(v any)
	normalize = func(v any) {
		switch x := v.(type) {
		case float64, string, bool:
		case []any:
			for _, item := range x {
				normalize(item)
			}
		case map[string]any:
			for _, item := range x {
				normalize(item)
			}
		default:
			t.Errorf("placed as %T", v)
		}
	}
	normalize(asObject(p.Value())["v"])

	cases := []struct {
		value any
		text  string
	}{
		{math.NaN(), jsonvalue.NonFinite},
		{math.Inf(-1), jsonvalue.NonFinite},
		{json.Number("1e400"), jsonvalue.NonFinite},
		{float32(math.NaN()), jsonvalue.NonFinite},
		{float32(math.Inf(1)), jsonvalue.NonFinite},
		{json.Number("NaN"), jsonvalue.NonFinite},
		{func() {}, fmt.Sprintf(jsonvalue.NonJSON, "function")},
		{complex(1, 2), fmt.Sprintf(jsonvalue.NonJSON, "complex128")},
		{make(chan int), fmt.Sprintf(jsonvalue.NonJSON, "chan int")},
		{time.Unix(0, 0), jsonvalue.Plain},
		{&struct{}{}, jsonvalue.Plain},
		{map[int]string{1: "b"}, jsonvalue.Plain},
		{[]byte("a"), jsonvalue.Plain},
	}
	c = mustBegin(t, tr)
	for _, tc := range cases {
		var ve *ValueError
		if err := c.State().Set("v", map[string]any{"nested": tc.value}); !errors.As(err, &ve) || ve.Message != tc.text {
			t.Errorf("Set(%T %v): %v, want %q", tc.value, tc.value, err, tc.text)
		}
		if _, err := c.State().At("xs").Push(1, tc.value); !errors.As(err, &ve) || ve.Message != tc.text {
			t.Errorf("Push(%T %v): %v, want %q", tc.value, tc.value, err, tc.text)
		}
	}
	if n := c.State().At("xs").Len(); n != 0 || c.State().Has("v") {
		t.Errorf("a refused placement changed the draft: %d items, v present %v", n, c.State().Has("v"))
	}
	// The error names the number, whatever kind it came as.
	if err := c.State().Set("v", float32(math.Inf(-1))); err == nil || !strings.Contains(err.Error(), "got -Inf") {
		t.Errorf("float32 -Inf: %v, want the error to say it got -Inf", err)
	}
	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	if err := c.State().Set("v", cyclic); err == nil || !strings.Contains(err.Error(), jsonvalue.Cycle) {
		t.Errorf("cycle: %v", err)
	}
	loop := make([]any, 1)
	loop[0] = loop
	if _, err := c.State().At("xs").Push(loop); err == nil || !strings.Contains(err.Error(), jsonvalue.Cycle) {
		t.Errorf("array cycle: %v", err)
	}
	c.Abort()
	wantJSON(t, tr.Value(), tree(t, `{"xs": []}`))

	// A scalar root is a revision, as in pi's runtime (golden "scalar
	// roots"). It has no draft, and a replacement must be a container.
	for _, root := range []any{nil, 1.0, "x", true} {
		tr := Track(root)
		if _, err := tr.BeginChange(); !errors.Is(err, ErrScalarRevision) {
			t.Errorf("BeginChange on %#v: %v, want ErrScalarRevision", root, err)
		}
		if _, err := tr.PrepareReplace(2.0); !errors.Is(err, ErrScalarRevision) {
			t.Errorf("PrepareReplace of a scalar on %#v: %v, want ErrScalarRevision", root, err)
		}
		if _, err := tr.PrepareReplace(map[string]any{}); err != nil {
			t.Errorf("PrepareReplace of an object on %#v: %v", root, err)
		}
	}
}

// A nil draft — what At returns for a member that is absent or a scalar —
// reads as empty, and every write to it reports the missing link. An array
// method on an object draft is upstream's incompatible receiver.
func TestDraftNilAndKindErrors(t *testing.T) {
	tr := mustTrack(t, `{"n": 1, "o": {}, "v": []}`)
	c := mustBegin(t, tr)
	for _, key := range []string{"missing", "n"} {
		d := c.State().At(key)
		if d != nil {
			t.Fatalf("At(%q) = %v, want nil", key, d)
		}
		if d.Len() != 0 || d.Keys() != nil || d.IsArray() || d.Has("x") {
			t.Errorf("nil draft reads as non-empty")
		}
		if err := d.Set("x", 1); !errors.Is(err, errNilDraft) {
			t.Errorf("Set on nil draft: %v", err)
		}
		if _, err := d.Push(1); !errors.Is(err, errNilDraft) {
			t.Errorf("Push on nil draft: %v", err)
		}
	}
	o := c.State().At("o")
	for name, call := range map[string]func() error{
		"Push":       func() error { _, err := o.Push(1); return err },
		"Pop":        func() error { _, _, err := o.Pop(); return err },
		"Shift":      func() error { _, _, err := o.Shift(); return err },
		"Unshift":    func() error { _, err := o.Unshift(1); return err },
		"Splice":     func() error { _, err := o.Splice(0, 0); return err },
		"Sort":       func() error { return o.Sort(nil) },
		"Reverse":    func() error { return o.Reverse() },
		"Fill":       func() error { return o.Fill(1, 0, 0) },
		"CopyWithin": func() error { return o.CopyWithin(0, 0, 0) },
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "Array mutator called on incompatible receiver ("+name) {
			t.Errorf("%s on an object: %v", name, err)
		}
	}
	if err := o.SetLen(0); err == nil || !strings.Contains(err.Error(), "SetLen on an object draft") {
		t.Errorf("SetLen on an object: %v", err)
	}
	if err := c.State().At("v").SetLen(-1); err == nil || !strings.Contains(err.Error(), "Invalid array length") {
		t.Errorf("SetLen(-1): %v", err)
	}
	if err := c.State().Set(true, 1); !errors.Is(err, ErrInvalidOp) {
		t.Errorf("Set with a bool key: %v", err)
	}
	c.Abort()
	wantJSON(t, tr.Value(), tree(t, `{"n": 1, "o": {}, "v": []}`))
}

// Characterization of documented reads with no pi value to capture: Len of an
// object is its member count (Object.keys(o).length), and Splice, Pop and
// Shift hand back removed containers as drafts detached from the tree, whose
// writes publish nothing.
func TestDraftGoReads(t *testing.T) {
	tr := mustTrack(t, `{"o": {"a": 1, "b": 2, "c": 3}, "v": [{"n": 1}, 2, {"n": 3}]}`)
	c := mustBegin(t, tr)
	if n := c.State().At("o").Len(); n != 3 {
		t.Errorf("Len of a 3-member object = %d", n)
	}
	must(t, c.State().At("o").Delete("b"))
	if n := c.State().At("o").Len(); n != 2 {
		t.Errorf("Len after a delete = %d", n)
	}
	v := c.State().At("v")
	removed, err := v.Splice(0, 2)
	must(t, err)
	if len(removed) != 2 || removed[1] != 2.0 {
		t.Fatalf("Splice returned %#v", removed)
	}
	detached, ok := removed[0].(*Draft)
	if !ok {
		t.Fatalf("a removed object came back as %T", removed[0])
	}
	must(t, detached.Set("n", 10))
	last, ok, err := v.Pop()
	must(t, err)
	if d, isDraft := last.(*Draft); !ok || !isDraft || d.Len() != 1 {
		t.Fatalf("Pop returned %#v", last)
	}
	p := mustPrepare(t, c)
	wantJSON(t, p.Value(), tree(t, `{"o": {"a": 1, "c": 3}, "v": []}`))
}

// ─── Retention ───────────────────────────────────────────────────────────────
//
// delta-tracker/retention.test.ts: what a settled change, an unadopted
// prepared change, a stale competitor and a settled draft may still hold.

func collected[T any](t *testing.T, w weak.Pointer[T], what string) {
	t.Helper()
	for range 30 {
		runtime.GC()
		if w.Value() == nil {
			return
		}
	}
	t.Errorf("%s is still retained", what)
}

func payloadTracker() *Tracker[map[string]any] {
	return Track(map[string]any{"payload": nil})
}

func bigPayload() map[string]any {
	rows := make([]any, 50_000)
	for i := range rows {
		rows[i] = map[string]any{"value": i}
	}
	return map[string]any{"rows": rows}
}

//go:noinline
func abortedPayload(t *testing.T, tr *Tracker[map[string]any]) (weak.Pointer[Draft], weak.Pointer[any]) {
	c := mustBegin(t, tr)
	must(t, c.State().Set("payload", bigPayload()))
	rows := c.State().At("payload").At("rows")
	backing := rows.n.base.([]any)
	c.Abort()
	return weak.Make(rows), weak.Make(&backing[0])
}

//go:noinline
func unadoptedPrepared(t *testing.T, tr *Tracker[map[string]any]) weak.Pointer[any] {
	c := mustBegin(t, tr)
	must(t, c.State().Set("payload", bigPayload()))
	p := mustPrepare(t, c)
	rows := asObject(p.Value()["payload"])["rows"].([]any)
	return weak.Make(&rows[0])
}

//go:noinline
func draftHandles(t *testing.T, tr *Tracker[map[string]any]) weak.Pointer[Draft] {
	c := mustBegin(t, tr)
	must(t, c.State().Set("payload", map[string]any{"rows": []any{map[string]any{"value": 1}}}))
	row := c.State().At("payload").At("rows").At(0)
	c.Abort()
	return weak.Make(row)
}

var retainedHandles []*Draft

// A draft handle retained past its change retains nothing of the change.
//
//go:noinline
func retainedHandle(t *testing.T, tr *Tracker[map[string]any]) weak.Pointer[any] {
	c := mustBegin(t, tr)
	must(t, c.State().Set("payload", bigPayload()))
	payload := c.State().At("payload")
	rows := payload.At("rows")
	backing := rows.n.base.([]any)
	c.Abort()
	retainedHandles = append(retainedHandles, payload, rows)
	return weak.Make(&backing[0])
}

var retainedSettled []any

// A tracker whose settled change and prepared change are retained is still
// collected, and so is its committed payload.
//
//go:noinline
func settledLifecycle(t *testing.T) (weak.Pointer[Tracker[map[string]any]], weak.Pointer[any]) {
	tr := payloadTracker()
	c := mustBegin(t, tr)
	must(t, c.State().Set("payload", map[string]any{"rows": []any{map[string]any{"value": 1}}}))
	retained := mustPrepare(t, c)
	must(t, tr.Adopt(retained))
	retainedSettled = append(retainedSettled, c, retained)

	future, err := tr.PrepareReplace(map[string]any{"payload": bigPayload()})
	must(t, err)
	must(t, tr.Adopt(future))
	rows := asObject(future.Value()["payload"])["rows"].([]any)
	return weak.Make(tr), weak.Make(&rows[0])
}

// retention.worker.ts "stale-unprepared-change": a competitor the caller keeps
// retains nothing of the revision it was begun on once another change is
// adopted.
//
//go:noinline
func staleUnprepared(t *testing.T) (*Tracker[map[string]any], weak.Pointer[any]) {
	tr := Track(map[string]any{"payload": bigPayload()})
	stale := mustBegin(t, tr)
	must(t, stale.State().At("payload").At("rows").At(0).Set("value", -1))
	obsolete := asObject(tr.Value()["payload"])["rows"].([]any)
	winner, err := tr.PrepareReplace(map[string]any{"payload": map[string]any{"rows": []any{map[string]any{"value": 1}}}})
	must(t, err)
	must(t, tr.Adopt(winner))
	if _, err := stale.Prepare(); !errors.Is(err, ErrDraftSettled) {
		t.Errorf("Prepare of the stale change: %v", err)
	}
	retainedSettled = append(retainedSettled, stale)
	return tr, weak.Make(&obsolete[0])
}

// retention.worker.ts "retained-settled-prepared": a prepared change keeps its
// revisions readable.
//
//go:noinline
func retainedPrepared(t *testing.T) (*Prepared[map[string]any], weak.Pointer[any]) {
	tr := Track(map[string]any{"payload": bigPayload()})
	c := mustBegin(t, tr)
	must(t, c.State().Set("n", 1))
	p := mustPrepare(t, c)
	rows := asObject(p.Base()["payload"])["rows"].([]any)
	return p, weak.Make(&rows[0])
}

func TestTrackerRetention(t *testing.T) {
	t.Run("aborted-payload", func(t *testing.T) {
		tr := payloadTracker()
		handle, backing := abortedPayload(t, tr)
		collected(t, handle, "an aborted change's draft")
		collected(t, backing, "an aborted change's payload")
		wantJSON(t, tr.Value(), tree(t, `{"payload": null}`))
	})
	t.Run("unadopted-prepared", func(t *testing.T) {
		tr := payloadTracker()
		collected(t, unadoptedPrepared(t, tr), "an unadopted prepared payload")
		wantJSON(t, tr.Value(), tree(t, `{"payload": null}`))
	})
	t.Run("retained-handle", func(t *testing.T) {
		tr := payloadTracker()
		collected(t, retainedHandle(t, tr), "the payload behind a retained, settled draft")
	})
	t.Run("draft-handles", func(t *testing.T) {
		tr := payloadTracker()
		collected(t, draftHandles(t, tr), "an aborted change's member draft")
		wantJSON(t, tr.Value(), tree(t, `{"payload": null}`))
	})
	t.Run("settled-lifecycle", func(t *testing.T) {
		tracker, future := settledLifecycle(t)
		collected(t, tracker, "a tracker whose settled change and prepared change are retained")
		collected(t, future, "a dropped tracker's committed payload")
	})
	t.Run("stale-unprepared-change", func(t *testing.T) {
		tr, obsolete := staleUnprepared(t)
		collected(t, obsolete, "the revision a retained stale change was begun on")
		wantJSON(t, tr.Value(), tree(t, `{"payload": {"rows": [{"value": 1}]}}`))
	})
	t.Run("retained-settled-prepared", func(t *testing.T) {
		p, base := retainedPrepared(t)
		for range 5 {
			runtime.GC()
		}
		if base.Value() == nil {
			t.Error("a retained Prepared lost its base revision")
		}
		runtime.KeepAlive(p)
	})
}

// A comparator that settles its own change — prepares or aborts it, or adopts
// a competitor — ends the sort with ErrDraftSettled rather than a crash on the
// overlay it released. Upstream leaves such a comparator unspecified.
func TestSortComparatorThatSettlesItsChange(t *testing.T) {
	for name, settle := range map[string]func(tr *Tracker[any], c *Change[any]){
		"abort":   func(_ *Tracker[any], c *Change[any]) { c.Abort() },
		"prepare": func(_ *Tracker[any], c *Change[any]) { _, _ = c.Prepare() },
		"adopt a competitor": func(tr *Tracker[any], _ *Change[any]) {
			p, err := tr.PrepareReplace(tree(t, `{"v": []}`))
			must(t, err)
			must(t, tr.Adopt(p))
		},
	} {
		tr := mustTrack(t, `{"v": [3, 1, 2]}`)
		c := mustBegin(t, tr)
		settled := false
		err := noPanic(t, name, func() error {
			return c.State().At("v").Sort(func(a, b any) int {
				if !settled {
					settled = true
					settle(tr, c)
				}
				return int(a.(float64) - b.(float64))
			})
		})
		if !errors.Is(err, ErrDraftSettled) {
			t.Errorf("%s: Sort = %v, want ErrDraftSettled", name, err)
		}
	}
}

// tracker.test.ts (upstream 091bcc099) "emits one insert for an append to a
// long array": the ops are the golden "append-10000"'s; here, that the plan
// behind them is computed from the pieces alone. A walk over the base entries
// allocates per entry, the plan of two pieces a handful of bytes.
func TestTrackerAppendPlanWalksNoEntries(t *testing.T) {
	const n = 100_000
	values := make([]any, n)
	for i := range values {
		values[i] = float64(i)
	}
	tr := Track(map[string]any{"values": values})
	c := mustBegin(t, tr)
	d := c.State().At("values")
	if _, err := d.Push(-1.0); err != nil {
		t.Fatal(err)
	}
	var plan *arrayPlan
	bytes := allocatedBytes(func() { plan = d.n.buildPlan() })
	if bytes > 4096 {
		t.Errorf("planning an append to a %d-entry array allocated %d bytes: it walked the base entries", n, bytes)
	}
	if len(plan.removals) != 0 || plan.permutation != nil || len(plan.insertions) != 1 || plan.insertions[0].at != n {
		t.Errorf("plan %+v, want one insertion at %d", plan, n)
	}
	p := mustPrepare(t, c)
	wantOps(t, p.Ops(), `[["p", ["values"], 100000, 0, [-1]]]`)
}

// tracker.test.ts (upstream 091bcc099) "emits a permutation when kept
// elements change order": the pieces that moved base entries take the walk.
func TestTrackerMovedEntriesEmitAPermutation(t *testing.T) {
	parity := func(a, b any) int {
		x, y := int(a.(float64)), int(b.(float64))
		if c := x%2 - y%2; c != 0 {
			return c
		}
		return x - y
	}
	for _, c := range []struct {
		name   string
		mutate func(d *Draft) error
		want   string
	}{
		{"reverse", (*Draft).Reverse, `{"values":[4,3,2,1,0]}`},
		{"sort by parity", func(d *Draft) error { return d.Sort(parity) }, `{"values":[0,2,4,1,3]}`},
	} {
		tr := Track(tree(t, `{"values":[0,1,2,3,4]}`).(map[string]any))
		change := mustBegin(t, tr)
		must(t, c.mutate(change.State().At("values")))
		p := mustPrepare(t, change)
		if !slices.ContainsFunc(p.Ops(), func(op Op) bool { _, ok := op.(Permute); return ok }) {
			t.Errorf("%s: ops %s carry no permutation", c.name, jsonText(t, p.Ops()))
		}
		replica, err := ApplyImmutable(tr.Value(), p.Ops())
		must(t, err)
		wantJSON(t, replica, tree(t, c.want))
	}
}

// One write to a wide object prepares in time linear in the object: counting
// and copying its members sorts nothing, and a key that is not an index is
// told apart from one without strconv's allocating error.
func TestTrackerWideObjectWriteIsLinear(t *testing.T) {
	wide := make(map[string]any, 20_000)
	for i := range 20_000 {
		wide[fmt.Sprintf("field%d", i)] = float64(i)
	}
	tr := Track(map[string]any{"wide": wide})
	allocs := testing.AllocsPerRun(1, func() {
		c := mustBegin(t, tr)
		d := c.State().At("wide")
		must(t, d.Set("field7", -7.0))
		if d.Len() != 20_000 {
			t.Fatalf("Len = %d", d.Len())
		}
		p := mustPrepare(t, c)
		if len(p.Ops()) != 1 {
			t.Fatalf("ops %s", jsonText(t, p.Ops()))
		}
	})
	// A copy of the object is a few thousand allocations; a sort of its keys
	// with an allocating comparison is a million.
	if allocs > 20_000 {
		t.Errorf("%v allocations to prepare one write to a 20,000-member object", allocs)
	}
}

// A nil map or slice root is JSON null — encoding/json writes it so, and a
// base batch carries it so — and pi's overlay refuses a null root as it does a
// scalar (its WeakMap takes only objects). Drafting it as an empty container
// would publish ops a replica built from that base cannot apply.
func TestTrackerRefusesANilRoot(t *testing.T) {
	if _, err := Track[map[string]any](nil).BeginChange(); !errors.Is(err, ErrScalarRevision) {
		t.Errorf("BeginChange on a nil map: %v, want ErrScalarRevision", err)
	}
	if _, err := Track[[]any](nil).BeginChange(); !errors.Is(err, ErrScalarRevision) {
		t.Errorf("BeginChange on a nil slice: %v, want ErrScalarRevision", err)
	}
	if _, err := Track(map[string]any{"a": 1.0}).PrepareReplace(nil); !errors.Is(err, ErrScalarRevision) {
		t.Errorf("PrepareReplace of a nil map: %v, want ErrScalarRevision", err)
	}
}
