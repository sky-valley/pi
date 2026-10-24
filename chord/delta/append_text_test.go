package delta

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// delta-append-text.test.ts (upstream da412b039, #9265): streaming writers
// appended to growing strings by assignment, and the tracker's prefix check
// read the whole string on every change. testdata/upstream_delta.json pins the
// same cases' ops, values and error text against pi (the "appendText ..."
// scenarios).

const appendTextDoc = `{"title": "t", "content": [{"type": "text", "text": "hello", "tags": ["a"]}], "meta": {"note": "n"}}`

// commitReplayed is the upstream test's commit: one change, adopted, whose
// ops replayed over the base give the new value.
func commitReplayed(t *testing.T, tr *Tracker[any], mutate func(d *Draft)) []Op {
	t.Helper()
	base := tr.Value()
	c := mustBegin(t, tr)
	mutate(c.State())
	p := mustPrepare(t, c)
	replayed, err := ApplyImmutable(base, p.Ops())
	if err != nil {
		t.Fatalf("replay %v: %v", p.Ops(), err)
	}
	wantJSON(t, replayed, p.Value())
	must(t, tr.Adopt(p))
	return p.Ops()
}

func block0(d *Draft) *Draft { return d.At("content").At(0) }

func TestAppendTextRecordsAnAppendOp(t *testing.T) {
	tr := mustTrack(t, appendTextDoc)
	before := tr.Value()
	wantOps(t, commitReplayed(t, tr, func(d *Draft) { must(t, block0(d).AppendText("text", " world")) }),
		`[["a", ["content", 0, "text"], " world"]]`)
	wantJSON(t, member(member(member(before, "content"), 0.0), "text"), "hello")
	wantOps(t, commitReplayed(t, tr, func(d *Draft) { must(t, d.AppendText("title", "!")) }),
		`[["a", ["title"], "!"]]`)
	wantOps(t, commitReplayed(t, tr, func(d *Draft) { must(t, d.At("meta").AppendText("note", "ote")) }),
		`[["a", ["meta", "note"], "ote"]]`)
	wantJSON(t, tr.Value(), tree(t, `{"title": "t!", "content": [{"type": "text", "text": "hello world", "tags": ["a"]}], "meta": {"note": "note"}}`))
}

func TestAppendTextJoinsAppendsToOneKey(t *testing.T) {
	tr := mustTrack(t, appendTextDoc)
	ops := commitReplayed(t, tr, func(d *Draft) {
		must(t, block0(d).AppendText("text", " a"))
		must(t, d.Set("title", "new"))
		must(t, block0(d).AppendText("text", " b"))
		must(t, block0(d).AppendText("text", ""))
		must(t, block0(d).AppendText("text", " c"))
	})
	// Upstream asserts membership and length; the order is the change's.
	wantOps(t, ops, `[["s", ["title"], "new"], ["a", ["content", 0, "text"], " a b c"]]`)
	wantJSON(t, member(member(member(tr.Value(), "content"), 0.0), "text"), "hello a b c")
}

func TestAppendTextFallsBackWhenTheKeyIsAlsoWritten(t *testing.T) {
	tr := mustTrack(t, appendTextDoc)
	wantOps(t, commitReplayed(t, tr, func(d *Draft) {
		must(t, block0(d).AppendText("text", " x"))
		concat(t, block0(d), "text", " y")
	}), `[["a", ["content", 0, "text"], " x y"]]`)
	wantOps(t, commitReplayed(t, tr, func(d *Draft) {
		must(t, block0(d).AppendText("text", " z"))
		must(t, block0(d).Set("text", "replaced"))
	}), `[["s", ["content", 0, "text"], "replaced"]]`)
	wantOps(t, commitReplayed(t, tr, func(d *Draft) {
		must(t, d.Set("title", "assigned"))
		must(t, d.AppendText("title", " then appended"))
	}), `[["s", ["title"], "assigned then appended"]]`)
	wantOps(t, commitReplayed(t, tr, func(d *Draft) {
		must(t, d.At("meta").AppendText("note", "x"))
		must(t, d.At("meta").Delete("note"))
	}), `[["d", ["meta", "note"]]]`)
}

func TestAppendTextInsideAPlacedBlock(t *testing.T) {
	tr := mustTrack(t, appendTextDoc)
	commitReplayed(t, tr, func(d *Draft) {
		content := d.At("content")
		_, err := content.Push(tree(t, `{"type": "text", "text": "new", "tags": []}`))
		must(t, err)
		must(t, content.At(1).AppendText("text", " block"))
		must(t, content.Set(0, tree(t, `{"type": "text", "text": "other", "tags": []}`)))
		must(t, content.At(0).AppendText("text", " one"))
	})
	content := member(tr.Value(), "content")
	wantJSON(t, []any{member(member(content, 0.0), "text"), member(member(content, 1.0), "text")}, []any{"other one", "new block"})
}

// "rejects what a JavaScript caller can get wrong" and "accepts only keys
// whose type is exactly string": the runtime half. A non-draft cannot reach a
// *Draft method; a nil *Draft is refused as every draft method refuses one.
// StringKeys<T> is a compile-time type with no Go counterpart.
func TestAppendTextRefusals(t *testing.T) {
	tr := mustTrack(t, appendTextDoc)
	var none *Draft
	if err := none.AppendText("text", "x"); !errors.Is(err, errNilDraft) {
		t.Errorf("nil draft: %v, want errNilDraft", err)
	}
	c := mustBegin(t, tr)
	for _, tc := range []struct {
		d   *Draft
		key string
	}{
		{c.State(), "content"},
		{c.State(), "missing"},
		{c.State().At("content"), "0"},
		{block0(c.State()), "signature"},
		{block0(c.State()), "tags"},
		{c.State(), `a"b<é>`},
	} {
		want := fmt.Sprintf("appendText() needs a string at %s", jsonText(t, tc.key))
		if err := tc.d.AppendText(tc.key, "x"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("AppendText(%q): %v, want an error carrying %q", tc.key, err, want)
		}
	}
	// A string member whose TypeScript type is a literal is still a string.
	must(t, block0(c.State()).AppendText("type", "x"))
	// The checks come before the no-op of empty text.
	if err := c.State().AppendText("missing", ""); err == nil {
		t.Error("AppendText of empty text to a missing key succeeded")
	}

	block := block0(c.State())
	mustPrepare(t, c)
	if err := block.AppendText("text", "x"); !errors.Is(err, ErrDraftSettled) {
		t.Errorf("after Prepare: %v, want ErrDraftSettled", err)
	}
	aborted := mustBegin(t, tr)
	state := aborted.State()
	aborted.Abort()
	if err := state.AppendText("title", "x"); !errors.Is(err, ErrDraftSettled) {
		t.Errorf("after Abort: %v, want ErrDraftSettled", err)
	}
}

// "keeps an op text that does not hold a larger string it was cut from
// alive": neither the op nor the new value shares the caller's bytes — not
// even when the member was empty, where Go's "" + text would return text
// itself.
func TestAppendTextOwnsItsText(t *testing.T) {
	for _, initial := range []string{"", "hello"} {
		tr := Track[any](map[string]any{"text": initial})
		for i := range 5 {
			source := strings.Repeat(fmt.Sprint(i), 100_000)
			ops := commitReplayed(t, tr, func(d *Draft) { must(t, d.AppendText("text", source[:40])) })
			if len(ops) != 1 {
				t.Fatalf("ops %v, want one append", ops)
			}
			if a := ops[0].(Append); aliases(a.Text, source) {
				t.Errorf("initial %q: the op text shares the caller's %d-byte source", initial, len(source))
			}
			if aliases(asObject(tr.Value())["text"].(string), source) {
				t.Errorf("initial %q: the new value shares the caller's %d-byte source", initial, len(source))
			}
		}
	}
}

// "appendText cost". Upstream's claim is O(chunk) per change, because a V8
// string append is a rope; a Go string is flat, so the new value is a copy of
// the whole string either way and a stream of appends allocates
// O(length²) — appendText cannot change that in Go. What it does skip is the
// same: prepare publishes the recorded text without comparing the strings or
// cutting the op text out of the new value, so preparing an appended chunk
// allocates nothing in proportion to it.
func TestAppendTextPrepareSkipsTheComparison(t *testing.T) {
	const chunk = 1 << 20
	tr := Track[any](map[string]any{"text": strings.Repeat("x", chunk)})
	text := strings.Repeat("y", chunk)
	c := mustBegin(t, tr)
	must(t, c.State().AppendText("text", text))
	var p *Prepared[any]
	bytes := allocatedBytes(func() { p = mustPrepare(t, c) })
	if len(p.Ops()) != 1 {
		t.Fatalf("ops %v, want one append", p.Ops())
	}
	if a, ok := p.Ops()[0].(Append); !ok || a.Text != text {
		t.Fatalf("op text is %d bytes, want the %d appended", len(a.Text), len(text))
	}
	if bytes >= chunk/2 {
		t.Errorf("Prepare allocated %d bytes for a %d-byte append: it re-derived the op from the strings", bytes, chunk)
	}
}
