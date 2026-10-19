package delta

import (
	"runtime"
	"testing"
)

// jsonText is the JSON form of a value or a batch, as marshalJSON writes it:
// object keys sorted, no HTML escaping.
func jsonText(t *testing.T, v any) string {
	t.Helper()
	data, err := marshalJSON(v)
	if err != nil {
		t.Fatalf("marshal %#v: %v", v, err)
	}
	return string(data)
}

// wantOps compares a batch with tuples given as a JSON literal.
func wantOps(t *testing.T, got []Op, literal string) {
	t.Helper()
	if got == nil {
		t.Fatalf("got a nil batch; an empty batch is [] on the wire")
	}
	want := jsonText(t, tree(t, literal))
	if g := jsonText(t, got); g != want {
		t.Errorf("ops\n got %s\nwant %s", g, want)
	}
}

// allocatedBytes is how many heap bytes one call of fn allocates. Where
// upstream's complexity tests count calls or time, a copy shows here as its
// size. The test must not be parallel: the count is the whole process's.
func allocatedBytes(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// wantJSON compares two values by their JSON form.
func wantJSON(t *testing.T, got, want any) {
	t.Helper()
	if g, w := jsonText(t, got), jsonText(t, want); g != w {
		t.Errorf("\n got %s\nwant %s", g, w)
	}
}

// mustPanic runs fn and reports the panic value as an error, or fails.
func mustPanic(t *testing.T, fn func()) (err error) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected a panic")
		}
		e, ok := r.(error)
		if !ok {
			t.Fatalf("panic value %#v is not an error", r)
		}
		err = e
	}()
	fn()
	return nil
}

// cloneJSON deep-copies the containers of a JSON tree: upstream's
// structuredClone in the tests.
func cloneJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, c := range x {
			m[k] = cloneJSON(c)
		}
		return m
	case []any:
		return cloneItems(x)
	}
	return v
}

func cloneItems(items []any) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = cloneJSON(item)
	}
	return out
}

// mustTrack tracks a JSON literal.
func mustTrack(t *testing.T, literal string) *Tracker[any] {
	t.Helper()
	return Track(tree(t, literal))
}

// commit runs one change through a tracker — begin, mutate, prepare, adopt —
// and returns the batch it published.
func commit[T any](t *testing.T, tr *Tracker[T], mutate func(d *Draft)) []Op {
	t.Helper()
	c, err := tr.BeginChange()
	if err != nil {
		t.Fatalf("BeginChange: %v", err)
	}
	mutate(c.State())
	p, err := c.Prepare()
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := tr.Adopt(p); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	return p.Ops()
}

// must fails the test on a draft write's error.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// concat is `draft[key] += text`.
func concat(t *testing.T, d *Draft, key string, text string) {
	t.Helper()
	v, _ := d.Get(key)
	must(t, d.Set(key, v.(string)+text))
}
