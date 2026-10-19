package delta

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The oracle: testdata/upstream_delta.json, captured from pi's own
// packages/chord/src/delta by testdata/capture.mts (its header says how, and
// what each section is) at the sha its "sha" field names, under node v26.4.0.
// Every batch below is compared byte for byte with pi's, as encoding/json
// writes both. A tracker's batches are pi's own, in pi's order. The one
// normalization is DiffRevisions' order of an object's member ops, which the
// capture records in Go's enumeration order next to pi's own ("raw") — a Go
// map has no insertion order (docs/UPSTREAM.md, D69).

type goldenFile struct {
	SHA          string           `json:"sha"`
	Diffs        []goldenBatch    `json:"diffs"`
	Scenarios    []goldenScript   `json:"scenarios"`
	Generated    []goldenBatch    `json:"generated"`
	Fuzz         []goldenFuzzSeed `json:"fuzz"`
	Differential []goldenScript   `json:"differential"`
}

// goldenBatch is one recorded batch; a large one is recorded by hash.
type goldenBatch struct {
	Name      string                     `json:"name"`
	Gen       string                     `json:"gen"`
	Refs      map[string]json.RawMessage `json:"refs"`
	Before    json.RawMessage            `json:"before"`
	After     json.RawMessage            `json:"after"`
	Ops       json.RawMessage            `json:"ops"`
	Raw       json.RawMessage            `json:"raw"`
	OpsHash   string                     `json:"opsHash"`
	OpsBytes  int                        `json:"opsBytes"`
	OpsHead   string                     `json:"opsHead"`
	ValueHash string                     `json:"valueHash"`
}

type goldenScript struct {
	Name     string                     `json:"name"`
	Script   *int                       `json:"script"`
	Trackers map[string]json.RawMessage `json:"trackers"`
	Initial  json.RawMessage            `json:"initial"`
	Steps    []map[string]any           `json:"steps"`
	Results  []goldenResult             `json:"results"`
}

type goldenResult struct {
	goldenBatch
	Error   string          `json:"error"`
	Missing bool            `json:"missing"`
	Value   json.RawMessage `json:"value"`
	Noop    *bool           `json:"noop"`
}

type goldenFuzzSeed struct {
	Seed int    `json:"seed"`
	Hash string `json:"hash"`
}

var loadGolden = sync.OnceValues(func() (*goldenFile, error) {
	data, err := os.ReadFile("testdata/upstream_delta.json")
	if err != nil {
		return nil, err
	}
	var g goldenFile
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, err
	}
	return &g, nil
})

func golden(t *testing.T) *goldenFile {
	t.Helper()
	g, err := loadGolden()
	if err != nil {
		t.Fatalf("testdata/upstream_delta.json: %v (regenerate it with testdata/capture.mts)", err)
	}
	// A table that can become empty is not a test.
	if len(g.Diffs) == 0 || len(g.Scenarios) == 0 || len(g.Generated) == 0 || len(g.Fuzz) == 0 || len(g.Differential) == 0 {
		t.Fatalf("testdata/upstream_delta.json has an empty section: %d diffs, %d scenarios, %d generated, %d fuzz, %d differential",
			len(g.Diffs), len(g.Scenarios), len(g.Generated), len(g.Fuzz), len(g.Differential))
	}
	return g
}

func sha(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// checkBatch compares a Go batch with pi's, and reports whether pi's own
// member order differed from Go's.
func checkBatch(t *testing.T, where string, got []Op, want goldenBatch) (reordered bool) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: nil batch; an empty batch is []", where)
		return false
	}
	text := jsonText(t, got)
	switch {
	case want.Ops != nil:
		if text != string(want.Ops) {
			t.Errorf("%s: ops\n  go %s\n  pi %s", where, text, want.Ops)
			if want.Raw != nil {
				t.Logf("%s: pi's own member order: %s", where, want.Raw)
			}
		}
	case want.OpsHash != "":
		if h := sha(text); h != want.OpsHash {
			head := text[:min(len(text), 120)]
			t.Errorf("%s: ops hash %s (%d bytes), pi %s (%d bytes)\n  go %s\n  pi %s", where, h, len(text), want.OpsHash, want.OpsBytes, head, want.OpsHead)
		}
	default:
		t.Fatalf("%s: the golden records no batch", where)
	}
	return want.Raw != nil
}

// ─── Building values ─────────────────────────────────────────────────────────

// builder turns a golden value spec into a Go value: fresh containers as
// encoding/json makes them (an empty array has no capacity), with the
// capture's markers resolved.
type builder struct {
	t     *testing.T
	refs  map[string]json.RawMessage
	built map[string]any
	held  map[string]any
}

func (b *builder) build(spec any) any {
	switch x := spec.(type) {
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = b.build(v)
		}
		return out
	case map[string]any:
		if name, ok := x["$ref"].(string); ok {
			if v, ok := b.built[name]; ok {
				return v
			}
			var raw any
			if err := json.Unmarshal(b.refs[name], &raw); err != nil {
				b.t.Fatalf("ref %s: %v", name, err)
			}
			if b.built == nil {
				b.built = map[string]any{}
			}
			v := b.build(raw)
			if xs, ok := v.([]any); ok && len(xs) == 0 {
				// A shared array is one array in pi. An empty Go slice has an
				// identity only once it has capacity (value.go, anonymous).
				v = make([]any, 0, 1)
			}
			b.built[name] = v
			return v
		}
		if name, ok := x["$held"].(string); ok {
			return b.held[name]
		}
		if _, ok := x["$nan"]; ok {
			return math.NaN()
		}
		if _, ok := x["$inf"]; ok {
			return math.Inf(1)
		}
		if _, ok := x["$date"]; ok {
			return time.Unix(0, 0)
		}
		if _, ok := x["$cycle"]; ok {
			cyclic := map[string]any{}
			cyclic["self"] = cyclic
			return cyclic
		}
		if r, ok := x["$repeat"].([]any); ok {
			return strings.Repeat(r[0].(string), int(r[1].(float64)))
		}
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[k] = b.build(v)
		}
		return out
	}
	return spec
}

func (b *builder) raw(data json.RawMessage) any {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		b.t.Fatalf("golden value %s: %v", data, err)
	}
	return b.build(v)
}

// ─── Diffs ───────────────────────────────────────────────────────────────────

func TestGoldenDiffRevisions(t *testing.T) {
	g := golden(t)
	reordered := 0
	for _, c := range g.Diffs {
		var before, after any
		if c.Gen != "" {
			before, after = generatedDiff(t, c.Gen)
		} else {
			b := &builder{t: t, refs: c.Refs}
			before, after = b.raw(c.Before), b.raw(c.After)
		}
		ops := DiffRevisions(before, after)
		if checkBatch(t, c.Name, ops, c) {
			reordered++
		}
		if c.Gen != "" {
			continue
		}
		replica, err := ApplyImmutable(before, ops)
		if err != nil {
			t.Errorf("%s: replaying the batch: %v", c.Name, err)
			continue
		}
		wantJSON(t, normalizeZero(replica), normalizeZero(after))
	}
	t.Logf("%d diffs; pi's own member order differs from Go's in %d", len(g.Diffs), reordered)
}

// normalizeZero rewrites -0 as 0: a replica converges on the JSON value.
func normalizeZero(v any) any {
	switch x := v.(type) {
	case float64:
		if x == 0 {
			return 0.0
		}
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = normalizeZero(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = normalizeZero(item)
		}
		return out
	}
	return v
}

// generatedDiff builds a "gen" case's inputs exactly as capture.mts does.
func generatedDiff(t *testing.T, name string) (before, after any) {
	t.Helper()
	values := func(n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = obj("value", float64(i))
		}
		return out
	}
	switch {
	case strings.HasPrefix(name, "retained-payload-"):
		size, _ := strconv.Atoi(strings.TrimPrefix(name, "retained-payload-"))
		payload := strings.Repeat("x", size)
		var r []any
		for _, id := range []string{"a", "b", "c", "d", "e"} {
			r = append(r, obj("id", id, "payload", payload))
		}
		return obj("values", r), obj("values", []any{r[0], r[2], r[4]})
	case strings.HasPrefix(name, "narrow-"):
		parts := strings.Split(name, "-")
		size, _ := strconv.Atoi(parts[2])
		vs := values(size)
		switch parts[1] {
		case "push":
			return obj("values", vs), obj("values", append(slices.Clone(vs), obj("value", float64(size))))
		case "pop":
			return obj("values", vs), obj("values", slices.Clone(vs[:size-1]))
		case "middle":
			middle := size / 2
			return obj("values", vs), obj("values", slices.Concat(vs[:middle], vs[middle+1:]))
		}
	case name == "sparse-40000":
		rows := make([]any, 40_000)
		for i := range rows {
			rows[i] = obj("value", float64(i), "stable", obj("value", float64(i)))
		}
		sparse := slices.Clone(rows)
		for i := 100; i < len(sparse); i += 400 {
			sparse[i] = obj("value", float64(-i), "stable", rows[i].(map[string]any)["stable"])
		}
		return obj("values", rows), obj("values", sparse)
	case name == "reconstructed-1000":
		rows := make([]any, 1_000)
		for i := range rows {
			rows[i] = obj("value", float64(i), "label", fmt.Sprintf("row-%d", i))
		}
		edited := cloneItems(rows)
		edited[700].(map[string]any)["label"] = "changed"
		return obj("values", rows), obj("values", edited)
	case name == "unshift-500":
		rows := make([]any, 10_000)
		for i := range rows {
			rows[i] = obj("value", float64(i), "payload", strings.Repeat("x", 100))
		}
		inserted := make([]any, 500)
		for i := range inserted {
			inserted[i] = obj("value", float64(-i-1))
		}
		return obj("values", rows), obj("values", slices.Concat(inserted, rows))
	case name == "wide-object-20000":
		b, a := map[string]any{}, map[string]any{}
		for i := range 20_000 {
			b[fmt.Sprintf("field%d", i)] = 0.0
			a[fmt.Sprintf("field%d", i)] = 1.0
		}
		return b, a
	case name == "base-fallback-40000":
		zeros, ones := make([]any, 40_000), make([]any, 40_000)
		for i := range zeros {
			zeros[i], ones[i] = 0.0, 1.0
		}
		return obj("values", zeros), obj("values", ones)
	case strings.HasPrefix(name, "op-cap-"):
		n, _ := strconv.Atoi(strings.TrimPrefix(name, "op-cap-"))
		b, a := map[string]any{}, map[string]any{}
		for i := range n {
			b[strconv.FormatInt(int64(i), 36)] = 0.0
			a[strconv.FormatInt(int64(i), 36)] = 1.0
		}
		return b, a
	case strings.HasPrefix(name, "cost-"):
		parts := strings.Split(name, "-")
		l, _ := strconv.Atoi(parts[2])
		r := strings.Repeat
		pad := r("E", l)
		if parts[1] == "astral" {
			pad = r("😀", l)
		}
		switch parts[1] {
		case "s", "astral":
			return obj("k1", r("A", 40_000), "k2", r("B", 40_000), "k3", pad),
				obj("k1", r("C", 40_000), "k2", r("D", 40_000), "k3", pad)
		case "a":
			return obj("t1", "x", "t2", "y", "k3", pad), obj("t1", "x"+r("C", 40_000), "t2", "y"+r("D", 40_000), "k3", pad)
		case "t":
			return obj("t", r("Q", 100)+r("Z", 10), "k3", pad), obj("t", r("Z", 10)+r("C", 80_000), "k3", pad)
		case "p":
			return obj("v", []any{}, "k3", pad), obj("v", []any{r("C", 80_000)}, "k3", pad)
		case "m":
			v, reversed := make([]any, 20_000), make([]any, 20_000)
			for i := range v {
				v[i], reversed[len(v)-1-i] = float64(i%10), float64(i%10)
			}
			return obj("v", v, "k3", pad), obj("v", reversed, "k3", pad)
		case "d":
			return obj("k1", r("A", 40_000), "k2", r("B", 40_000), "d0", 0.0, "d1", 0.0, "d2", 0.0, "d3", 0.0, "d4", 0.0, "k3", pad),
				obj("k1", r("C", 40_000), "k2", r("D", 40_000), "k3", pad)
		case "index":
			v, appended := make([]any, 8_000), make([]any, 8_000)
			for i := range v {
				v[i], appended[i] = 0.0, 0.0
				if i%2 == 1 {
					v[i], appended[i] = "x", "xy"
				}
			}
			return obj("k3", pad, "v", v), obj("k3", pad, "v", appended)
		case "num", "null", "true", "false", "spell", "keys":
			m0, m1 := map[string]any{}, map[string]any{}
			for i := range 4_000 {
				key := fmt.Sprintf("k%04d", i)
				m0[key], m1[key] = 0.0, 1.0
			}
			padding := padOf(parts[1], l)
			return obj("m", m0, "pad", padding), obj("m", m1, "pad", padding)
		}
	case strings.HasPrefix(name, "threshold-"):
		n, _ := strconv.Atoi(strings.TrimPrefix(name, "threshold-"))
		return obj("t", "x"), obj("t", "x"+strings.Repeat("C", n))
	case strings.HasPrefix(name, "anchor-budget-"):
		n, _ := strconv.Atoi(strings.TrimPrefix(name, "anchor-budget-"))
		a := obj("id", "a")
		before, after := []any{"b0"}, []any{"c0"}
		for range n {
			before = append(before, a)
		}
		for range 500 {
			after = append(after, a)
		}
		return append(before, "b1"), append(after, "c1")
	case strings.HasPrefix(name, "semantic-cells-"):
		n, _ := strconv.Atoi(strings.TrimPrefix(name, "semantic-cells-"))
		k := map[string]any{}
		before, after := make([]any, n), make([]any, 256)
		for v := range before {
			before[v] = obj("k", k, "v", float64(v))
		}
		for v := range after {
			after[v] = obj("k", k, "v", float64(v+1_000))
		}
		return before, after
	}
	t.Fatalf("unknown generated case %q: add it to generatedDiff as capture.mts builds it", name)
	return nil, nil
}

// padOf is capture.mts's padOf: l values of one kind, whose cost per element
// decides where a "cost-<kind>" case flips.
func padOf(kind string, l int) any {
	switch kind {
	case "keys":
		m := make(map[string]any, l)
		for i := range l {
			m[fmt.Sprintf("é%d", i)] = 0.0
		}
		return m
	}
	spellings := []any{1e21, 1.5e-7, 1e-6, math.Copysign(0, -1), 123.456, -1e-7, float64(1 << 53), 0.1, 1e-7, 5e-324}
	xs := make([]any, l)
	for i := range xs {
		switch kind {
		case "num":
			xs[i] = 1e6
		case "null":
			xs[i] = nil
		case "true":
			xs[i] = true
		case "false":
			xs[i] = false
		case "spell":
			xs[i] = spellings[i%len(spellings)]
		}
	}
	return xs
}

// ─── Scripts ─────────────────────────────────────────────────────────────────

// runner is the Go interpreter of capture.mts's step language.
type runner struct {
	t          *testing.T
	trackers   map[string]*Tracker[any]
	changes    map[string]*Change[any]
	prepared   map[string]*Prepared[any]
	b          *builder
	hashValues bool
}

// outcome is one step's result on the Go side.
type outcome struct {
	err     error
	missing bool
	value   any
	hasVal  bool
	ops     []Op
	noop    bool
	prepare bool
}

func newRunner(t *testing.T, hashValues bool) *runner {
	return &runner{
		t:          t,
		trackers:   map[string]*Tracker[any]{},
		changes:    map[string]*Change[any]{},
		prepared:   map[string]*Prepared[any]{},
		b:          &builder{t: t, held: map[string]any{}},
		hashValues: hashValues,
	}
}

func name(step map[string]any, key string) string {
	if v, ok := step[key].(string); ok {
		return v
	}
	return "main"
}

func (r *runner) nav(step map[string]any) *Draft {
	var d *Draft
	if from, ok := step["from"].(string); ok {
		d, _ = r.b.held[from].(*Draft)
	} else if c := r.changes[name(step, "c")]; c != nil {
		d = c.State()
	}
	at, _ := step["at"].([]any)
	for _, seg := range at {
		if d == nil {
			// JavaScript's d[seg] on undefined.
			panic(undefinedRead(seg))
		}
		d = d.At(seg)
	}
	return d
}

// undefinedRead is the TypeError JavaScript throws for a property read on
// undefined — where a step's path leaves the tree, the Go draft chain meets a
// nil *Draft.
func undefinedRead(prop any) error {
	return fmt.Errorf("Cannot read properties of undefined (reading '%v')", prop)
}

// draft is nav for a step that reads prop from the draft it lands on.
func (r *runner) draft(step map[string]any, prop any) *Draft {
	d := r.nav(step)
	if d == nil {
		panic(undefinedRead(prop))
	}
	return d
}

// snapshot is a value read out of a draft as plain JSON: JSON.stringify of
// the proxy, which reads through it.
func snapshot(v any) any {
	switch x := v.(type) {
	case *Draft:
		if x.IsArray() {
			out := make([]any, x.Len())
			for i := range out {
				item, _ := x.Get(i)
				out[i] = snapshot(item)
			}
			return out
		}
		out := map[string]any{}
		for _, k := range x.Keys() {
			item, _ := x.Get(k)
			out[k] = snapshot(item)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = snapshot(item)
		}
		return out
	}
	return v
}

func (r *runner) exec(step map[string]any) (o outcome) {
	defer func() {
		// A read through a settled draft panics where pi throws, and so does a
		// step that reads a property of undefined. A runtime error is a bug in
		// the port, not an outcome to compare.
		if p := recover(); p != nil {
			err, ok := p.(error)
			if _, crashed := p.(runtime.Error); !ok || crashed {
				panic(p)
			}
			o = outcome{err: err}
		}
	}()
	value := func() any { return r.b.build(step["value"]) }
	items := func() []any {
		raw, _ := step["items"].([]any)
		out := make([]any, len(raw))
		for i, v := range raw {
			out[i] = r.b.build(v)
		}
		return out
	}
	num := func(key string) int { return int(step[key].(float64)) }
	key := step["key"]
	result := func(v any, ok bool) outcome {
		if !ok {
			return outcome{missing: true}
		}
		return outcome{value: snapshot(v), hasVal: true}
	}
	prepared := func(p *Prepared[any], err error) outcome {
		if err != nil {
			return outcome{err: err}
		}
		r.prepared[name(step, "p")] = p
		return outcome{prepare: true, ops: p.Ops(), noop: same(p.Value(), p.Base()), value: p.Value(), hasVal: true}
	}
	switch step["do"] {
	case "begin":
		c, err := r.trackers[name(step, "t")].BeginChange()
		if err == nil {
			r.changes[name(step, "c")] = c
		}
		return outcome{err: err}
	case "prepare":
		return prepared(r.changes[name(step, "c")].Prepare())
	case "replace":
		return prepared(r.trackers[name(step, "t")].PrepareReplace(value()))
	case "adopt":
		return outcome{err: r.trackers[name(step, "t")].Adopt(r.prepared[name(step, "p")])}
	case "abort":
		r.changes[name(step, "c")].Abort()
		return outcome{}
	case "abortPrepared":
		r.prepared[name(step, "p")].Abort()
		return outcome{}
	case "baseRevision":
		return outcome{value: r.prepared[name(step, "p")].BaseRevision(), hasVal: true}
	case "revision":
		return outcome{value: r.trackers[name(step, "t")].Revision(), hasVal: true}
	case "value":
		return outcome{value: r.trackers[name(step, "t")].Value(), hasVal: true}
	case "hold":
		d := r.nav(step)
		r.b.held[step["as"].(string)] = d
		return outcome{}
	case "shared":
		p := r.prepared[name(step, "p")]
		value, base := p.Value(), p.Base()
		at, _ := step["at"].([]any)
		for _, seg := range at {
			value, base = member(value, seg), member(base, seg)
		}
		return outcome{value: same(value, base), hasVal: true}
	case "keys":
		return outcome{value: r.nav(step).Keys(), hasVal: true}
	case "get":
		return result(r.draft(step, key).Get(key))
	case "set":
		return outcome{err: r.draft(step, key).Set(key, value())}
	case "append":
		d := r.draft(step, key)
		current, _ := d.Get(key)
		return outcome{err: d.Set(key, jsConcat(current, step["text"].(string)))}
	case "delete":
		return outcome{err: r.draft(step, key).Delete(key)}
	case "push", "unshift":
		d := r.nav(step)
		method := d.Push
		if step["do"] == "unshift" {
			method = d.Unshift
		}
		n, err := method(items()...)
		if err != nil {
			return outcome{err: err}
		}
		return outcome{value: n, hasVal: true}
	case "pop", "shift":
		d := r.nav(step)
		method := d.Pop
		if step["do"] == "shift" {
			method = d.Shift
		}
		v, ok, err := method()
		if err != nil {
			return outcome{err: err}
		}
		if as, isHeld := step["as"].(string); isHeld {
			r.b.held[as] = v
		}
		return result(v, ok)
	case "splice":
		removed, err := r.nav(step).Splice(num("start"), num("deleteCount"), items()...)
		if err != nil {
			return outcome{err: err}
		}
		return result(removed, true)
	case "reverse":
		return outcome{err: r.nav(step).Reverse()}
	case "sort":
		d := r.nav(step)
		return outcome{err: d.Sort(r.comparator(d, step))}
	case "fill":
		return outcome{err: r.nav(step).Fill(value(), num("start"), num("end"))}
	case "copyWithin":
		return outcome{err: r.nav(step).CopyWithin(num("target"), num("start"), num("end"))}
	case "setLength":
		return outcome{err: r.nav(step).SetLen(num("length"))}
	}
	r.t.Fatalf("unknown step %v: add it to runner.exec as capture.mts runs it", step["do"])
	return outcome{}
}

// member is v[seg] for a revision: an object's member or an array's element.
func member(v, seg any) any {
	switch x := v.(type) {
	case map[string]any:
		return x[seg.(string)]
	case []any:
		return x[int(seg.(float64))]
	}
	return nil
}

// jsConcat is `current + text` for the string members the scripts append to.
func jsConcat(current any, text string) any {
	s, ok := current.(string)
	if !ok {
		return current
	}
	return s + text
}

// comparator is the step's sort comparator: nil for the default order, else
// (field(l) - field(r)) * direction, bumping a counter on both sides first,
// writing over an element of the array being sorted, or unshifting into it
// on the first call.
func (r *runner) comparator(array *Draft, step map[string]any) func(a, b any) int {
	by, ok := step["by"].(string)
	if !ok {
		return nil
	}
	direction := 1.0
	if step["desc"] == true {
		direction = -1
	}
	bump, _ := step["bump"].(string)
	write, _ := step["write"].([]any)
	unshiftOnce, hasUnshift := step["unshiftOnce"].([]any)
	unshifted := false
	field := func(v any) float64 {
		if by == "." {
			return v.(float64)
		}
		f, _ := v.(*Draft).Get(by)
		return f.(float64)
	}
	return func(a, b any) int {
		if bump != "" {
			for _, side := range []any{a, b} {
				d := side.(*Draft)
				n, _ := d.Get(bump)
				must(r.t, d.Set(bump, n.(float64)+1))
			}
		}
		if write != nil {
			must(r.t, array.Set(write[0], r.b.build(write[1])))
		}
		if hasUnshift && !unshifted {
			unshifted = true
			items := make([]any, len(unshiftOnce))
			for i, item := range unshiftOnce {
				items[i] = r.b.build(item)
			}
			_, err := array.Unshift(items...)
			must(r.t, err)
		}
		switch diff := (field(a) - field(b)) * direction; {
		case diff < 0:
			return -1
		case diff > 0:
			return 1
		}
		return 0
	}
}

// check compares one step's outcome with pi's.
func (r *runner) check(where string, step map[string]any, got outcome, want goldenResult) (reordered bool) {
	t := r.t
	t.Helper()
	if want.Error != "" {
		if got.err == nil {
			t.Errorf("%s: %v succeeded; pi threw %q", where, step["do"], want.Error)
		} else if !strings.Contains(got.err.Error(), want.Error) {
			t.Errorf("%s: %v error %q does not carry pi's text %q", where, step["do"], got.err, want.Error)
		}
		return false
	}
	if got.err != nil {
		t.Errorf("%s: %v failed: %v (pi succeeded)", where, step["do"], got.err)
		return false
	}
	if want.Missing != got.missing {
		t.Errorf("%s: %v missing = %v, pi %v", where, step["do"], got.missing, want.Missing)
	}
	if got.prepare {
		reordered = checkBatch(t, where, got.ops, want.goldenBatch)
		if want.Noop == nil {
			t.Errorf("%s: noop = %v, pi recorded none", where, got.noop)
		} else if *want.Noop != got.noop {
			t.Errorf("%s: noop = %v, pi %v", where, got.noop, *want.Noop)
		}
	}
	switch {
	case want.ValueHash != "":
		if h := sha(jsonText(t, got.value)); h != want.ValueHash {
			t.Errorf("%s: %v value hash differs\n  go %s", where, step["do"], jsonText(t, got.value))
		}
	case want.Value != nil:
		if g := jsonText(t, got.value); g != string(want.Value) {
			t.Errorf("%s: %v value\n  go %s\n  pi %s", where, step["do"], g, want.Value)
		}
	case got.hasVal && !got.prepare && !got.missing:
		t.Errorf("%s: %v returned %s; pi recorded no value", where, step["do"], jsonText(t, got.value))
	}
	return reordered
}

func (r *runner) run(label string, s goldenScript) (reordered int) {
	if len(s.Steps) != len(s.Results) {
		r.t.Fatalf("%s: %d steps, %d results", label, len(s.Steps), len(s.Results))
	}
	for i, step := range s.Steps {
		where := fmt.Sprintf("%s step %d (%v)", label, i, step["do"])
		if r.check(where, step, r.exec(step), s.Results[i]) {
			reordered++
		}
	}
	return reordered
}

func TestGoldenScenarios(t *testing.T) {
	g := golden(t)
	for _, s := range g.Scenarios {
		r := newRunner(t, false)
		for tname, initial := range s.Trackers {
			r.trackers[tname] = Track(r.b.raw(initial))
		}
		r.run(s.Name, s)
	}
}

// Every scenario and script replayed many times: a Go map iterates in a
// randomized order, so an emission that followed one would match pi only by
// chance.
func TestGoldenScenariosAreDeterministic(t *testing.T) {
	g := golden(t)
	for range 10 {
		for _, s := range g.Scenarios {
			r := newRunner(t, false)
			for tname, initial := range s.Trackers {
				r.trackers[tname] = Track(r.b.raw(initial))
			}
			r.run(s.Name, s)
		}
		for _, s := range g.Differential {
			r := newRunner(t, true)
			r.trackers["main"] = Track(r.b.raw(s.Initial))
			r.run(fmt.Sprintf("script %d", *s.Script), s)
		}
		if t.Failed() {
			return
		}
	}
}

// state-fuzz.test.ts, differential: the same seeds and steps, each step's ops
// and value hashed as pi's were.
func TestGoldenFuzz(t *testing.T) {
	g := golden(t)
	for _, want := range g.Fuzz {
		if got := fuzzSeed(t, want.Seed); got != want.Hash {
			t.Errorf("fuzz seed %d: ops and values differ from pi's", want.Seed)
		}
	}
}

// mulberry32 is state-fuzz.test.ts's random(seed), in int32 arithmetic.
func mulberry32(seed int32) func() float64 {
	return func() float64 {
		seed += 0x6d2b79f5
		v := int32(uint32(seed^int32(uint32(seed)>>15)) * uint32(1|seed))
		v = int32(uint32(v)+uint32(int32(uint32(v^int32(uint32(v)>>7))*uint32(61|v)))) ^ v
		return float64(uint32(v^int32(uint32(v)>>14))) / 4_294_967_296
	}
}

func fuzzSeed(t *testing.T, seed int) string {
	t.Helper()
	rng := mulberry32(int32(seed))
	initial := map[string]any{"text": "start", "meta": map[string]any{"revision": 0.0}}
	items := make([]any, 4)
	for id := range items {
		items[id] = map[string]any{"id": float64(id), "text": fmt.Sprintf("item-%d", id), "score": 0.0}
	}
	initial["items"] = items
	tr := Track[map[string]any](initial)
	replica := cloneJSON(tr.Value())
	var err error
	stream := sha256.New()
	for step := range 100 {
		choice := int(math.Floor(rng() * 14))
		value := seed*1_000 + step
		ops := commit(t, tr, func(d *Draft) { fuzzMutate(t, d, choice, value) })
		if replica, err = ApplyImmutable[any](replica, ops); err != nil {
			t.Fatalf("seed %d step %d: %v", seed, step, err)
		}
		wantJSON(t, replica, tr.Value())
		fmt.Fprintf(stream, "%s\n%s\n", jsonText(t, ops), jsonText(t, tr.Value()))
	}
	return hex.EncodeToString(stream.Sum(nil))
}

// fuzzMutate is state-fuzz.test.ts's mutate on a draft.
func fuzzMutate(t *testing.T, d *Draft, choice, value int) {
	t.Helper()
	item := func() any {
		return map[string]any{"id": float64(value), "text": fmt.Sprintf("item-%d", value), "score": float64(value % 7)}
	}
	items := d.At("items")
	n := items.Len()
	switch choice {
	case 0:
		concat(t, d, "text", fmt.Sprintf("-%d", value))
	case 1:
		text, _ := d.Get("text")
		s := text.(string)
		must(t, d.Set("text", s[min(2, len(s)):]+strconv.Itoa(value)))
	case 2:
		_, err := items.Push(item())
		must(t, err)
	case 3:
		_, err := items.Unshift(item())
		must(t, err)
	case 4:
		if n > 0 {
			_, _, err := items.Shift()
			must(t, err)
		}
	case 5:
		if n > 0 {
			_, _, err := items.Pop()
			must(t, err)
		}
	case 6:
		index, remove := 0, 0
		if n > 0 {
			index, remove = value%(n+1), value%2
		}
		_, err := items.Splice(index, remove, item())
		must(t, err)
	case 7:
		must(t, items.Reverse())
	case 8:
		must(t, items.Sort(func(a, b any) int {
			l, _ := a.(*Draft).Get("id")
			r, _ := b.(*Draft).Get("id")
			return int(l.(float64) - r.(float64))
		}))
	case 9:
		if n > 0 {
			must(t, items.At(value%n).Set("score", float64(value)))
		}
	case 10:
		meta := d.At("meta")
		revision, _ := meta.Get("revision")
		must(t, meta.Set("revision", revision.(float64)+1))
		must(t, meta.Set("label", fmt.Sprintf("revision-%d", value)))
	case 11:
		must(t, d.At("meta").Delete("label"))
	case 12:
		if n > 1 {
			must(t, items.Set(1, items.At(0)))
		}
	default:
		for index := range min(2, n) {
			must(t, items.Set(index, item()))
		}
	}
}

// Random scripts over every draft operation, generated by capture.mts against
// pi's live draft and replayed here step for step.
func TestGoldenDifferential(t *testing.T) {
	g := golden(t)
	steps := 0
	for _, s := range g.Differential {
		r := newRunner(t, true)
		r.trackers["main"] = Track(r.b.raw(s.Initial))
		r.run(fmt.Sprintf("script %d", *s.Script), s)
		steps += len(s.Steps)
	}
	t.Logf("%d scripts, %d steps", len(g.Differential), steps)
}

// The tracker cases whose inputs are too large to record, built as
// capture.mts builds them.
func TestGoldenGenerated(t *testing.T) {
	g := golden(t)
	for _, want := range g.Generated {
		var p *Prepared[map[string]any]
		if want.Name == "prepareReplace-rows-10000" {
			rows := make([]any, 10_000)
			for i := range rows {
				rows[i] = obj("value", float64(i), "stable", obj("value", float64(i)))
			}
			tr := Track(obj("rows", rows))
			replacement := slices.Clone(tr.Value()["rows"].([]any))
			replacement[5_000] = obj("value", -1.0, "stable", replacement[5_000].(map[string]any)["stable"])
			var err error
			if p, err = tr.PrepareReplace(obj("rows", replacement)); err != nil {
				t.Fatalf("%s: %v", want.Name, err)
			}
		} else {
			initial, edit := generatedChange(t, want.Name)
			tr := Track(initial)
			c, err := tr.BeginChange()
			if err != nil {
				t.Fatalf("%s: %v", want.Name, err)
			}
			edit(c.State())
			if p, err = c.Prepare(); err != nil {
				t.Fatalf("%s: %v", want.Name, err)
			}
		}
		checkBatch(t, want.Name, p.Ops(), want)
		if h := sha(jsonText(t, p.Value())); h != want.ValueHash {
			t.Errorf("%s: value differs from pi's", want.Name)
		}
		replica, err := ApplyImmutable(p.Base(), p.Ops())
		if err != nil {
			t.Fatalf("%s: replaying: %v", want.Name, err)
		}
		wantJSON(t, replica, p.Value())
	}
}

func obj(kv ...any) map[string]any {
	m := make(map[string]any, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

// generatedChange is a "generated" case's root and edit, as capture.mts's
// generate builds them.
func generatedChange(t *testing.T, name string) (map[string]any, func(d *Draft)) {
	t.Helper()
	numbers := func(n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = float64(i)
		}
		return out
	}
	rows := func(n int, row func(i int) any) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = row(i)
		}
		return out
	}
	value := func(i int) any { return obj("value", float64(i)) }
	add := func(d *Draft, key string, delta float64) {
		v, _ := d.Get(key)
		must(t, d.Set(key, v.(float64)+delta))
	}
	switch name {
	case "draft-unshift-100000", "draft-splice-100000":
		return obj("values", []any{-1.0}), func(d *Draft) {
			var err error
			if name == "draft-unshift-100000" {
				_, err = d.At("values").Unshift(numbers(100_000)...)
			} else {
				_, err = d.At("values").Splice(1, 0, numbers(100_000)...)
			}
			must(t, err)
		}
	case "dense-rows-1000":
		return obj("rows", rows(1_000, value)), func(d *Draft) {
			rs := d.At("rows")
			for i := range rs.Len() {
				add(rs.At(i), "value", 1)
			}
		}
	case "dense-values-600":
		return obj("values", numbers(1_000)), func(d *Draft) {
			for i := range 600 {
				must(t, d.At("values").Set(i, float64(-i-1)))
			}
		}
	case "dense-nested-2000":
		return obj("rows", rows(2_000, func(i int) any { return obj("nested", value(i)) })), func(d *Draft) {
			rs := d.At("rows")
			must(t, rs.At(0).At("nested").Set("value", -1.0))
			for i := 500; i < 1_000; i++ {
				must(t, rs.At(i).At("nested").Set("value", float64(-i)))
			}
		}
	case "dense-reserved-400":
		rs := rows(400, func(int) any { return obj("flag", 0.0) })
		rs[100].(map[string]any)["special"] = obj("__proto__", obj("values", numbers(400)))
		return obj("rows", rs), func(d *Draft) {
			rs := d.At("rows")
			for i := range 256 {
				must(t, rs.At(i).Set("flag", 1.0))
			}
			values := rs.At(100).At("special").At("__proto__").At("values")
			for i := range 256 {
				must(t, values.Set(i, float64(-i-1)))
			}
		}
	case "dense-covered-400":
		rs := rows(400, func(i int) any {
			if i == 100 {
				return obj("flag", 0.0, "values", rows(400, value))
			}
			return obj("flag", 0.0, "values", []any{})
		})
		return obj("rows", rs), func(d *Draft) {
			rs := d.At("rows")
			for i := range 256 {
				must(t, rs.At(i).Set("flag", 1.0))
			}
			values := rs.At(100).At("values")
			for i := range 256 {
				must(t, values.At(i).Set("value", float64(-i-1)))
			}
			_, err := values.Push(obj("value", 999.0))
			must(t, err)
		}
	case "dense-disjoint-1400":
		return obj("values", rows(1_400, value)), func(d *Draft) {
			vs := d.At("values")
			for _, i := range []int{97, 358, 897, 1_158} {
				must(t, vs.At(i).Set("value", float64(-i-1)))
			}
			for i := 100; i < 356; i++ {
				must(t, vs.At(i).Set("value", float64(-i-1)))
			}
			for i := 900; i < 1_156; i++ {
				must(t, vs.At(i).Set("value", float64(-i-1)))
			}
		}
	case "queue-20000":
		return obj("values", rows(20_000, value)), func(d *Draft) {
			vs := d.At("values")
			for i := range 10_000 {
				_, _, err := vs.Shift()
				must(t, err)
				_, err = vs.Push(obj("value", float64(20_000+i)))
				must(t, err)
			}
		}
	case "fragmentation-20000":
		return obj("values", rows(20_000, value)), func(d *Draft) {
			vs := d.At("values")
			var held []*Draft
			for _, i := range []int{1, 1_001, 5_001, 10_001, 15_001, 19_999} {
				held = append(held, vs.At(i))
			}
			for i := 0; i < 20_000; i += 2 {
				_, err := vs.Splice(i, 1, obj("value", float64(-i-1)))
				must(t, err)
			}
			for _, h := range held {
				add(h, "value", 100_000)
			}
		}
	case "null-overrides-10000":
		return obj("values", numbers(10_000)), func(d *Draft) {
			must(t, d.At("values").Set(17, nil))
			must(t, d.At("values").Set(9_000, nil))
		}
	case "append-10000":
		return obj("values", numbers(10_000)), func(d *Draft) {
			_, err := d.At("values").Push(-1.0)
			must(t, err)
		}
	case "op-cap-object-4096", "op-cap-object-4097":
		n, _ := strconv.Atoi(strings.TrimPrefix(name, "op-cap-object-"))
		initial := map[string]any{}
		for i := range n {
			initial[strconv.FormatInt(int64(i), 36)] = 0.0
		}
		return initial, func(d *Draft) {
			for i := range n {
				must(t, d.Set(strconv.FormatInt(int64(i), 36), 1.0))
			}
		}
	case "op-cap-array-4096", "op-cap-array-4097":
		n, _ := strconv.Atoi(strings.TrimPrefix(name, "op-cap-array-"))
		v := rows(n, func(int) any { return obj("x", 0.0) })
		w := rows(n, func(int) any { return 0.0 })
		return obj("v", v, "w", w), func(d *Draft) {
			must(t, d.At("v").Reverse())
			for i := range n {
				x := 0.0
				if i%3 == 0 {
					x = 1
				}
				must(t, d.At("w").Set(i, x))
			}
		}
	}
	t.Fatalf("unknown generated case %q: add it to generatedChange as capture.mts builds it", name)
	return nil, nil
}
