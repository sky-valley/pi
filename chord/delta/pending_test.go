package delta

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"unsafe"
	"weak"
)

// delta-pending.test.ts (upstream d8d99950a, with bdb151774's fuzz timing):
// folding is invisible to a replica, the cap, ownership of received ops,
// cost, and what the queue keeps alive. testdata/upstream_delta.json's
// "pending" section pins take() and size against pi's own PendingOps over the
// same sequences and a corpus of pi's tracker and diffRevisions batches.

// ─── A seeded generator ──────────────────────────────────────────────────────

// pendingRandom is the upstream test's random(seed): the same LCG, so a
// failing sequence reproduces from its seed. Its texts leave out the lone
// surrogate halves upstream's include: a Go string cannot hold one, and a Go
// producer never splits a code point.
type pendingRandom struct{ state uint32 }

func newPendingRandom(seed uint32) *pendingRandom { return &pendingRandom{state: seed} }

func (r *pendingRandom) next() float64 {
	r.state = r.state*1664525 + 1013904223
	return float64(r.state) / 4294967296
}

func (r *pendingRandom) int(n int) int { return int(math.Floor(r.next() * float64(n))) }

var (
	pendingTexts = []string{"a", "bc", " tok", "é", "😀", "\u2028", "日本語", `"q"`, `\`, "", strings.Repeat("x", 30)}
	pendingKeys  = []string{"a", "b", "0", "1", "01", "a.b", "x y", "text", "list"}
)

func (r *pendingRandom) text() string { return pendingTexts[r.int(len(pendingTexts))] }

func (r *pendingRandom) primitive() any {
	switch r.int(4) {
	case 0:
		return float64(r.int(100))
	case 1:
		return r.text()
	case 2:
		return r.next() < 0.5
	}
	return nil
}

func (r *pendingRandom) value(depth int) any {
	roll := r.next()
	if depth > 2 || roll < 0.4 {
		return r.primitive()
	}
	if roll < 0.7 {
		items := make([]any, r.int(4))
		for i := range items {
			items[i] = r.value(depth + 1)
		}
		return items
	}
	object := map[string]any{}
	for count := r.int(4); count >= 0; count-- {
		object[pendingKeys[r.int(len(pendingKeys))]] = r.value(depth + 1)
	}
	return object
}

func (r *pendingRandom) root() any {
	object := map[string]any{}
	for count := r.int(4) + 1; count >= 0; count-- {
		object[pendingKeys[r.int(len(pendingKeys))]] = r.value(0)
	}
	return object
}

// ─── Random edits through a draft ────────────────────────────────────────────

type draftSlot struct {
	parent *Draft
	key    any
}

// draftSlots is every (parent, key) slot under d, parents first.
func draftSlots(t *testing.T, d *Draft, out []draftSlot) []draftSlot {
	var keys []any
	if d.IsArray() {
		for i := range d.Len() {
			keys = append(keys, i)
		}
	} else {
		for _, k := range d.Keys() {
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		out = append(out, draftSlot{d, k})
		if v, _ := d.Get(k); v != nil {
			if child, ok := v.(*Draft); ok {
				out = draftSlots(t, child, out)
			}
		}
	}
	return out
}

// plain is a value as a comparator reads it: a draft's snapshot.
func plain(t *testing.T, v any) any {
	if d, ok := v.(*Draft); ok {
		s, err := d.Snapshot()
		must(t, err)
		return s
	}
	return v
}

func byJSON(t *testing.T) func(a, b any) int {
	return func(a, b any) int {
		return strings.Compare(jsonText(t, plain(t, a)), jsonText(t, plain(t, b)))
	}
}

func pick[T any](r *pendingRandom, items []T) T { return items[r.int(len(items))] }

// mutateAnywhere is the upstream test's: one random edit anywhere in the
// draft — string appends (through AppendText when appendText is set) and
// trims, array splices and reorders, object edits.
func mutateAnywhere(t *testing.T, d *Draft, r *pendingRandom, appendText bool) {
	all := draftSlots(t, d, nil)
	if len(all) == 0 {
		must(t, d.Set(pick(r, []string{"a", "b"}), r.value(0)))
		return
	}
	s := pick(r, all)
	current, _ := s.parent.Get(s.key)
	roll := r.next()
	switch c := current.(type) {
	case string:
		switch {
		case roll < 0.3 && !s.parent.IsArray():
			if appendText {
				must(t, s.parent.AppendText(s.key.(string), r.text()))
			} else {
				must(t, s.parent.Set(s.key, c+r.text()))
			}
		case roll < 0.6:
			must(t, s.parent.Set(s.key, c+r.text()))
		case roll < 0.8:
			// current.slice(min(length, n)), in UTF-16 code units; the texts
			// are BMP or whole pairs, so a cut of up to 2 units at the front
			// lands on a boundary or inside the one astral character.
			must(t, s.parent.Set(s.key, jsSliceFront(c, r.int(3))))
		default:
			must(t, s.parent.Set(s.key, r.primitive()))
		}
	case *Draft:
		if c.IsArray() {
			n := c.Len()
			switch {
			case roll < 0.3:
				_, err := c.Push(r.value(0))
				must(t, err)
			case roll < 0.5:
				start, remove := r.int(n+1), r.int(2)
				var items []any
				if r.next() < 0.5 {
					items = []any{r.value(0)}
				}
				_, err := c.Splice(start, remove, items...)
				must(t, err)
			case roll < 0.65 && n > 0:
				must(t, c.Set(r.int(n), r.value(0)))
			case roll < 0.75 && n > 1:
				must(t, c.Reverse())
			case roll < 0.85 && n > 1:
				must(t, c.Sort(byJSON(t)))
			case roll < 0.95:
				must(t, s.parent.Set(s.key, r.value(0)))
			default:
				must(t, c.SetLen(0))
			}
			return
		}
		keys := c.Keys()
		switch {
		case roll < 0.4:
			must(t, c.Set(pick(r, []string{"a", "b", "1", "x y"}), r.value(0)))
		case roll < 0.6 && len(keys) > 0:
			must(t, c.Delete(pick(r, keys)))
		default:
			must(t, s.parent.Set(s.key, r.value(0)))
		}
	default:
		if r.next() < 0.7 {
			must(t, s.parent.Set(s.key, r.primitive()))
		} else {
			must(t, s.parent.Set(s.key, r.value(0)))
		}
	}
}

// jsSliceFront is s.slice(min(s.length, units)) in UTF-16 code units, with a
// cut inside a pair dropping the whole character (the high half pi would keep
// is not a Go string).
func jsSliceFront(s string, units int) string {
	for i, ch := range s {
		if units <= 0 {
			return s[i:]
		}
		units--
		if ch > 0xFFFF {
			units--
		}
	}
	return ""
}

func newListEntry(x string) map[string]any { return map[string]any{"x": x, "n": 0.0} }

// mutateList is the upstream test's: leaf edits of a list of elements
// interleaved with splices, pushes and reorders of the list.
func mutateList(t *testing.T, d *Draft, r *pendingRandom) {
	list := d.At("list")
	if list == nil || !list.IsArray() {
		must(t, d.Set("list", []any{newListEntry(""), newListEntry(""), newListEntry("")}))
		return
	}
	n := list.Len()
	roll := r.next()
	switch {
	case roll < 0.45 && n > 0:
		element := list.At(r.int(n))
		if element != nil && !element.IsArray() {
			switch {
			case r.next() < 0.3:
				if err := element.AppendText("x", r.text()); err != nil {
					// pi's appendText throws on a non-string x, which the
					// generator never makes: an element is {x, n} or a primitive.
					t.Fatal(err)
				}
			case r.next() < 0.5:
				x, _ := element.Get("x")
				must(t, element.Set("x", x.(string)+r.text()))
			default:
				must(t, element.Set("n", float64(r.int(9))))
			}
		} else {
			must(t, list.Set(r.int(n), newListEntry("")))
		}
	case roll < 0.6:
		start, remove := r.int(n+1), r.int(2)
		var items []any
		if r.next() < 0.6 {
			items = []any{newListEntry(r.text())}
		}
		_, err := list.Splice(start, remove, items...)
		must(t, err)
	case roll < 0.7:
		_, err := list.Push(newListEntry(""))
		must(t, err)
	case roll < 0.8 && n > 1:
		must(t, list.Reverse())
	case roll < 0.9 && n > 0:
		must(t, list.Set(r.int(n), r.primitive()))
	case n > 0:
		_, err := list.Splice(r.int(n), 1)
		must(t, err)
	}
}

// mutateRootArray is the upstream test's: pushes, splices, reorders, element
// sets and shifts of a root array.
func mutateRootArray(t *testing.T, d *Draft, r *pendingRandom) {
	n := d.Len()
	var err error
	switch r.int(7) {
	case 0:
		_, err = d.Push(r.value(0))
	case 1:
		_, err = d.Splice(r.int(n+1), r.int(4), r.value(0))
	case 2:
		err = d.Reverse()
	case 3:
		err = d.Sort(byJSON(t))
	case 4:
		if n > 0 {
			err = d.Set(r.int(n), r.value(0))
		}
	case 5:
		_, _, err = d.Shift()
	default:
		_, err = d.Splice(r.int(n+1), r.int(4))
	}
	must(t, err)
}

// ─── Sequences ───────────────────────────────────────────────────────────────

type pendingCommit struct {
	ops   []Op
	value any
}

type pendingSequence struct {
	initial any
	commits []pendingCommit
}

func trackedSequence(t *testing.T, initial any, commits int, r *pendingRandom, mutate func(*Draft), replace func() any) pendingSequence {
	tr := Track(cloneJSON(initial))
	out := make([]pendingCommit, 0, commits)
	for range commits {
		var p *Prepared[any]
		if r.next() < 0.03 {
			var err error
			p, err = tr.PrepareReplace(replace())
			must(t, err)
		} else {
			c := mustBegin(t, tr)
			for edits := 1 + r.int(4); edits > 0; edits-- {
				mutate(c.State())
			}
			p = mustPrepare(t, c)
		}
		must(t, tr.Adopt(p))
		out = append(out, pendingCommit{ops: p.Ops(), value: tr.Value()})
	}
	return pendingSequence{initial: initial, commits: out}
}

// diffSequence is diffs between values, each an edit of the previous one or a
// new value. The edit runs through a draft over {"value": previous}.
func diffSequence(t *testing.T, r *pendingRandom, commits int) pendingSequence {
	initial := r.value(0)
	out := make([]pendingCommit, 0, commits)
	value := initial
	for range commits {
		tr := Track[any](map[string]any{"value": cloneJSON(value)})
		c := mustBegin(t, tr)
		for edits := 1 + r.int(3); edits > 0; edits-- {
			mutateAnywhere(t, c.State(), r, false)
		}
		edited := asObject(mustPrepare(t, c).Value())["value"]
		next := edited
		if r.next() < 0.2 {
			next = r.value(0)
		}
		out = append(out, pendingCommit{ops: DiffRevisions(value, next), value: next})
		value = next
	}
	return pendingSequence{initial: initial, commits: out}
}

// overTheWire is ops encoded, marshalled, unmarshalled and decoded, as a
// remote replica receives them.
func overTheWire(t *testing.T, enc *Encoder, dec *Decoder, ops []Op) []Op {
	t.Helper()
	data, err := json.Marshal(enc.Encode(ops))
	must(t, err)
	var raw []json.RawMessage
	must(t, json.Unmarshal(data, &raw))
	wire := make([]WireOp, len(raw))
	for i, m := range raw {
		wire[i], err = UnmarshalWireOp(m)
		must(t, err)
	}
	return decode(t, dec, wire)
}

// checkSequence feeds the sequence through one queue, taking at random points
// as a slow consumer would, and replays what was taken on a replica, once as
// ops and once over the wire. Both must reach the final value — the value the
// raw ops reach — and the received ops must be untouched.
func checkSequence(t *testing.T, s pendingSequence, r *pendingRandom) (ops, folded int) {
	t.Helper()
	batches := make([][]Op, len(s.commits))
	for i, c := range s.commits {
		batches[i] = c.ops
	}
	received := jsonText(t, batches)
	var queue PendingOps
	type taken struct {
		batch PendingTake
		value any
	}
	var takes []taken
	for i, c := range s.commits {
		queue.Push(c.ops)
		ops += len(c.ops)
		if r.next() < 0.1 || i == len(s.commits)-1 {
			takes = append(takes, taken{queue.Take(), c.value})
		}
	}
	final := s.initial
	if len(s.commits) > 0 {
		final = s.commits[len(s.commits)-1].value
	}
	var enc Encoder
	var dec Decoder
	replica := cloneJSON(s.initial)
	decoded, err := ApplyImmutable[any](nil, overTheWire(t, &enc, &dec, []Op{Replace{Value: cloneJSON(s.initial)}}))
	must(t, err)
	for _, tk := range takes {
		batch := tk.batch.Ops
		if tk.batch.Replace {
			batch = []Op{Replace{Value: tk.value}}
		}
		folded += len(batch)
		if replica, err = ApplyImmutable(replica, batch); err != nil {
			t.Fatalf("replay %s: %v", jsonText(t, batch), err)
		}
		if decoded, err = ApplyImmutable(decoded, overTheWire(t, &enc, &dec, batch)); err != nil {
			t.Fatalf("replay over the wire %s: %v", jsonText(t, batch), err)
		}
	}
	if g, w := jsonText(t, replica), jsonText(t, final); g != w {
		t.Fatalf("replica of the taken ops\n got %s\nwant %s\nreceived %s", g, w, received)
	}
	if g, w := jsonText(t, decoded), jsonText(t, final); g != w {
		t.Fatalf("replica over the wire\n got %s\nwant %s", g, w)
	}
	if g := jsonText(t, batches); g != received {
		t.Fatalf("the queue modified a received batch\n got %s\nwant %s", g, received)
	}
	return ops, folded
}

// ─── Folding is invisible to a replica ──────────────────────────────────────

func TestPendingOpsFoldingTrackerEditsAnywhere(t *testing.T) {
	r := newPendingRandom(1)
	ops, folded := 0, 0
	for range 800 {
		s := trackedSequence(t, r.root(), 1+r.int(30), r, func(d *Draft) { mutateAnywhere(t, d, r, true) }, r.root)
		o, f := checkSequence(t, s, r)
		ops, folded = ops+o, folded+f
	}
	// The sequences fold: a fuzzer whose ops never merge proves nothing.
	if float64(folded) >= float64(ops)*0.9 {
		t.Errorf("%d ops folded to %d: the queue does not fold", ops, folded)
	}
}

func TestPendingOpsFoldingListElements(t *testing.T) {
	r := newPendingRandom(2)
	ops, folded := 0, 0
	for range 800 {
		initial := map[string]any{"list": []any{map[string]any{"x": "a", "n": 0.0}, map[string]any{"x": "b", "n": 1.0}}}
		s := trackedSequence(t, initial, 5+r.int(40), r, func(d *Draft) { mutateList(t, d, r) }, func() any { return map[string]any{"list": []any{}} })
		o, f := checkSequence(t, s, r)
		ops, folded = ops+o, folded+f
	}
	if float64(folded) >= float64(ops)*0.9 {
		t.Errorf("%d ops folded to %d: the queue does not fold", ops, folded)
	}
}

func TestPendingOpsFoldingRootArray(t *testing.T) {
	r := newPendingRandom(3)
	values := func() any {
		items := make([]any, r.int(6))
		for i := range items {
			items[i] = r.value(0)
		}
		return items
	}
	for range 300 {
		checkSequence(t, trackedSequence(t, values(), 40, r, func(d *Draft) { mutateRootArray(t, d, r) }, values), r)
	}
}

func TestPendingOpsFoldingDiffRevisions(t *testing.T) {
	r := newPendingRandom(4)
	for range 500 {
		checkSequence(t, diffSequence(t, r, 30), r)
	}
}

// ─── Adversarial sequences ───────────────────────────────────────────────────

// foldOneByOne pushes each op as its own batch, takes, and checks the replay
// against the raw ops, in process and over the wire; it returns the folded
// ops.
func foldOneByOne(t *testing.T, initial string, raw []Op) []Op {
	t.Helper()
	var queue PendingOps
	for _, op := range raw {
		queue.Push([]Op{op})
	}
	batch := queue.Take()
	if batch.Replace {
		t.Fatal("unexpected overflow")
	}
	want, err := ApplyImmutable(tree(t, initial), raw)
	must(t, err)
	got, err := ApplyImmutable(tree(t, initial), batch.Ops)
	must(t, err)
	wantJSON(t, got, want)
	var enc Encoder
	var dec Decoder
	base, err := ApplyImmutable[any](nil, overTheWire(t, &enc, &dec, []Op{Replace{Value: tree(t, initial)}}))
	must(t, err)
	wire, err := ApplyImmutable(base, overTheWire(t, &enc, &dec, batch.Ops))
	must(t, err)
	wantJSON(t, wire, want)
	return batch.Ops
}

func wantLen(t *testing.T, got []Op, n int) {
	t.Helper()
	if len(got) != n {
		t.Errorf("folded to %s, want %d ops", jsonText(t, got), n)
	}
}

func TestPendingOpsAdversarial(t *testing.T) {
	t.Run("a leaf set in an array element, a splice before it, the same path set again", func(t *testing.T) {
		raw := ops(t, `[["s", ["arr", 2, "x"], 1], ["p", ["arr"], 0, 0, [{"x": 9}]], ["s", ["arr", 2, "x"], 2]]`)
		wantOps(t, foldOneByOne(t, `{"arr": [{"x": 0}, {"x": 0}, {"x": 0}]}`, raw), jsonText(t, raw))
		appends := ops(t, `[["a", ["arr", 0, "x"], "1"], ["p", ["arr"], 0, 0, [{"x": "new"}]], ["a", ["arr", 0, "x"], "2"]]`)
		wantOps(t, foldOneByOne(t, `{"arr": [{"x": "a"}, {"x": "b"}]}`, appends), jsonText(t, appends))
	})
	t.Run("t then a, and appends around a trim", func(t *testing.T) {
		wantLen(t, foldOneByOne(t, `{"s": "hello"}`, ops(t, `[["t", ["s"], 2], ["a", ["s"], "!"]]`)), 2)
		wantLen(t, foldOneByOne(t, `{"s": "hello"}`, ops(t, `[["a", ["s"], "AB"], ["t", ["s"], 3], ["a", ["s"], "C"]]`)), 3)
	})
	t.Run("an object s at a parent, then leaf sets below it", func(t *testing.T) {
		wantLen(t, foldOneByOne(t, `{"o": {"a": 1}}`, ops(t, `[["s", ["o", "a"], 2], ["s", ["o"], {"a": 5, "b": 6}], ["s", ["o", "a"], 7]]`)), 3)
		wantOps(t, foldOneByOne(t, `{"x": {"s": "a"}}`, ops(t, `[["s", ["x"], {"s": "b"}], ["a", ["x", "s"], "c"], ["s", ["x", "s"], "d"]]`)),
			`[["s", ["x"], {"s": "b"}], ["s", ["x", "s"], "d"]]`)
	})
	t.Run("an ancestor primitive s between leaf ops below it", func(t *testing.T) {
		wantOps(t, foldOneByOne(t, `{"o": {"a": "x", "b": "y"}}`, ops(t, `[["a", ["o", "a"], "1"], ["s", ["o", "b"], "q"], ["s", ["o"], "flat"], ["a", ["o"], "!"]]`)),
			`[["a", ["o", "a"], "1"], ["s", ["o", "b"], "q"], ["s", ["o"], "flat!"]]`)
		wantOps(t, foldOneByOne(t, `{"a": {"b": 0}}`, ops(t, `[["s", ["a", "b"], 1], ["s", ["a"], null], ["s", ["a"], "x"]]`)),
			`[["s", ["a", "b"], 1], ["s", ["a"], "x"]]`)
	})
	t.Run("deletes, moves and splices shift what a path names", func(t *testing.T) {
		wantLen(t, foldOneByOne(t, `{"arr": {"0": "a", "1": "b", "2": "c"}}`, ops(t, `[["s", ["arr", "2"], "C"], ["d", ["arr", "0"]], ["s", ["arr", "1"], "Z"]]`)), 3)
		wantLen(t, foldOneByOne(t, `{"arr": [1, 2, 3]}`, ops(t, `[["s", ["arr", 0], 10], ["m", ["arr"], [2, 1, 0]], ["s", ["arr", 0], 20]]`)), 3)
		wantLen(t, foldOneByOne(t, `{"arr": [{"x": 0}]}`, ops(t, `[["s", ["arr", 0, "x"], 1], ["p", ["arr"], 1, 0, [{"x": 0}]], ["s", ["arr", 0, "x"], 2], ["s", ["arr", 1, "x"], 3]]`)), 4)
	})
	t.Run("keys of one parent never collide", func(t *testing.T) {
		foldOneByOne(t, `{"a": {"b": "x"}, "a.b": "z"}`, ops(t, `[["a", ["a", "b"], "1"], ["s", ["a.b"], "y"], ["a", ["a", "b"], "2"]]`))
		foldOneByOne(t, `{"o": {"1": "a"}, "arr": ["x", "y"]}`, ops(t, `[["s", ["o", "1"], "b"], ["s", ["arr", 1], "z"], ["s", ["o", "1"], "c"], ["s", ["arr", 1], "w"]]`))
	})
	t.Run("an array set at the end appends, and a later set of that index folds into it", func(t *testing.T) {
		wantOps(t, foldOneByOne(t, `{"arr": [1]}`, ops(t, `[["s", ["arr", 1], 2], ["s", ["arr", 0], 9], ["s", ["arr", 1], 3]]`)),
			`[["s", ["arr", 1], 3], ["s", ["arr", 0], 9]]`)
	})
	t.Run("r drops everything before it", func(t *testing.T) {
		wantOps(t, foldOneByOne(t, `{"a": 1}`, ops(t, `[["s", ["a"], 2], ["r", {"b": "x"}], ["a", ["b"], "y"]]`)),
			`[["r", {"b": "x"}], ["a", ["b"], "y"]]`)
	})
	t.Run("joins appends and folds appends into a string set", func(t *testing.T) {
		wantOps(t, foldOneByOne(t, `{"s": ""}`, ops(t, `[["s", ["s"], "ab"], ["a", ["s"], "c"], ["s", ["t"], 1], ["a", ["s"], "d"]]`)),
			`[["s", ["s"], "abcd"], ["s", ["t"], 1]]`)
		// Upstream's split-surrogate case ("\ud83d" then "\ude00") has no Go
		// form: a Go string cannot hold a lone surrogate. Whole characters:
		wantOps(t, foldOneByOne(t, `{"s": ""}`, ops(t, `[["a", ["s"], "😀"], ["s", ["n"], 1], ["a", ["s"], "é"]]`)),
			`[["a", ["s"], "😀é"], ["s", ["n"], 1]]`)
	})
	t.Run("a set replaces appends joined before it, and appends join after it", func(t *testing.T) {
		// Not an upstream case: it pins the port's deferred joining (joined).
		wantOps(t, foldOneByOne(t, `{"s": ""}`, ops(t, `[["a", ["s"], "a"], ["a", ["s"], "b"], ["s", ["s"], "x"], ["s", ["n"], 1]]`)),
			`[["s", ["s"], "x"], ["s", ["n"], 1]]`)
		wantOps(t, foldOneByOne(t, `{"s": ""}`, ops(t, `[["a", ["s"], "a"], ["a", ["s"], "b"], ["s", ["s"], "x"], ["a", ["s"], "y"], ["a", ["s"], "z"]]`)),
			`[["s", ["s"], "xyz"]]`)
		// An "r" inside a batch: a batch starting with one never folds.
		var queue PendingOps
		queue.Push(ops(t, `[["s", ["n"], 1], ["a", ["s"], "a"]]`))
		queue.Push(ops(t, `[["a", ["s"], "b"], ["r", {"s": "", "l": 0}], ["s", ["l"], 1], ["a", ["s"], "c"]]`))
		wantOps(t, queue.Take().Ops, `[["r", {"l": 0, "s": ""}], ["s", ["l"], 1], ["a", ["s"], "c"]]`)
	})
	t.Run("interleaved leaves, a structural op, then the same leaves again", func(t *testing.T) {
		wantOps(t, foldOneByOne(t, `{"a": "", "b": "", "list": [0]}`, ops(t, `[["a", ["a"], "1"], ["a", ["b"], "1"], ["a", ["a"], "2"], ["p", ["list"], 1, 0, [1]], ["a", ["b"], "2"], ["a", ["a"], "3"]]`)),
			`[["a", ["a"], "12"], ["a", ["b"], "1"], ["p", ["list"], 1, 0, [1]], ["a", ["b"], "2"], ["a", ["a"], "3"]]`)
	})
	t.Run("a non-string primitive set takes no append", func(t *testing.T) {
		// Not a tracker sequence; merge's undefined branch: the append is
		// kept, and indexed in the set's place.
		wantOps(t, foldOneByOne(t, `{"s": 1}`, ops(t, `[["s", ["s"], 2], ["s", ["s"], "x"], ["a", ["s"], "y"]]`)),
			`[["s", ["s"], "xy"]]`)
		got := pendingTakeOf(t, ops(t, `[["s", ["s"], 2], ["a", ["s"], "y"], ["a", ["s"], "z"]]`))
		wantOps(t, got.Ops, `[["s", ["s"], 2], ["a", ["s"], "yz"]]`)
	})
}

// The upstream "deletes" case deletes an array element, which a decoded op
// cannot do here (Apply refuses a "d" on an array, as an overlay array has no
// holes); the port runs it on an object with the same keys.

// pendingTakeOf pushes each op as its own batch and takes, without a replay.
func pendingTakeOf(t *testing.T, raw []Op) PendingTake {
	t.Helper()
	var queue PendingOps
	for _, op := range raw {
		queue.Push([]Op{op})
	}
	return queue.Take()
}

// ─── The cap ─────────────────────────────────────────────────────────────────

// distinctSets is count set ops on distinct paths.
func distinctSets(count, from int) []Op {
	out := make([]Op, count)
	for i := range out {
		out[i] = Set{Path: Path{Key("k"), Index(from + i)}, Value: float64(i)}
	}
	return out
}

func wantTakeOps(t *testing.T, got PendingTake, n int) {
	t.Helper()
	if got.Replace || len(got.Ops) != n {
		t.Errorf("take: replace %v, %d ops; want %d ops", got.Replace, len(got.Ops), n)
	}
}

func wantReplace(t *testing.T, got PendingTake) {
	t.Helper()
	if !got.Replace || got.Ops != nil {
		t.Errorf("take: replace %v, %d ops; want the replacement", got.Replace, len(got.Ops))
	}
}

// sameBatch reports whether got is want itself, not a copy.
func sameBatch(got, want []Op) bool {
	return len(got) == len(want) && len(got) > 0 && unsafe.SliceData(got) == unsafe.SliceData(want)
}

func TestPendingOpsCap(t *testing.T) {
	half := MaxPendingOps / 2
	t.Run("keeps the cap and replaces past it", func(t *testing.T) {
		var queue PendingOps
		queue.Push([]Op{})
		queue.Push(distinctSets(MaxPendingOps, 0))
		wantTakeOps(t, queue.Take(), MaxPendingOps)

		queue.Push(distinctSets(half, 0))
		queue.Push(distinctSets(half, half))
		wantTakeOps(t, queue.Take(), MaxPendingOps)

		queue.Push(distinctSets(half, 0))
		queue.Push(distinctSets(half+1, half))
		wantReplace(t, queue.Take())
	})
	t.Run("counts ops after folding", func(t *testing.T) {
		var queue PendingOps
		for range 3 * MaxPendingOps {
			queue.Push([]Op{Append{Path: Path{Key("text")}, Text: "x"}})
		}
		got := queue.Take()
		if got.Replace || len(got.Ops) != 1 {
			t.Fatalf("take: replace %v, %d ops; want one append", got.Replace, len(got.Ops))
		}
		if a, ok := got.Ops[0].(Append); !ok || a.Text != strings.Repeat("x", 3*MaxPendingOps) {
			t.Errorf("take %s, want one append of every x", jsonText(t, got.Ops[0]))
		}
	})
	t.Run("accumulates nothing once replaced, until taken or a batch starting with r", func(t *testing.T) {
		var queue PendingOps
		queue.Push(distinctSets(1, 0))
		queue.Push(distinctSets(MaxPendingOps+1, 0))
		queue.Push(distinctSets(3, 0))
		wantReplace(t, queue.Take())
		wantOps(t, queue.Take().Ops, `[]`)

		queue.Push(distinctSets(1, 0))
		queue.Push(distinctSets(MaxPendingOps+1, 0))
		fresh := ops(t, `[["r", {"k": [0]}]]`)
		queue.Push(fresh)
		if got := queue.Take(); got.Replace || !sameBatch(got.Ops, fresh) {
			t.Errorf("take %s: not the r batch as received", jsonText(t, got.Ops))
		}
		queue.Push(distinctSets(1, 0))
		queue.Push(fresh)
		queue.Push(ops(t, `[["s", ["n"], 1]]`))
		wantOps(t, queue.Take().Ops, `[["r", {"k": [0]}], ["s", ["n"], 1]]`)
	})
	t.Run("hands one batch over as received, whatever its size", func(t *testing.T) {
		var queue PendingOps
		batch := distinctSets(MaxPendingOps+1, 0)
		queue.Push(batch)
		if got := queue.Take(); got.Replace || !sameBatch(got.Ops, batch) {
			t.Errorf("take: replace %v, %d ops: not the batch as received", got.Replace, len(got.Ops))
		}
		queue.Push(batch)
		queue.Push(ops(t, `[["s", ["n"], 1]]`))
		wantReplace(t, queue.Take())
	})
}

func TestPendingOpsOwnership(t *testing.T) {
	var queue PendingOps
	first := ops(t, `[["a", ["s"], "1"]]`)
	queue.Push(first)
	if got := queue.Take(); !sameBatch(got.Ops, first) {
		t.Errorf("take %s: not the batch as received", jsonText(t, got.Ops))
	}
	second := ops(t, `[["a", ["s"], "2"]]`)
	queue.Push(first)
	queue.Push(second)
	wantOps(t, queue.Take().Ops, `[["a", ["s"], "12"]]`)
	wantOps(t, first, `[["a", ["s"], "1"]]`)
	wantOps(t, second, `[["a", ["s"], "2"]]`)
}

func TestPendingOpsSize(t *testing.T) {
	var queue PendingOps
	wantSize := func(n int) {
		t.Helper()
		if got := queue.Size(); got != n {
			t.Errorf("Size() = %d, want %d", got, n)
		}
	}
	wantSize(0)
	queue.Push([]Op{})
	wantSize(0)
	queue.Push(ops(t, `[["a", ["text"], "a"], ["s", ["n"], 1]]`))
	wantSize(2)
	queue.Push(ops(t, `[["a", ["text"], "b"], ["s", ["n"], 2], ["p", ["list"], 0, 0, [1]]]`))
	wantSize(3)
	queue.Take()
	wantSize(0)

	queue.Push(distinctSets(MaxPendingOps, 0))
	wantSize(MaxPendingOps)
	queue.Push(ops(t, `[["s", ["n"], 1]]`))
	wantSize(1)
	wantReplace(t, queue.Take())
	wantSize(0)
}

// ─── Cost ────────────────────────────────────────────────────────────────────

// "computes ... none for one batch": a consumer that keeps up pays nothing.
// (Upstream counts JSON.stringify calls; the port counts allocations.)
func TestPendingOpsOneBatchCostsNothing(t *testing.T) {
	leaves := ops(t, `[["a", ["entries", 0, "text"], "a"], ["s", ["meta", "tokens"], 1]]`)
	var queue PendingOps
	if allocs := testing.AllocsPerRun(100, func() {
		queue.Push(leaves)
		queue.Take()
	}); allocs != 0 {
		t.Errorf("push and take of one batch: %v allocations, want none", allocs)
	}
}

// "keeps one op per leaf path however many commits write it" — and, as V8's
// ropes make it upstream, in time linear in the text: a slow consumer of a
// streamed answer must not pay O(length) per chunk.
func TestPendingOpsLeafPathsFoldInLinearCost(t *testing.T) {
	const commits = 50_000
	var queue PendingOps
	batches := make([][]Op, commits)
	for i := range batches {
		batches[i] = []Op{
			Append{Path: Path{Key("entries"), Index(0), Key("text")}, Text: "tok "},
			Set{Path: Path{Key("meta"), Key("tokens")}, Value: float64(i)},
		}
	}
	var got PendingTake
	bytes := allocatedBytes(func() {
		for _, b := range batches {
			queue.Push(b)
		}
		got = queue.Take()
	})
	wantJSON(t, got.Ops, []Op{
		Append{Path: Path{Key("entries"), Index(0), Key("text")}, Text: strings.Repeat("tok ", commits)},
		Set{Path: Path{Key("meta"), Key("tokens")}, Value: float64(commits - 1)},
	})
	// The text is 200 KB; joining it chunk by chunk with + copies 5 GB.
	if limit := uint64(64 << 20); bytes > limit {
		t.Errorf("folding %d commits allocated %d bytes, over %d: appends are joined in quadratic time", commits, bytes, limit)
	}
}

// ─── References ──────────────────────────────────────────────────────────────

//go:noinline
func pendingPayload(queue *PendingOps) (payload weak.Pointer[any], replaced weak.Pointer[Seg]) {
	big := []any{strings.Repeat("x", 1000)}
	replacedPath := Path{Key("n")}
	queue.Push([]Op{Set{Path: replacedPath, Value: 1.0}})
	queue.Push([]Op{Set{Path: Path{Key("n")}, Value: 2.0}, Set{Path: Path{Key("o")}, Value: big}})
	return weak.Make(&big[0]), weak.Make(&replacedPath[0])
}

func TestPendingOpsReleasesWhatItDropped(t *testing.T) {
	var queue PendingOps
	payload, replaced := pendingPayload(&queue)
	// The set of n replaced the first one: only its newest op is held.
	collected(t, replaced, "the op a later set of its path folded over")
	if payload.Value() == nil {
		t.Fatal("a pending op's payload was collected while pending")
	}
	queue.Take()
	collected(t, payload, "a payload taken from the queue")

	// Taken while the folding state holds the index: nothing stays behind.
	payload, _ = pendingPayload(&queue)
	queue.Push([]Op{Splice{Path: Path{Key("list")}, Index: 0, Remove: 0, Items: []any{1.0}}})
	queue.Take()
	collected(t, payload, "a payload taken from a folding queue")
}
