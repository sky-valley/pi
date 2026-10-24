package delta

import (
	"fmt"
	"strings"
	"testing"
	"unsafe"
)

// delta-op-text-retention.test.ts (upstream f715a15c1, #9265): an append op's
// text was a slice of the whole new string, so every op kept the string it
// was cut from alive, and a delta chain built from N appends to a growing text
// held N copies. A Go substring pins its backing array exactly as a V8 sliced
// string does; upstream measures retained heap, the port asserts the cause
// directly: an op's text shares no bytes with the string it was cut from.

// aliases reports whether sub's bytes lie inside s's backing array.
func aliases(sub, s string) bool {
	if sub == "" || s == "" {
		return false
	}
	lo := uintptr(unsafe.Pointer(unsafe.StringData(s)))
	p := uintptr(unsafe.Pointer(unsafe.StringData(sub)))
	return p >= lo && p < lo+uintptr(len(s))
}

const retentionCommits = 30

var retentionBase = strings.Repeat("x", 100_000)

func retentionPiece(i int) string { return fmt.Sprintf("line %d of a long streamed text\n", i) }

// wantOwnedAppends fails for every append op in ops whose text aliases from.
func wantOwnedAppends(t *testing.T, ops []Op, from string) {
	t.Helper()
	appends := 0
	for _, op := range ops {
		a, ok := op.(Append)
		if !ok {
			continue
		}
		appends++
		if aliases(a.Text, from) {
			t.Errorf("append %q shares the backing array of the %d-byte string it was cut from", a.Text, len(from))
		}
	}
	if appends == 0 {
		t.Fatalf("no append op in %v", ops)
	}
}

func TestTrackerAppendOpsOwnTheirText(t *testing.T) {
	tr := Track[any](map[string]any{"text": retentionBase})
	for i := range retentionCommits {
		ops := commit(t, tr, func(d *Draft) { concat(t, d, "text", retentionPiece(i)) })
		if len(ops) != 1 {
			t.Fatalf("commit %d: ops %v, want one append", i, ops)
		}
		wantOwnedAppends(t, ops, asObject(tr.Value())["text"].(string))
	}
}

func TestTrackerRollingWindowAppendOwnsItsText(t *testing.T) {
	tr := Track[any](map[string]any{"text": "abcdef"})
	ops := commit(t, tr, func(d *Draft) { must(t, d.Set("text", "defghijklmnopqrstuvwxyz")) })
	wantOps(t, ops, `[["t", ["text"], 3], ["a", ["text"], "ghijklmnopqrstuvwxyz"]]`)
	wantOwnedAppends(t, ops, asObject(tr.Value())["text"].(string))
}

func TestDiffRevisionsAppendOpsOwnTheirText(t *testing.T) {
	text := retentionBase
	for i := range retentionCommits {
		next := text + retentionPiece(i)
		ops := DiffRevisions(map[string]any{"text": text}, map[string]any{"text": next})
		if len(ops) != 1 {
			t.Fatalf("diff %d: ops %v, want one append", i, ops)
		}
		wantOwnedAppends(t, ops, next)
		text = next
	}
}

func TestDiffRevisionsRollingWindowAppendOwnsItsText(t *testing.T) {
	after := "defghijklmnopqrstuvwxyz"
	ops := DiffRevisions(map[string]any{"text": "abcdef"}, map[string]any{"text": after})
	wantOps(t, ops, `[["t", ["text"], 3], ["a", ["text"], "ghijklmnopqrstuvwxyz"]]`)
	wantOwnedAppends(t, ops, after)
}

// The third upstream case — a replica kept current by applying the appends
// retains the text once — holds in Go by construction: Apply's "a" is a
// concatenation, which always allocates the result. What it asserts beyond
// that is the op texts above.
func TestReplicaOfAppendStreamMatchesTracker(t *testing.T) {
	tr := Track[any](map[string]any{"text": retentionBase})
	var replica any = map[string]any{"text": retentionBase}
	for i := range retentionCommits {
		ops := commit(t, tr, func(d *Draft) { concat(t, d, "text", retentionPiece(i)) })
		var err error
		if replica, err = ApplyImmutable(replica, ops); err != nil {
			t.Fatal(err)
		}
	}
	wantJSON(t, replica, tr.Value())
}
